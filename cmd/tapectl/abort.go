package main

import (
	"context"
	"fmt"
	"io"

	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// abortRun implements `tapectl abort`, ending a run paused on a Load/Write
// failure in a reported state with no further tapes written (SPEC §4.3).
func abortRun(ctx context.Context, args []string, out io.Writer) error {
	if err := requireNoArgs("abort", args); err != nil {
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

	if err := temporalClient.SignalWorkflow(ctx, backupWorkflowID, "", backup.OperatorAbortSignal, nil); err != nil {
		return fmt.Errorf("signal workflow %q to abort: %w", backupWorkflowID, err)
	}

	_, err = fmt.Fprintf(out, "Abort signal sent to run %s.\n", backupWorkflowID)

	return err
}
