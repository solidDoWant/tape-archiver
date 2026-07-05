package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// resumeRun implements `tapectl resume <workflow-id>`. It sends the
// OperatorResumeSignal to a paused run, resuming either operator-in-the-loop pause
// (SPEC §4.3): the Eject phase paused because the import/export station filled, or
// the tape path paused because a Load or Write failed for one drive-set. The
// operator runs it after clearing the blocking condition — removing the exported
// tapes, or swapping the suspect tapes for fresh blanks in the same slots. On an
// Eject pause it is the fallback for libraries that do not report the import/export
// access bit (one that does resumes automatically); on a write-path pause it is
// the sole resume path.
func resumeRun(ctx context.Context, args []string, out io.Writer) error {
	workflowID, err := parseResumeArgs(args)
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

	if err := temporalClient.SignalWorkflow(ctx, workflowID, "", backup.OperatorResumeSignal, nil); err != nil {
		return fmt.Errorf("signal workflow %q to resume: %w", workflowID, err)
	}

	_, err = fmt.Fprintf(out, "Resume signal sent to run %s.\n", workflowID)

	return err
}

// parseResumeArgs parses the `resume` subcommand and returns the workflow ID.
func parseResumeArgs(args []string) (string, error) {
	flagSet := flag.NewFlagSet("resume", flag.ContinueOnError)
	if err := flagSet.Parse(args); err != nil {
		return "", err
	}

	if flagSet.NArg() != 1 {
		return "", fmt.Errorf("exactly one workflow ID is required\n\nUsage: tapectl resume <workflow-id>")
	}

	return flagSet.Arg(0), nil
}
