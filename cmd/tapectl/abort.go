package main

import (
	"context"
	"fmt"
	"io"

	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// abortRun implements `tapectl abort`. It sends the OperatorAbortSignal to the
// backup run paused because a Load or Write failed for one drive-set (SPEC §4.3):
// rather than swapping in fresh blanks and resuming, the operator ends the run in a
// defined, reported state with no further tapes written. Runs are a singleton
// (backupWorkflowID, SPEC §4.2), so it takes no arguments.
func abortRun(ctx context.Context, args []string, out io.Writer) error {
	if err := requireNoArgs("abort", args); err != nil {
		return err
	}

	workflowID := backupWorkflowID

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
