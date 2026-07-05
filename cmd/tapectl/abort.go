package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// abortRun implements `tapectl abort <workflow-id>`. It sends the
// OperatorAbortSignal to a run paused because a Load or Write failed for one
// drive-set (SPEC §4.3): rather than swapping in fresh blanks and resuming, the
// operator ends the run in a defined, reported state with no further tapes
// written.
func abortRun(ctx context.Context, args []string, out io.Writer) error {
	workflowID, err := parseAbortArgs(args)
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

	if err := temporalClient.SignalWorkflow(ctx, workflowID, "", backup.OperatorAbortSignal, nil); err != nil {
		return fmt.Errorf("signal workflow %q to abort: %w", workflowID, err)
	}

	_, err = fmt.Fprintf(out, "Abort signal sent to run %s.\n", workflowID)

	return err
}

// parseAbortArgs parses the `abort` subcommand and returns the workflow ID.
func parseAbortArgs(args []string) (string, error) {
	flagSet := flag.NewFlagSet("abort", flag.ContinueOnError)
	if err := flagSet.Parse(args); err != nil {
		return "", err
	}

	if flagSet.NArg() != 1 {
		return "", fmt.Errorf("exactly one workflow ID is required\n\nUsage: tapectl abort <workflow-id>")
	}

	return flagSet.Arg(0), nil
}
