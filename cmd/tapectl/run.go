package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// runOptions holds the parsed flags for the `run` subcommand.
type runOptions struct {
	configPath string
	dryRun     bool
	id         string
}

// runRun implements `tapectl run`. It prepares the submission entirely
// client-side (load, validate, dry-run override, ID) so an invalid config or a
// missing Temporal address fails before any connection is attempted, then
// submits the backup workflow and prints its ID.
func runRun(ctx context.Context, args []string, out io.Writer) error {
	options, err := parseRunArgs(args)
	if err != nil {
		return err
	}

	cfg, workflowID, err := buildSubmission(options, time.Now())
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
		ID:        workflowID,
		TaskQueue: backup.TaskQueue,
	}, backup.WorkflowType, cfg)
	if err != nil {
		return fmt.Errorf("submit backup workflow: %w", err)
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
	flagSet.StringVar(&options.id, "id", "", "workflow ID to submit under (default: backup-<timestamp>)")

	if err := flagSet.Parse(args); err != nil {
		return runOptions{}, err
	}

	return options, nil
}

// buildSubmission performs every step that must happen before contacting
// Temporal: it loads and validates the config, applies the dry-run device
// override, re-validates the result, and resolves the workflow ID. now supplies
// the timestamp for a generated ID; it is a parameter so tests are
// deterministic.
func buildSubmission(options runOptions, now time.Time) (*config.Config, string, error) {
	if options.configPath == "" {
		return nil, "", fmt.Errorf("--config is required")
	}

	cfg, err := config.LoadFile(options.configPath)
	if err != nil {
		return nil, "", err
	}

	if options.dryRun {
		applyDryRun(cfg, getenv)

		// The override changes the device set, so the result must satisfy the
		// same invariants (e.g. copies <= drives) as the original config.
		if err := cfg.Validate(); err != nil {
			return nil, "", fmt.Errorf("invalid config after dry-run override: %w", err)
		}
	}

	workflowID := options.id
	if workflowID == "" {
		workflowID = generateWorkflowID(now)
	}

	return cfg, workflowID, nil
}

// generateWorkflowID builds the default workflow ID, "backup-<timestamp>", from
// a UTC timestamp so IDs sort chronologically and are filesystem-safe.
func generateWorkflowID(now time.Time) string {
	return "backup-" + now.UTC().Format("20060102T150405Z")
}
