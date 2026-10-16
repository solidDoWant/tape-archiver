package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"

	"github.com/solidDoWant/tape-archiver/pkg/runsapi"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// phaseUnavailable is printed when neither the live query nor the workflow
// history yields a phase.
const phaseUnavailable = "unavailable"

const phaseNone = "none"

// statusQueryTimeout bounds the live phase query: until the server notices no
// worker is polling, such a query waits out its deadline, and history answers
// without one.
const statusQueryTimeout = 5 * time.Second

// statusClient is the slice of the Temporal client status needs.
type statusClient interface {
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	QueryWorkflow(ctx context.Context, workflowID string, runID string, queryType string, args ...interface{}) (converter.EncodedValue, error)
	runsapi.HistoryReader
}

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

	return writeStatus(ctx, temporalClient, out)
}

// writeStatus prints the run's status. The last completed phase prefers the
// live query but falls back to workflow history, which needs no polling
// control worker (KEDA scales it to zero during long data-queue activities,
// issue #430).
func writeStatus(ctx context.Context, temporalClient statusClient, out io.Writer) error {
	description, err := temporalClient.DescribeWorkflowExecution(ctx, backupWorkflowID, "")
	if err != nil {
		return fmt.Errorf("describe workflow %q: %w", backupWorkflowID, err)
	}

	info := description.GetWorkflowExecutionInfo()
	runID := info.GetExecution().GetRunId()

	lastCompleted, queryErr := queryLastCompletedPhase(ctx, temporalClient, runID)
	current := phaseUnavailable

	if phases, historyErr := runsapi.DeriveRunPhases(ctx, temporalClient, runID); historyErr == nil {
		if queryErr != nil {
			lastCompleted, queryErr = phases.LastCompleted, nil
		}

		current = formatActivePhases(phases.Active)
	}

	switch {
	case queryErr != nil:
		lastCompleted = phaseUnavailable
	case lastCompleted == "":
		lastCompleted = phaseNone
	}

	_, err = fmt.Fprintf(out,
		"Workflow:             %s\nStatus:               %s\nLast completed phase: %s\nCurrent phase:        %s\n",
		backupWorkflowID, formatStatus(info.GetStatus()), lastCompleted, current)

	return err
}

func queryLastCompletedPhase(ctx context.Context, temporalClient statusClient, runID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, statusQueryTimeout)
	defer cancel()

	response, err := temporalClient.QueryWorkflow(ctx, backupWorkflowID, runID, backup.LastCompletedPhaseQuery)
	if err != nil {
		return "", err
	}

	var phase string
	if err := response.Get(&phase); err != nil {
		return "", err
	}

	return phase, nil
}

// formatActivePhases renders e.g. "Prepare (running since 2026-01-01T00:00:00Z)".
func formatActivePhases(active []runsapi.PhaseInfo) string {
	if len(active) == 0 {
		return phaseNone
	}

	parts := make([]string, 0, len(active))

	for _, phase := range active {
		if phase.StartTime == nil {
			parts = append(parts, phase.Name)

			continue
		}

		parts = append(parts, fmt.Sprintf("%s (running since %s)", phase.Name, phase.StartTime.UTC().Format(time.RFC3339)))
	}

	return strings.Join(parts, ", ")
}

func formatStatus(status enumspb.WorkflowExecutionStatus) string {
	return status.String()
}
