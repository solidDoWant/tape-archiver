// Package runsapi implements the JSON HTTP API cmd/web serves under /api/runs:
// listing past/current executions of the singleton backup workflow, fetching
// detail for one execution, and submitting a new run (optionally as a
// dry-run against the mhvtl virtual library). Submission reuses
// pkg/runsubmit — the same validation, dry-run override, and singleton
// conflict handling as `tapectl run` — so the CLI and this API can never
// drift on what a run submission means (docs/web-ui-design.md §2, §3, §8
// item 3). The two GET endpoints are read-only views over Temporal
// visibility and the workflow's own query handlers — there is no UI-owned
// store (SPEC §4.2).
package runsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"time"

	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"golang.org/x/sync/errgroup"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/runsubmit"
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

// maxSubmitBodyBytes bounds a POST /api/runs request body. Run configs are
// small JSON documents (schemas/run-config.schema.json) — even a config
// naming many sources stays well under this — so a generous fixed clamp is
// enough to reject an obviously abusive/mistaken upload before it is read
// into memory, without needing real streaming or size negotiation.
const maxSubmitBodyBytes = 4 << 20 // 4 MiB

// TemporalClient is the subset of client.Client the runs API needs, so
// handlers are unit-testable against a fake without a real Temporal
// connection. go.temporal.io/sdk/client.Client satisfies it. It embeds
// runsubmit.TemporalClient (ExecuteWorkflow) so POST /api/runs shares the
// exact submission call pkg/runsubmit.Submit makes on behalf of both this
// API and `tapectl run`.
type TemporalClient interface {
	ListWorkflow(ctx context.Context, request *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error)
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	QueryWorkflow(ctx context.Context, workflowID string, runID string, queryType string, args ...interface{}) (converter.EncodedValue, error)
	runsubmit.TemporalClient
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

// SubmitRunRequest is the POST /api/runs request body: a run-config JSON
// document (config, decoded with internal/config.Parse — the same
// validation `tapectl run` applies) and whether to submit it as a dry-run
// (pkg/runsubmit.ApplyDryRun — the same mhvtl override `tapectl run
// --dry-run` applies). Config is kept as raw JSON rather than decoded
// inline so config.Parse (not encoding/json directly) performs the decode,
// preserving its DisallowUnknownFields behavior and validation.
type SubmitRunRequest struct {
	Config json.RawMessage `json:"config"`
	DryRun bool            `json:"dryRun"`
}

// SubmitRunResponse is the POST /api/runs response body on success: enough
// to identify the started execution and link straight to GET
// /api/runs/{runID}.
type SubmitRunResponse struct {
	WorkflowID string `json:"workflowId"`
	RunID      string `json:"runId"`
}

// New builds the /api/runs HTTP handler. temporalClient must be non-nil.
func New(temporalClient TemporalClient) http.Handler {
	return newMux(newHandler(temporalClient, os.Getenv))
}

// newHandler builds a handler with an injectable getenv, so tests can drive
// the dry-run mhvtl-env-var gate (runsubmit.ApplyDryRun) without mutating
// the process environment. New wires this to os.Getenv for production.
func newHandler(temporalClient TemporalClient, getenv func(string) string) *handler {
	return &handler{temporalClient: temporalClient, getenv: getenv}
}

// newMux mounts h's handlers behind their routes.
func newMux(h *handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs", h.listRuns)
	mux.HandleFunc("GET /api/runs/{runID}", h.getRun)
	mux.HandleFunc("POST /api/runs", h.submitRun)

	return mux
}

type handler struct {
	temporalClient TemporalClient
	getenv         func(string) string
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

// submitRun implements POST /api/runs: parse and validate the run config
// (internal/config.Parse — identical to `tapectl run`'s config.LoadFile),
// optionally apply the dry-run mhvtl override (runsubmit.ApplyDryRun —
// identical to `tapectl run --dry-run`), then submit the backup workflow
// under its fixed singleton options (runsubmit.Submit). Every failure short
// of Temporal actually accepting the submission — a malformed request body,
// an invalid config, a dry-run with the mhvtl env vars unset — is reported
// as 400 before any Temporal RPC is made, mirroring `tapectl run`'s
// client-side-first validation order.
func (h *handler) submitRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	// http.MaxBytesReader (not a bare io.LimitReader): an oversized body needs
	// a distinguishable signal, not a silent truncation that json.Decode would
	// otherwise fail on with a generic, confusing "unexpected EOF".
	body := http.MaxBytesReader(w, r.Body, maxSubmitBodyBytes)

	var request SubmitRunRequest

	decoder := json.NewDecoder(body)
	// DisallowUnknownFields: SubmitRunRequest has exactly two fields, and a
	// misspelled/wrong "dryRun" key (e.g. "isDryRun") must fail the request
	// rather than silently defaulting DryRun to false and submitting a real,
	// non-dry-run run — the same fail-closed reasoning internal/config.Parse
	// already applies one call below.
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&request); err != nil {
		status := http.StatusBadRequest

		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			status = http.StatusRequestEntityTooLarge
		}

		writeError(w, status, fmt.Errorf("parse request body: %w", err))

		return
	}

	if len(request.Config) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("config is required"))

		return
	}

	cfg, err := config.Parse(request.Config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)

		return
	}

	if request.DryRun {
		// io.Discard: there is no stderr-shaped place to surface the
		// optical-burn advisory over HTTP; the response already reports
		// whether the submission succeeded, and the dry-run override itself
		// is not sensitive (unlike an operator-facing CLI, no human is
		// watching this process's stderr).
		if err := runsubmit.ApplyDryRun(cfg, h.getenv, io.Discard); err != nil {
			writeError(w, http.StatusBadRequest, err)

			return
		}
	}

	run, err := runsubmit.Submit(ctx, h.temporalClient, cfg)
	if err != nil {
		writeError(w, statusForTemporalError(err), err)

		return
	}

	w.Header().Set("Location", "/api/runs/"+run.GetRunID())
	writeJSON(w, http.StatusCreated, SubmitRunResponse{WorkflowID: run.GetID(), RunID: run.GetRunID()})
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
// endpoints (resume/abort — docs/web-ui-design.md §8) extend one consistent
// mapping instead of each hand-rolling its own: a missing resource is
// serviceerror.NotFound (404), a malformed client-supplied identifier/
// argument is serviceerror.InvalidArgument (400), a singleton-conflict
// submission is serviceerror.WorkflowExecutionAlreadyStarted (409 Conflict —
// runs are a singleton, SPEC §4.2, so a second submission while one is
// in-flight is a conflict with existing state, not a missing/malformed
// request), and anything else is 502 — this handler is a proxy over
// Temporal, so an unclassified failure is upstream, not the client's fault.
//
// submitRun passes this the error returned by runsubmit.Submit, which wraps
// (via %w, see runsubmit.TranslateSubmitError) rather than replaces the
// underlying serviceerror, so errors.As here still recovers it.
func statusForTemporalError(err error) int {
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return http.StatusNotFound
	}

	var invalidArgument *serviceerror.InvalidArgument
	if errors.As(err, &invalidArgument) {
		return http.StatusBadRequest
	}

	var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &alreadyStarted) {
		return http.StatusConflict
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
