// Command webdevseed submits a few sample dry-run backups against the local
// `make web-dev` stack so the web UI's History is not empty. Dev tooling only.
//
// The first WEBDEVSEED_FAIL_COUNT runs deliver their report to the fake
// receiver's reject endpoint so they fail terminally at Deliver. Deliver is the
// failure lever because its retry is bounded; earlier phases retry forever and
// would wedge the run "Running".
//
// Backup runs are a Temporal singleton (workflow ID "backup"), so runs are
// submitted serially. Seed configs set allowNonBlankTapes so repeat invocations
// can reuse the same small pool of mhvtl slots without cross-run state; these
// are disposable dev archives.
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
	defaultFailCount     = 1 // of defaultCount runs, how many are seeded to fail (see run)
	defaultSource        = "tape_test/archive@test-snap"
	defaultPerRunTimeout = 8 * time.Minute
	maxSubmitAttempts    = 8 // ~2 minutes of retrying a singleton conflict (at submitRetryWait apart) before giving up this iteration

	// Seed slots stay away from 0-2, which integration tests hardcode.
	slotPoolStart = 20
	slotPoolSize  = 3

	tapeCapacityBytes = 2_500_000_000_000 // nominal LTO-6 native capacity, matching other sample configs in this repo
	redundancyPercent = 10.0
)

// submitRetryWait is a var so tests can shrink it.
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

	failCount, err := envNonNegativeInt(getenv, "WEBDEVSEED_FAIL_COUNT", defaultFailCount)
	if err != nil {
		return err
	}

	if failCount > count {
		failCount = count
	}

	source := envOr(getenv, "WEBDEVSEED_SOURCE", defaultSource)

	// Unset leaves report delivery a no-op.
	webhookURL := getenv("WEBDEVSEED_WEBHOOK_URL")

	// The reject endpoint 4xx's the upload, failing the run terminally at Deliver.
	failWebhookURL := getenv("WEBDEVSEED_FAIL_WEBHOOK_URL")
	failCount = effectiveFailCount(failCount, failWebhookURL)

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

		// Failures go first so the most recent run (the dashboard's "last run")
		// is a success.
		expectFailure := i < failCount

		runWebhookURL := webhookURL
		if expectFailure {
			runWebhookURL = failWebhookURL
		}

		cfg := buildSeedConfig(source, recipient, identity, slot, runWebhookURL)
		if err := runsubmit.ApplyDryRun(cfg, getenv, os.Stdout); err != nil {
			return fmt.Errorf("apply dry-run override: %w", err)
		}

		kind := "sample dry-run"
		if expectFailure {
			kind = "sample dry-run seeded to FAIL (report delivery rejected)"
		}

		fmt.Printf("webdevseed: [%d/%d] submitting %s (source %s, slot %d)...\n", i+1, count, kind, source, slot)

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
			outcome := "finished with an error"
			if expectFailure {
				outcome = "failed as intended"
			}

			fmt.Printf("webdevseed: [%d/%d] run %s %s (visible in History): %v\n", i+1, count, submittedRun.GetRunID(), outcome, getErr)

			continue
		}

		if expectFailure {
			fmt.Printf("webdevseed: [%d/%d] run %s unexpectedly SUCCEEDED (was seeded to fail)\n", i+1, count, submittedRun.GetRunID())

			continue
		}

		fmt.Printf("webdevseed: [%d/%d] run %s completed\n", i+1, count, submittedRun.GetRunID())
	}

	return nil
}

// effectiveFailCount returns 0 (and logs why) when no reject webhook is
// configured, since there is then no clean way to make a run fail.
func effectiveFailCount(failCount int, failWebhookURL string) int {
	if failCount > 0 && failWebhookURL == "" {
		fmt.Println("webdevseed: WEBDEVSEED_FAIL_COUNT is set but WEBDEVSEED_FAIL_WEBHOOK_URL is not; seeding no failing runs")

		return 0
	}

	return failCount
}

// submitWithRetry submits cfg, retrying while the singleton backup workflow is
// still occupied by an earlier run.
func submitWithRetry(ctx context.Context, temporalClient runsubmit.TemporalClient, cfg *config.Config) (client.WorkflowRun, error) {
	var lastErr error

	for attempt := range maxSubmitAttempts {
		submittedRun, err := runsubmit.Submit(ctx, temporalClient, cfg, true)
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

// buildSeedConfig builds the config for one sample dry-run. Changer/Drives are
// placeholders that runsubmit.ApplyDryRun overwrites.
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

// generateKeypair shells out to `age-keygen -pq`; there is no Go-side
// post-quantum keypair generator.
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

func envOr(getenv func(string) string, name, fallback string) string {
	if value := getenv(name); value != "" {
		return value
	}

	return fallback
}

// envInt parses a positive integer env var, returning fallback when unset.
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

// envNonNegativeInt is envInt but also accepts 0.
func envNonNegativeInt(getenv func(string) string, name string, fallback int) (int, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", name, raw)
	}

	return value, nil
}
