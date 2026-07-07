package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// backupWorkflowID is the fixed workflow ID every backup run submits under. It
// is a singleton on purpose: the backup model is serial (one data worker on one
// storage host, one disk staging area — SPEC §4.2), so all runs must be
// mutually exclusive. Combined with the conflict/reuse policies in submitRun, a
// second run submitted while one is already running is refused, while a new run
// after the previous one closes starts normally.
const backupWorkflowID = "backup"

// runOptions holds the parsed flags for the `run` subcommand.
type runOptions struct {
	configPath string
	dryRun     bool
}

// submitRun implements `tapectl run`. It prepares the submission entirely
// client-side (load, validate, dry-run override) so an invalid config or a
// missing Temporal address fails before any connection is attempted, then
// submits the backup workflow under the singleton ID and prints it.
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

	run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        backupWorkflowID,
		TaskQueue: backup.TaskQueue,
		// Refuse to start a second run while one is already running, but allow a
		// fresh run once the previous one has closed. WorkflowExecutionError-
		// WhenAlreadyStarted makes ExecuteWorkflow return the conflict as an
		// error rather than silently handing back the running run.
		WorkflowIDConflictPolicy:                 enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, backup.WorkflowType, cfg)
	if err != nil {
		return translateSubmitError(err)
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
// Temporal: it loads and validates the config, applies the dry-run device
// override, and re-validates the result.
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

		// The override changes the device set, so the result must satisfy the
		// same invariants (e.g. copies <= drives) as the original config.
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("invalid config after dry-run override: %w", err)
		}
	}

	return cfg, nil
}

// translateSubmitError converts the error returned by ExecuteWorkflow into an
// operator-facing message. A conflict with an already-running backup (the
// singleton guard firing) is reported as an actionable message naming the
// in-progress run rather than an opaque Temporal error; every other error is
// wrapped verbatim.
func translateSubmitError(err error) error {
	var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &alreadyStarted) {
		return fmt.Errorf("a backup run is already in progress (workflow ID %q, run ID %s); "+
			"backup runs are mutually exclusive — wait for it to finish or inspect it with `tapectl status`",
			backupWorkflowID, alreadyStarted.RunId)
	}

	return fmt.Errorf("submit backup workflow: %w", err)
}
