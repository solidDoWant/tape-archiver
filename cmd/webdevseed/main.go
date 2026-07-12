// Command webdevseed submits a handful of sample dry-run backups against the
// local dev stack `make web-dev` brings up (issue #265), so a developer
// opening the web UI immediately has something to look at in History rather
// than an empty list. It is dev tooling only — never built into a shipped
// image or run in CI (see the Non-Goals in issue #265).
//
// It reuses the one shared submit path (pkg/runsubmit.Submit/.ApplyDryRun —
// the same functions `tapectl run --dry-run` and `POST /api/runs` call) and a
// real `age-keygen -pq` post-quantum keypair (the same pattern
// e2e/harness_test.go's generateTestKeypair and
// workflows/backup/e2e_integration_test.go use), submitting a
// config.Config sourced from the ZFS snapshot scripts/zpool-up.sh creates.
//
// Backup runs are a Temporal singleton (SPEC §4.2 — workflow ID always
// "backup"), so seeding cannot submit several at once: this loops
// sequentially, submitting one, blocking on its completion via
// client.WorkflowRun.Get, then submitting the next. scripts/web-dev-up.sh
// runs this in the background (not before starting cmd/web in the
// foreground) precisely because of that serial cost — a developer should not
// wait several minutes of real mhvtl writes before `make web-dev` hands back
// a prompt; History fills in progressively instead.
//
// Seed configs set library.allowNonBlankTapes so repeat invocations (`make
// web-dev` run again against an already-up stack, per issue #265 AC3) can
// keep reusing the same small pool of mhvtl storage slots indefinitely
// without needing any cross-invocation state to track which slots are
// still blank — these are disposable dev archives, not real backups, so
// reclaiming them deliberately (the same opt-out a real operator can use,
// CLAUDE.md's "Hardware and Safety") is the simplest correct choice.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/runsubmit"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
)

const (
	defaultCount         = 3
	defaultSource        = "tape_test/archive@test-snap"
	defaultPerRunTimeout = 8 * time.Minute
	maxSubmitAttempts    = 8 // ~2 minutes of retrying a singleton conflict (at submitRetryWait apart) before giving up this iteration

	// slotPoolStart/slotPoolSize pick a small, fixed range of mhvtl storage
	// slots for seed runs, deliberately away from 0-2, which existing
	// integration tests (workflows/backup/*_integration_test.go) hardcode —
	// see the package doc comment for why reuse across invocations is safe
	// (allowNonBlankTapes) rather than needing to avoid collisions here too.
	slotPoolStart = 20
	slotPoolSize  = 3

	tapeCapacityBytes = 2_500_000_000_000 // nominal LTO-6 native capacity, matching other sample configs in this repo
	sliceSizeBytes    = 1 << 20           // small slices: keeps PAR2 generation fast against the zpool-up.sh 8M sample payload
	redundancyPercent = 10.0
)

// submitRetryWait is how long submitWithRetry waits between attempts while
// the singleton backup workflow is occupied. A package-level var (not a
// const) so tests can shrink it and exercise the give-up-after-maxSubmitAttempts
// path without a slow real-time test.
var submitRetryWait = 15 * time.Second

func main() {
	if err := run(context.Background(), os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "webdevseed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string) error {
	count, err := envInt(getenv, "WEBDEVSEED_COUNT", defaultCount)
	if err != nil {
		return err
	}

	source := envOr(getenv, "WEBDEVSEED_SOURCE", defaultSource)

	// WEBDEVSEED_WEBHOOK_URL points seeded runs' report delivery at the local
	// fake Discord receiver (cmd/webdevdiscord, wired by scripts/web-dev-up.sh)
	// so the web UI's "Discord report ↗" deep-link renders locally. Unset (any
	// context other than web-dev) leaves delivery a no-op — see buildSeedConfig.
	webhookURL := getenv("WEBDEVSEED_WEBHOOK_URL")

	identity, recipient, err := generateKeypair(ctx)
	if err != nil {
		return fmt.Errorf("generate sample age keypair: %w", err)
	}

	temporalClient, shutdown, err := temporalclient.New(ctx, nil)
	if err != nil {
		return fmt.Errorf("connect to Temporal: %w", err)
	}
	defer shutdown()

	for i := range count {
		slot := slotPoolStart + i%slotPoolSize

		cfg := buildSeedConfig(source, recipient, identity, slot, webhookURL)
		if err := runsubmit.ApplyDryRun(cfg, getenv, os.Stdout); err != nil {
			return fmt.Errorf("apply dry-run override: %w", err)
		}

		fmt.Printf("webdevseed: [%d/%d] submitting sample dry-run (source %s, slot %d)...\n", i+1, count, source, slot)

		submittedRun, err := submitWithRetry(ctx, temporalClient, cfg)
		if err != nil {
			fmt.Printf("webdevseed: [%d/%d] skipped: %v\n", i+1, count, err)

			continue
		}

		fmt.Printf("webdevseed: [%d/%d] run %s submitted, waiting for completion...\n", i+1, count, submittedRun.GetRunID())

		waitCtx, cancel := context.WithTimeout(ctx, defaultPerRunTimeout)
		getErr := submittedRun.Get(waitCtx, nil)

		cancel()

		if getErr != nil {
			fmt.Printf("webdevseed: [%d/%d] run %s finished with an error (still visible in History): %v\n", i+1, count, submittedRun.GetRunID(), getErr)

			continue
		}

		fmt.Printf("webdevseed: [%d/%d] run %s completed\n", i+1, count, submittedRun.GetRunID())
	}

	return nil
}

// submitWithRetry submits cfg, retrying while the singleton backup workflow
// is occupied by a still-running run (a real WorkflowExecutionAlreadyStarted
// conflict — the same one pkg/runsapi maps to 409) rather than failing
// immediately, since a previous `make web-dev` invocation's own seeding (or a
// developer's own ad hoc submission through the browser) may still be
// in flight. Gives up after maxSubmitAttempts and returns the last error.
func submitWithRetry(ctx context.Context, temporalClient runsubmit.TemporalClient, cfg *config.Config) (client.WorkflowRun, error) {
	var lastErr error

	for attempt := range maxSubmitAttempts {
		submittedRun, err := runsubmit.Submit(ctx, temporalClient, cfg)
		if err == nil {
			return submittedRun, nil
		}

		lastErr = err

		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if !errors.As(err, &alreadyStarted) {
			return nil, err
		}

		if attempt < maxSubmitAttempts-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(submitRetryWait):
			}
		}
	}

	return nil, fmt.Errorf("a backup run stayed active through %d retries: %w", maxSubmitAttempts, lastErr)
}

