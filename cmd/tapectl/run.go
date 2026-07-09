package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/runsubmit"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// backupWorkflowID is the fixed workflow ID every backup run submits under —
// an alias for backup.WorkflowID, the shared source of truth (also used by
// pkg/runsapi to list/describe runs via Temporal visibility) — kept as a
// local name since it is used pervasively throughout this package. It is a
// singleton on purpose: the backup model is serial (one data worker on one
// storage host, one disk staging area — SPEC §4.2), so all runs must be
// mutually exclusive. Combined with the conflict/reuse policies in
// pkg/runsubmit.StartOptions, a second run submitted while one is already
// running is refused, while a new run after the previous one closes starts
// normally.
const backupWorkflowID = backup.WorkflowID

// runOptions holds the parsed flags for the `run` subcommand.
type runOptions struct {
	configPath string
	dryRun     bool
}

// submitRun implements `tapectl run`. It prepares the submission entirely
// client-side (load, validate, dry-run override) so an invalid config or a
// missing Temporal address fails before any connection is attempted, then
// submits the backup workflow under the singleton ID (pkg/runsubmit.Submit —
// the same submission path pkg/runsapi's POST /api/runs uses) and prints it.
func submitRun(ctx context.Context, args []string, out io.Writer) error {
	options, err := parseRunArgs(args)
	if err != nil {
		return err
	}

	cfg, err := buildSubmission(options)
	if err != nil {
		return err
	}

	if err := requireTemporalAddress(getenv); err != nil {
		return err
	}

	temporalClient, shutdown, err := temporalclient.New(ctx, nil)
	if err != nil {
		return fmt.Errorf("connect to Temporal: %w", err)
	}
	defer shutdown()

	run, err := runsubmit.Submit(ctx, temporalClient, cfg)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintln(out, run.GetID())

	return err
}

// parseRunArgs parses the `run` subcommand flags.
func parseRunArgs(args []string) (runOptions, error) {
	flagSet := flag.NewFlagSet("run", flag.ContinueOnError)

	var options runOptions
	flagSet.StringVar(&options.configPath, "config", "", "path to the run-config JSON file (required)")
	flagSet.BoolVar(&options.dryRun, "dry-run", false, "override library devices to the mhvtl virtual library")

	if err := flagSet.Parse(args); err != nil {
		return runOptions{}, err
	}

	// Reject stray positional arguments. Go's flag package stops parsing at the
	// first positional, so a stray argument would silently drop any flags after
	// it (e.g. `--config prod.json backup --dry-run` loses --dry-run and submits
	// a real run). Fail fast naming the unexpected argument instead.
	if flagSet.NArg() != 0 {
		return runOptions{}, fmt.Errorf("tapectl run takes no positional arguments, but got %q\n\n"+
			"Usage: tapectl run --config <file> [--dry-run]", flagSet.Arg(0))
	}

	return options, nil
}

// buildSubmission performs every step that must happen before contacting
// Temporal: it loads and validates the config, then applies the dry-run
// device override (which re-validates the result itself — see
// pkg/runsubmit.ApplyDryRun).
func buildSubmission(options runOptions) (*config.Config, error) {
	if options.configPath == "" {
		return nil, fmt.Errorf("--config is required")
	}

	cfg, err := config.LoadFile(options.configPath)
	if err != nil {
		return nil, err
	}

	if options.dryRun {
		if err := applyDryRun(cfg, getenv); err != nil {
			return nil, err
		}
	}

	return cfg, nil
}

// translateSubmitError converts the error returned by ExecuteWorkflow into an
// operator-facing message. It is a thin wrapper over
// pkg/runsubmit.TranslateSubmitError — the same translation pkg/runsapi's
// POST /api/runs applies — kept as a package-local name since submitRun no
// longer needs it directly (runsubmit.Submit applies it internally) but it
// stays useful, and tested, as a standalone entry point.
func translateSubmitError(err error) error {
	return runsubmit.TranslateSubmitError(err)
}
