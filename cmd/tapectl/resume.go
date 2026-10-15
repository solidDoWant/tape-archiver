package main

import (
	"context"
	"fmt"
	"io"

	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// resumeRun implements `tapectl resume`, resuming a run paused on a full
// import/export station or a failed Load/Write (SPEC §4.3).
func resumeRun(ctx context.Context, args []string, out io.Writer) error {
	if err := requireNoArgs("resume", args); err != nil {
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

	if err := temporalClient.SignalWorkflow(ctx, backupWorkflowID, "", backup.OperatorResumeSignal, nil); err != nil {
		return fmt.Errorf("signal workflow %q to resume: %w", backupWorkflowID, err)
	}

	_, err = fmt.Fprintf(out, "Resume signal sent to run %s.\n", backupWorkflowID)

	return err
}