// buildSeedConfig builds a config.Config for one sample dry-run: a single ZFS
// source, one copy, small redundancy slicing, and the given delivery webhook.
// webhookURL is empty in most contexts (an empty WebhookURL is documented
// pkg/webhook no-op behavior — see pkg/webhook/webhook.go — so seeding makes no
// network call); `make web-dev` points it at the local fake Discord receiver
// (cmd/webdevdiscord) so a seeded run actually delivers its report and the web
// UI's "Discord report ↗" deep-link renders locally. Library.Changer/Drives are
// placeholders overwritten unconditionally by runsubmit.ApplyDryRun; only
// BlankSlots and AllowNonBlankTapes matter here.
func buildSeedConfig(source, recipient, identity string, slot int, webhookURL string) *config.Config {
	return &config.Config{
		Sources: []config.Source{
			{ZFSPath: &config.ZFSPathSource{Name: source}},
		},
		Copies: 1,
		Library: config.Library{
			Changer:            "placeholder-overwritten-by-dry-run",
			Drives:             []string{"placeholder-overwritten-by-dry-run"},
			BlankSlots:         []int{slot},
			TapeCapacityBytes:  tapeCapacityBytes,
			AllowNonBlankTapes: true,
		},
		Redundancy: config.Redundancy{
			TargetPercentage: ptrFloat(redundancyPercent),
			SliceSizeBytes:   sliceSizeBytes,
		},
		Encryption: config.Encryption{
			Recipients: []string{recipient},
			Identity:   identity,
		},
		Delivery: config.Delivery{
			WebhookURL: webhookURL,
		},
	}
}

// generateKeypair shells out to `age-keygen -pq` for a real post-quantum
// identity/recipient pair, the same way e2e/harness_test.go's
// generateTestKeypair and workflows/backup/e2e_integration_test.go's do —
// there is no Go-side keypair generator, only a from-identity recipient
// derivation (pkg/agewrap.RecipientFromIdentity), which needs a keypair to
// derive from in the first place.
func generateKeypair(ctx context.Context) (identity, recipient string, err error) {
	dir, err := os.MkdirTemp("", "webdevseed-age-*")
	if err != nil {
		return "", "", fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	path := filepath.Join(dir, "identity.txt")
	if err := exec.CommandContext(ctx, "age-keygen", "-pq", "-o", path).Run(); err != nil {
		return "", "", fmt.Errorf("age-keygen: %w", err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read generated identity: %w", err)
	}

	const marker = "# public key: "

	for _, line := range strings.Split(string(contents), "\n") {
		if after, found := strings.CutPrefix(line, marker); found {
			return string(contents), strings.TrimSpace(after), nil
		}
	}

	return "", "", fmt.Errorf("recipient not found in age-keygen output")
}

func ptrFloat(v float64) *float64 { return &v }

// envOr reads the named environment variable, returning fallback when unset
// or empty.
func envOr(getenv func(string) string, name, fallback string) string {
	if value := getenv(name); value != "" {
		return value
	}

	return fallback
}

// envInt reads the named environment variable as an integer, returning
// fallback when unset or empty, and an error when set but not a valid
// positive integer.
func envInt(getenv func(string) string, name string, fallback int) (int, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", name, raw)
	}

	return value, nil
}
