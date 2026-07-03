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
// OperatorEjectClearedSignal to a run paused in the Eject phase because the
// import/export station filled (SPEC §4.3 phase 8). The operator runs it after
// removing the exported tapes and clearing the station; the run then re-reads the
// changer inventory and exports the remaining tapes into the freed slots. It is
// the fallback for libraries that do not report the import/export access bit — one
// that does resumes automatically without this signal.
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

	if err := temporalClient.SignalWorkflow(ctx, workflowID, "", backup.OperatorEjectClearedSignal, nil); err != nil {
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
