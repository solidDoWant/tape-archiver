package main

import (
	"context"
	"fmt"
	"io"

	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
)

// cancelRun implements `tapectl cancel`: graceful Temporal cancellation of any
// in-progress run, paused or not. Never TerminateWorkflow, which would skip
// the workflow's deferred cleanup (LTFS unmount, ZFS hold release) and risk
// wedged mounts.
func cancelRun(ctx context.Context, args []string, out io.Writer) error {
	if err := requireNoArgs("cancel", args); err != nil {
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

	if err := temporalClient.CancelWorkflow(ctx, backupWorkflowID, ""); err != nil {
		return fmt.Errorf("cancel workflow %q: %w", backupWorkflowID, err)
	}

	_, err = fmt.Fprintf(out, "Cancellation requested for run %s.\n", backupWorkflowID)

	return err
}
