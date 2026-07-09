// Package runsapi implements the JSON HTTP API cmd/web serves under /api/runs:
// listing past/current executions of the singleton backup workflow and
// fetching detail for one execution. Both endpoints are read-only views over
// Temporal visibility and the workflow's own query handlers — there is no
// UI-owned store (SPEC §4.2; docs/web-ui-design.md §2, §3).
package runsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"golang.org/x/sync/errgroup"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// requestTimeout bounds every Temporal RPC a handler makes. r.Context() alone
// is not enough: the net/http server's ReadTimeout/WriteTimeout govern the
// client connection, not an in-flight handler's context, so without an
// explicit deadline a stalled Temporal RPC would block the handler goroutine
// (and the underlying gRPC call) indefinitely.
const requestTimeout = 10 * time.Second

// listPageSize bounds a single GET /api/runs response. Backup runs are a
// serial singleton (one at a time, one storage host), so even generous
// Temporal visibility retention holds at most a few hundred executions in
// practice; a single unpaginated page comfortably covers that. Should this
// ever need real pagination (e.g. very long visibility retention), the
// endpoint would need a page-token query parameter — out of scope for this
// slice of the API (docs/web-ui-design.md §8, item 2).
const listPageSize = 1000

// TemporalClient is the subset of client.Client the runs API needs, so
// handlers are unit-testable against a fake without a real Temporal
// connection. go.temporal.io/sdk/client.Client satisfies it.
type TemporalClient interface {
	ListWorkflow(ctx context.Context, request *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error)
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	QueryWorkflow(ctx context.Context, workflowID string, runID string, queryType string, args ...interface{}) (converter.EncodedValue, error)
}

// Compile-time assertion that the real Temporal SDK client satisfies
// TemporalClient.
var _ TemporalClient = client.Client(nil)

// RunSummary is one execution of the backup workflow, as returned in the GET
// /api/runs list.
type RunSummary struct {
	WorkflowID string     `json:"workflowId"`
	RunID      string     `json:"runId"`
	Status     string     `json:"status"`
	StartTime  time.Time  `json:"startTime"`
	CloseTime  *time.Time `json:"closeTime,omitempty"`
}

// RunsResponse is the GET /api/runs response body.
type RunsResponse struct {
	Runs []RunSummary `json:"runs"`
}

// RunDetail is one execution's detail, as returned by GET /api/runs/{runID}.
type RunDetail struct {
	RunSummary

	// LastCompletedPhase is the name of the most recently completed workflow
	// phase (backup.LastCompletedPhaseQuery), or "" when no phase has
	// completed yet or the query could not be answered (e.g. no worker
	// currently polling) — the latter is logged server-side rather than
	// failing the request, since the execution status/timing above is still
	// valid and useful on its own.
	LastCompletedPhase string `json:"lastCompletedPhase"`
}

// errorResponse is the JSON body written for a non-2xx response.
type errorResponse struct {
	Error string `json:"error"`
}

// New builds the /api/runs HTTP handler. temporalClient must be non-nil.
func New(temporalClient TemporalClient) http.Handler {
	h := &handler{temporalClient: temporalClient}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs", h.listRuns)
	mux.HandleFunc("GET /api/runs/{runID}", h.getRun)

	return mux
}

type handler struct {
	temporalClient TemporalClient
}

// listRuns implements GET /api/runs: every execution of the singleton backup
// workflow (backup.WorkflowID), newest first, via Temporal visibility.
//
// Sorting is done here in Go, not via an "ORDER BY" clause in the visibility
// query: Temporal's standard (SQLite/SQL) visibility store — used by the dev
// server and any self-hosted deployment without Elasticsearch — rejects
// "ORDER BY" as an unsupported query operation, while Elasticsearch-backed
// advanced visibility accepts it. Sorting client-side works against both, so
// it is the only portable choice here.
func (h *handler) listRuns(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	response, err := h.temporalClient.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Query:    fmt.Sprintf("WorkflowId = %q", backup.WorkflowID),
		PageSize: listPageSize,
	})
	if err != nil {
		writeError(w, statusForTemporalError(err), fmt.Errorf("list workflow executions: %w", err))

		return
	}

	executions := response.GetExecutions()
	runs := make([]RunSummary, 0, len(executions))

	for _, execution := range executions {
		runs = append(runs, toRunSummary(execution))
	}

	sort.Slice(runs, func(i, j int) bool { return runs[i].StartTime.After(runs[j].StartTime) })

	writeJSON(w, http.StatusOK, RunsResponse{Runs: runs})
}

