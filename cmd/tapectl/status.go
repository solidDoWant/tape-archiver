package main

import (
	"context"
	"fmt"
	"io"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// phaseUnavailable is printed for the last completed phase when the workflow
// does not answer the phase query — e.g. no worker is currently polling, or the
// workflow predates the query handler. The execution status is still reported.
const phaseUnavailable = "unavailable"

// phaseNone is printed when the workflow answers the phase query but no phase
// has completed yet.
const phaseNone = "none"

// showStatus implements `tapectl status`. It prints the backup run's current
// execution status and the name of its last completed phase. Runs are a singleton
// (backupWorkflowID, SPEC §4.2), so it takes no arguments.
func showStatus(ctx context.Context, args []string, out io.Writer) error {
	if err := requireNoArgs("status", args); err != nil {
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

	description, err := temporalClient.DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		return fmt.Errorf("describe workflow %q: %w", workflowID, err)
	}

	status := description.GetWorkflowExecutionInfo().GetStatus()
	phase := queryLastCompletedPhase(ctx, temporalClient, workflowID)

	_, err = fmt.Fprintf(out,
		"Workflow:             %s\nStatus:               %s\nLast completed phase: %s\n",
		workflowID, formatStatus(status), phase)

	return err
}

// queryLastCompletedPhase asks the workflow for its last completed phase via the
// agreed query. A query failure is not fatal — the workflow may have no worker
// polling or may predate the handler — so it returns a sentinel rather than an
// error, letting status still report the execution state.
func queryLastCompletedPhase(ctx context.Context, temporalClient client.Client, workflowID string) string {
	response, err := temporalClient.QueryWorkflow(ctx, workflowID, "", backup.LastCompletedPhaseQuery)
	if err != nil {
		return phaseUnavailable
	}

	var phase string
	if err := response.Get(&phase); err != nil {
		return phaseUnavailable
	}

	if phase == "" {
		return phaseNone
	}

	return phase
}

// formatStatus renders a Temporal execution status enum as a human-readable
// label. The SDK enum's String already yields a friendly CamelCase form
// (e.g. "Running", "ContinuedAsNew").
func formatStatus(status enumspb.WorkflowExecutionStatus) string {
	return status.String()
}
