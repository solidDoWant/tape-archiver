package main

import (
	"context"
	"fmt"
	"io"

	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
)

// cancelRun implements `tapectl cancel`. It requests graceful Temporal
// cancellation of the in-progress backup run (client.CancelWorkflow), the same
// action the web UI's Cancel run button takes (POST /api/runs/{runID}/cancel).
// Unlike `tapectl abort`, which sends OperatorAbortSignal and only ends a run
// already paused on a Load/Write failure, cancel stops any in-progress run —
// paused or not: Temporal delivers cancellation into the workflow, whose
// deferred cleanup runs on a workflow.NewDisconnectedContext (SPEC §10), so the
// run tears down its LTFS mounts, releases its ZFS hold, posts the
// failure/cancellation alert, and closes as Canceled rather than being killed
// mid-flight. It is the graceful path the workflow was built for — never
// TerminateWorkflow, which would skip that cleanup and risk wedged mounts.
//
// Like resume/abort it signals the run unconditionally, with no pre-check that
// the run is still running (acceptable for a human operator acting deliberately
// on the one run): CancelWorkflow's own error surfaces if there is no in-progress
// execution to cancel. Runs are a singleton (backupWorkflowID, SPEC §4.2), so it
// takes no arguments and an empty run ID targets the latest execution.
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