// getRun implements GET /api/runs/{runID}: detail for one execution of the
// singleton backup workflow, identified by its Temporal run ID (the
// workflow ID is always backup.WorkflowID; the run ID disambiguates which
// execution/attempt of it).
func (h *handler) getRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	runID := r.PathValue("runID")

	if runID == "" {
		writeError(w, http.StatusBadRequest, errors.New("runID is required"))

		return
	}

	// DescribeWorkflowExecution and the last-completed-phase query are
	// independent RPCs — neither needs the other's result, only
	// backup.WorkflowID and runID — so they run concurrently rather than
	// paying the sum of both RPCs' latency.
	group, groupCtx := errgroup.WithContext(ctx)

	var description *workflowservice.DescribeWorkflowExecutionResponse

	group.Go(func() error {
		var err error

		description, err = h.temporalClient.DescribeWorkflowExecution(groupCtx, backup.WorkflowID, runID)

		return err
	})

	var lastCompletedPhase string

	group.Go(func() error {
		lastCompletedPhase = queryLastCompletedPhase(groupCtx, h.temporalClient, runID)

		return nil
	})

	if err := group.Wait(); err != nil {
		switch status := statusForTemporalError(err); status {
		case http.StatusNotFound:
			writeError(w, status, fmt.Errorf("run %q not found", runID))
		case http.StatusBadRequest:
			// A syntactically invalid run ID (Temporal run IDs are UUIDs)
			// comes back as InvalidArgument rather than NotFound — e.g.
			// "Invalid RunId." — so it is a 400 (bad client input), not a
			// 404 (well-formed ID, no such run) or a 502 (this is not an
			// upstream failure).
			writeError(w, status, fmt.Errorf("invalid run ID %q: %w", runID, err))
		default:
			writeError(w, status, fmt.Errorf("describe workflow execution: %w", err))
		}

		return
	}

	detail := RunDetail{
		RunSummary:         toRunSummary(description.GetWorkflowExecutionInfo()),
		LastCompletedPhase: lastCompletedPhase,
	}

	writeJSON(w, http.StatusOK, detail)
}

// queryLastCompletedPhase asks the workflow for its last completed phase via
// the agreed query (workflows/backup/contract.go). A query failure (e.g. no
// worker currently polling) is not fatal to the request — the execution
// status/timing already fetched via Describe is still valid — so it is
// logged and reported as "" rather than failing GET /api/runs/{runID}.
func queryLastCompletedPhase(ctx context.Context, temporalClient TemporalClient, runID string) string {
	response, err := temporalClient.QueryWorkflow(ctx, backup.WorkflowID, runID, backup.LastCompletedPhaseQuery)
	if err != nil {
		slog.WarnContext(ctx, "runsapi: last completed phase query failed", "run_id", runID, "error", err)

		return ""
	}

	var phase string
	if err := response.Get(&phase); err != nil {
		slog.WarnContext(ctx, "runsapi: decode last completed phase query result failed", "run_id", runID, "error", err)

		return ""
	}

	return phase
}

// statusForTemporalError classifies a Temporal RPC error into the HTTP
// status that best represents it, shared by every runsapi handler so future
// endpoints (submit/dry-run, resume/abort — docs/web-ui-design.md §8) extend
// one consistent mapping instead of each hand-rolling its own: a missing
// resource is serviceerror.NotFound (404), a malformed client-supplied
// identifier/argument is serviceerror.InvalidArgument (400), and anything
// else is 502 — this handler is a proxy over Temporal, so an unclassified
// failure is upstream, not the client's fault.
func statusForTemporalError(err error) int {
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return http.StatusNotFound
	}

	var invalidArgument *serviceerror.InvalidArgument
	if errors.As(err, &invalidArgument) {
		return http.StatusBadRequest
	}

	return http.StatusBadGateway
}

// toRunSummary maps a Temporal visibility record to the API's RunSummary
// shape.
func toRunSummary(execution *workflowpb.WorkflowExecutionInfo) RunSummary {
	summary := RunSummary{
		WorkflowID: execution.GetExecution().GetWorkflowId(),
		RunID:      execution.GetExecution().GetRunId(),
		Status:     execution.GetStatus().String(),
		StartTime:  execution.GetStartTime().AsTime(),
	}

	if closeTime := execution.GetCloseTime(); closeTime != nil {
		t := closeTime.AsTime()
		summary.CloseTime = &t
	}

	return summary
}

// writeJSON encodes body as the JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("runsapi: encode JSON response failed", "error", err)
	}
}

// writeError writes err as a JSON error response with the given status code.
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorResponse{Error: err.Error()})
}
