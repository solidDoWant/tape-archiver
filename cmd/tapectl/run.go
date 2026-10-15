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

// backupWorkflowID is a singleton on purpose: the backup model is serial (one
// data worker, one staging area — SPEC §4.2), so runs must be mutually exclusive.
const backupWorkflowID = backup.WorkflowID

type runOptions struct {
	configPath string
	dryRun     bool
}

// submitRun implements `tapectl run`. Config errors fail before any Temporal
// connection is attempted.
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

	run, err := runsubmit.Submit(ctx, temporalClient, cfg, options.dryRun)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintln(out, run.GetID())

	return err
}

func parseRunArgs(args []string) (runOptions, error) {
	flagSet := flag.NewFlagSet("run", flag.ContinueOnError)

	var options runOptions
	flagSet.StringVar(&options.configPath, "config", "", "path to the run-config JSON file (required)")
	flagSet.BoolVar(&options.dryRun, "dry-run", false, "override library devices to the mhvtl virtual library")

	if err := flagSet.Parse(args); err != nil {
		return runOptions{}, err
	}

	// The flag package stops at the first positional, so a stray argument would
	// silently drop later flags (e.g. losing --dry-run and submitting a real run).
	if flagSet.NArg() != 0 {
		return runOptions{}, fmt.Errorf("tapectl run takes no positional arguments, but got %q\n\n"+
			"Usage: tapectl run --config <file> [--dry-run]", flagSet.Arg(0))
	}

	return options, nil
}

// buildSubmission loads and validates the config and applies the dry-run
// override.
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

func translateSubmitError(err error) error {
	return runsubmit.TranslateSubmitError(err)
}
