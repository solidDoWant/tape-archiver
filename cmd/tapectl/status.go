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

// phaseUnavailable is printed when the phase query fails, e.g. no worker is
// polling.
const phaseUnavailable = "unavailable"

const phaseNone = "none"

func showStatus(ctx context.Context, args []string, out io.Writer) error {
	if err := requireNoArgs("status", args); err != nil {
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

	description, err := temporalClient.DescribeWorkflowExecution(ctx, backupWorkflowID, "")
	if err != nil {
		return fmt.Errorf("describe workflow %q: %w", backupWorkflowID, err)
	}

	status := description.GetWorkflowExecutionInfo().GetStatus()
	phase := queryLastCompletedPhase(ctx, temporalClient, backupWorkflowID)

	_, err = fmt.Fprintf(out,
		"Workflow:             %s\nStatus:               %s\nLast completed phase: %s\n",
		backupWorkflowID, formatStatus(status), phase)

	return err
}

// queryLastCompletedPhase returns a sentinel rather than an error on query
// failure so status can still report the execution state.
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

func formatStatus(status enumspb.WorkflowExecutionStatus) string {
	return status.String()
}
