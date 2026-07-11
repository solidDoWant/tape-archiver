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

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"golang.org/x/sync/errgroup"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/agewrap"
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
	SignalWorkflow(ctx context.Context, workflowID string, runID string, signalName string, arg interface{}) error
	// GetWorkflowHistory returns an iterator over a run's raw event history.
	// history.go's fetchRunHistory walks it to reconstruct the phase
	// timeline, the originally submitted run config, and tape/copy outcomes
	// entirely on demand (SPEC §4.2 — no persistent state or catalog):
	// issue #273's endpoints derive everything from these events rather than
	// from a replay-based query, which would panic on a closed run whose
	// history predates the currently-deployed workflow code (nondeterminism
	// — see history.go's doc comment).
	GetWorkflowHistory(ctx context.Context, workflowID string, runID string, isLongPoll bool, filterType enumspb.HistoryEventFilterType) client.HistoryEventIterator
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

	// CurrentPause is which operator-in-the-loop pause (if any) is blocking the
	// run right now (backup.CurrentPauseQuery), with enough context for a
	// client to act on it via POST /api/runs/{runID}/resume or /abort without
	// consulting the Temporal UI event history. Like LastCompletedPhase
	// above, a query failure here is logged server-side rather than failing
	// the request — but unlike LastCompletedPhase, Kind == "" alone does not
	// mean "not paused": CurrentPauseInfo.Unknown distinguishes a confirmed
	// not-paused run from a failed query, since collapsing the two would
	// hide a run that genuinely needs operator action. Also carried over GET
	// /api/events/runs/{runID} (events.go), whose poll loop compares this
	// field (alongside Status/LastCompletedPhase) to detect a pause starting
	// or clearing so a live view updates without a manual refresh — a poll
	// tick with Unknown set carries the last known pause state forward
	// rather than comparing against it, so a transient query blip can never
	// look like "the pause cleared".
	CurrentPause CurrentPauseInfo `json:"currentPause"`
}

// CurrentPauseInfo is the GET /api/runs/{runID} (and SSE) JSON projection of
// backup.CurrentPause: the same fields, translated to the API's camelCase
// JSON convention rather than exposing the workflow package's internal Go
// field names directly.
type CurrentPauseInfo struct {
	// Kind is which pause is active: "eject", "write-failure", "burn", or ""
	// (not paused). Mirrors backup.PauseKind's string values.
	Kind string `json:"kind"`
	// Phase is the failing phase for a write-failure pause ("Load" or
	// "Write"); omitted for an eject or burn pause.
	Phase string `json:"phase,omitempty"`
	// AffectedTapes lists barcodes to act on: the tapes to swap for a
	// write-failure pause, or the tapes ready for removal for an eject pause.
	AffectedTapes []string `json:"affectedTapes,omitempty"`
	// ReloadSlots lists the storage slots to restock with fresh blanks before
	// resuming a write-failure pause.
	ReloadSlots []int `json:"reloadSlots,omitempty"`
	// AwaitingExport is the count of written tapes still to be exported, for
	// an eject pause.
	AwaitingExport int `json:"awaitingExport,omitempty"`
	// Devices lists the optical burner devices needing a fresh blank disc, for
	// a burn pause.
	Devices []string `json:"devices,omitempty"`
	// ErrorSummary is the pause reason rendered as text — empty for an eject
	// pause, and for a burn pause that is a between-set disc swap rather than
	// a failure.
	ErrorSummary string `json:"errorSummary,omitempty"`
	// CanAbort mirrors abortRun's own errEjectPauseCannotAbort check — the
	// single source of truth for which pause kinds accept
	// POST /api/runs/{runID}/abort — so a client (e.g. PauseActions.tsx)
	// decides whether to show an Abort control from this field rather than
	// hand-duplicating the same kind-based rule and risking the two
	// silently drifting apart. Always false when Kind == "" or Unknown.
	CanAbort bool `json:"canAbort,omitempty"`
	// Unknown is true when backup.CurrentPauseQuery itself failed (e.g. no
	// worker currently polling) rather than confirming the run isn't paused.
	// Kind == "" alone cannot distinguish "confirmed not paused" from
	// "couldn't ask" — collapsing the two would let a run that genuinely
	// needs operator action render as healthy on a transient query blip.
	// Clients should treat Unknown == true as "pause status unavailable,
	// check `tapectl status`", not as "not paused".
	Unknown bool `json:"unknown,omitempty"`
}

// toCurrentPauseInfo maps a workflow query result to the API's JSON shape.
func toCurrentPauseInfo(pause backup.CurrentPause) CurrentPauseInfo {
	return CurrentPauseInfo{
		Kind:           string(pause.Kind),
		Phase:          pause.Phase,
		AffectedTapes:  pause.AffectedTapes,
		ReloadSlots:    pause.ReloadSlots,
		AwaitingExport: pause.AwaitingExport,
		Devices:        pause.Devices,
		ErrorSummary:   pause.ErrorSummary,
		CanAbort:       pauseAcceptsAbort(pause.Kind),
	}
}

// pauseAcceptsAbort reports whether workflows/backup's OperatorAbortSignal
// applies to kind — the single source of truth abortRun and
// toCurrentPauseInfo (CurrentPauseInfo.CanAbort) both use, so the API's
// rejection and the value clients are told to build their UI from can never
// drift apart. Every pause kind accepts abort except Eject: every tape is
// already safely written by the time an Eject pause happens, so there is
// nothing left for an abort to protect against, and OperatorAbortSignal is
// not among the signals ejectPhase's wait even listens for.
func pauseAcceptsAbort(kind backup.PauseKind) bool {
	return kind != backup.PauseNone && kind != backup.PauseEject
}

// ActionResponse is the response body for POST /api/runs/{runID}/resume and
// POST /api/runs/{runID}/abort: confirmation that the signal was sent, not
// that the run has necessarily processed it yet — the resume/abort signals
// are asynchronous (SPEC §4.3); GET /api/runs/{runID}'s CurrentPause (or the
// SSE stream) is the authoritative way to observe whether the run actually
// resumed or ended.
type ActionResponse struct {
	Status string `json:"status"`
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

// Option configures New's handler beyond its required Temporal client.
type Option func(*handler)

// WithDrainContext supplies a context that is cancelled when the hosting
// server begins graceful shutdown. Long-lived streaming handlers (the SSE
// run-event stream) return promptly once it is done, so http.Server.Shutdown
// can reach connection quiescence instead of stalling on them until its
// deadline (issue #270). Ordinary request/response handlers ignore it: they
// are bounded by requestTimeout already and should be allowed to finish
// their in-flight work during a graceful drain. When not supplied, streams
// only end on client disconnect or run completion, as before.
func WithDrainContext(ctx context.Context) Option {
	return func(h *handler) { h.drain = ctx }
}

// New builds the /api/runs HTTP handler. temporalClient must be non-nil.
func New(temporalClient TemporalClient, opts ...Option) http.Handler {
	return newMux(newHandler(temporalClient, os.Getenv, opts...))
}

// newHandler builds a handler with an injectable getenv, so tests can drive
// the dry-run mhvtl-env-var gate (runsubmit.ApplyDryRun) without mutating
// the process environment. New wires this to os.Getenv for production. The
// age-keygen dependency (generateAgeIdentity) is likewise injectable, for the
// same reason: agekeygen_test.go drives generateAgeKeypair's error path
// without depending on how the real age-keygen binary happens to fail. New
// wires it to agewrap.GenerateIdentity for production.
func newHandler(temporalClient TemporalClient, getenv func(string) string, opts ...Option) *handler {
	h := &handler{
		temporalClient:      temporalClient,
		getenv:              getenv,
		generateAgeIdentity: agewrap.GenerateIdentity,
		// A background default means "never drained": the drain case in
		// streaming handlers' selects simply never fires unless the host
		// opted in via WithDrainContext.
		drain: context.Background(),
	}

	for _, opt := range opts {
		opt(h)
	}

	return h
}

// newMux mounts h's handlers behind their routes.
func newMux(h *handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs", h.listRuns)
	mux.HandleFunc("GET /api/runs/{runID}", h.getRun)
	mux.HandleFunc("POST /api/runs", h.submitRun)
	mux.HandleFunc("POST /api/runs/{runID}/resume", h.resumeRun)
	mux.HandleFunc("POST /api/runs/{runID}/abort", h.abortRun)
	mux.HandleFunc("GET /api/events/runs/{runID}", h.streamRunEvents)
	// History-derived endpoints (issue #273): reconstructed on demand from
	// Temporal workflow history, never from a persisted catalog (SPEC §4.2).
	// See history.go's doc comment for why these read raw history events
	// rather than replay-based queries.
	mux.HandleFunc("GET /api/runs/{runID}/phases", h.getRunPhases)
	mux.HandleFunc("GET /api/runs/{runID}/config", h.getRunConfig)
	mux.HandleFunc("GET /api/runs/{runID}/tapes", h.getRunTapes)
	mux.HandleFunc("GET /api/tapes", h.listTapes)
	// Live VictoriaMetrics-backed drive metrics (issue #275): a thin,
	// allowlisted PromQL proxy scoped to this run's own tapes — see
	// metrics.go's doc comment.
	mux.HandleFunc("GET /api/runs/{runID}/metrics/drives", h.getRunDriveMetrics)
	mux.HandleFunc("GET /api/runs/{runID}/metrics/drives/{barcode}/history", h.getRunDriveMetricsHistory)
	// VictoriaLogs-backed log panel (issue #274): a plain JSON proxy, not a
	// persisted catalog (SPEC §4.2) and not this package's usual SSE
	// pattern — see logs.go's doc comment for why.
	mux.HandleFunc("GET /api/runs/{runID}/logs", h.getRunLogs)
	// Config-page support (issue #279): the committed run-config JSON Schema
	// (for client-side validation) and age post-quantum keypair generation —
	// see configschema.go's and agekeygen.go's doc comments.
	mux.HandleFunc("GET /api/config/schema", h.getConfigSchema)
	mux.HandleFunc("POST /api/age/keygen", h.generateAgeKeypair)

	return mux
}

type handler struct {
	temporalClient      TemporalClient
	getenv              func(string) string
	generateAgeIdentity func(context.Context) (identity, recipient string, err error)
	// drain is done when the hosting server has begun graceful shutdown —
	// see WithDrainContext. Defaults to context.Background() (never done).
	drain context.Context
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
		Query:    workflowIDQuery(),
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

	detail, err := fetchRunDetail(ctx, h.temporalClient, runID)
	if err != nil {
		writeRunDetailError(w, runID, err)

		return
	}

	writeJSON(w, http.StatusOK, detail)
}

// fetchRunDetail fetches one execution's current detail: DescribeWorkflowExecution
// and the last-completed-phase and current-pause queries, run concurrently since
// none needs another's result, only backup.WorkflowID and runID. This is the
// single shared core both getRun and streamRunEvents use to answer "what is this
// run's state right now" — the SSE handler is a poll loop around exactly this
// call, so the two endpoints can never drift on what "current state" means for a
// run (and a pause starting/clearing is visible over the live stream for free,
// via events.go's delta check on the returned CurrentPause).
func fetchRunDetail(ctx context.Context, temporalClient TemporalClient, runID string) (RunDetail, error) {
	group, groupCtx := errgroup.WithContext(ctx)

	var description *workflowservice.DescribeWorkflowExecutionResponse

	group.Go(func() error {
		var err error

		description, err = temporalClient.DescribeWorkflowExecution(groupCtx, backup.WorkflowID, runID)

		return err
	})

	var lastCompletedPhase string

	group.Go(func() error {
		lastCompletedPhase = queryLastCompletedPhase(groupCtx, temporalClient, runID)

		return nil
	})

	var (
		currentPause        backup.CurrentPause
		currentPauseUnknown bool
	)

	group.Go(func() error {
		pause, err := queryCurrentPause(groupCtx, temporalClient, runID)
		if err != nil {
			// A query failure here (e.g. no worker currently polling) is not
			// fatal to this request, mirroring queryLastCompletedPhase above:
			// the execution status/timing already fetched via Describe is
			// still valid and useful on its own. Unlike LastCompletedPhase,
			// though, this field gates whether an operator sees Resume/Abort
			// controls, so the failure is recorded (CurrentPauseInfo.Unknown)
			// rather than silently collapsed into "not paused" — a run that
			// is genuinely paused must never render as healthy just because
			// one query attempt hiccuped.
			slog.WarnContext(groupCtx, "runsapi: current pause query failed", "run_id", runID, "error", err)

			currentPauseUnknown = true

			return nil
		}

		currentPause = pause

		return nil
	})

	if err := group.Wait(); err != nil {
		return RunDetail{}, err
	}

	pauseInfo := toCurrentPauseInfo(currentPause)
	pauseInfo.Unknown = currentPauseUnknown

	return RunDetail{
		RunSummary:         toRunSummary(description.GetWorkflowExecutionInfo()),
		LastCompletedPhase: lastCompletedPhase,
		CurrentPause:       pauseInfo,
	}, nil
}

// writeRunDetailError classifies and writes a fetchRunDetail failure as a JSON
// error response, shared by getRun and streamRunEvents' initial fetch (the
// only point in the SSE handler where a Temporal error is still reported as a
// normal HTTP status rather than folded into the event stream itself — see
// streamRunEvents' doc comment).
func writeRunDetailError(w http.ResponseWriter, runID string, err error) {
	switch status := statusForTemporalError(err); status {
	case http.StatusNotFound:
		writeError(w, status, fmt.Errorf("run %q not found", runID))
	case http.StatusBadRequest:
		// A syntactically invalid run ID (Temporal run IDs are UUIDs) comes
		// back as InvalidArgument rather than NotFound — e.g.
		// "Invalid RunId." — so it is a 400 (bad client input), not a 404
		// (well-formed ID, no such run) or a 502 (this is not an upstream
		// failure).
		writeError(w, status, fmt.Errorf("invalid run ID %q: %w", runID, err))
	default:
		writeError(w, status, fmt.Errorf("describe workflow execution: %w", err))
	}
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

// resumeRun implements POST /api/runs/{runID}/resume: send
// backup.OperatorResumeSignal to the run, the same signal `tapectl resume`
// sends (cmd/tapectl/resume.go). Every pause kind accepts resume, so no
// allow-check is needed beyond "the run is currently paused" (signalPausedRun
// itself).
func (h *handler) resumeRun(w http.ResponseWriter, r *http.Request) {
	h.signalPausedRun(w, r, backup.OperatorResumeSignal, "resume", func(backup.CurrentPause) error {
		return nil
	})
}

// abortRun implements POST /api/runs/{runID}/abort: send
// backup.OperatorAbortSignal to the run, the same signal `tapectl abort`
// sends (cmd/tapectl/abort.go). An Eject pause (backup.PauseEject) rejects
// abort: see errEjectPauseCannotAbort and pauseAcceptsAbort.
func (h *handler) abortRun(w http.ResponseWriter, r *http.Request) {
	h.signalPausedRun(w, r, backup.OperatorAbortSignal, "abort", func(pause backup.CurrentPause) error {
		if !pauseAcceptsAbort(pause.Kind) {
			return errEjectPauseCannotAbort
		}

		return nil
	})
}

// signalPausedRun implements the shared resume/abort request flow. Unlike
// `tapectl resume`/`abort`, which signal unconditionally with no pause-state
// check (acceptable for a human operator who just watched the pause happen),
// this handler confirms via backup.CurrentPauseQuery that the run is
// currently paused, and lets allow reject a pause kind the signal does not
// apply to, before sending anything.
//
// This check is NOT atomic with the signal send that follows it — they are
// two independent Temporal RPCs, so it narrows but does not close the
// stale-signal hazard workflows/backup's drainStalePauseSignals doc
// describes (issues #154/#216): if the pause resolves by other means (a
// concurrent `tapectl`/browser action, or — for an Eject pause specifically —
// its own auto-resume, entirely independent of any signal, see
// waitForIOCleared) in the gap between this check and SignalWorkflow
// returning, the signal this handler sends can still be delivered late and
// buffered. Every pause site drains both stale resume and stale abort
// signals before its next alert (issue #254), so a signal buffered by this
// race can resume or abort at most the pause it raced, never leak forward
// onto a LATER, UNRELATED pause on the same run. Fixing that leak is out of
// scope for a web-API-side check alone — it belongs in, and is handled by,
// workflows/backup's drain.
func (h *handler) signalPausedRun(w http.ResponseWriter, r *http.Request, signalName, verb string, allow func(backup.CurrentPause) error) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	runID := r.PathValue("runID")

	if runID == "" {
		writeError(w, http.StatusBadRequest, errors.New("runID is required"))

		return
	}

	pause, err := queryCurrentPause(ctx, h.temporalClient, runID)
	if err != nil {
		switch status := statusForTemporalError(err); status {
		case http.StatusNotFound:
			writeError(w, status, fmt.Errorf("run %q not found", runID))
		default:
			writeError(w, status, fmt.Errorf("query current pause state: %w", err))
		}

		return
	}

	if pause.Kind == backup.PauseNone {
		writeError(w, statusForTemporalError(errRunNotPaused), errRunNotPaused)

		return
	}

	if err := allow(pause); err != nil {
		writeError(w, statusForTemporalError(err), err)

		return
	}

	if err := h.temporalClient.SignalWorkflow(ctx, backup.WorkflowID, runID, signalName, nil); err != nil {
		writeError(w, statusForTemporalError(err), fmt.Errorf("signal workflow to %s: %w", verb, err))

		return
	}

	writeJSON(w, http.StatusAccepted, ActionResponse{Status: verb + " signal sent"})
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

// queryCurrentPause asks the workflow which operator-in-the-loop pause (if
// any) is currently active, via the agreed query (backup.CurrentPauseQuery,
// workflows/backup/contract.go). Unlike queryLastCompletedPhase above, it
// propagates a query failure to the caller rather than swallowing it:
// resumeRun/abortRun need to know the actual pause state before deciding
// whether to send a signal — signalPausedRun's doc comment explains why
// acting on an unknown pause state is unsafe here — while fetchRunDetail, the
// other caller, degrades this to a warning log itself (mirroring its handling
// of queryLastCompletedPhase) since the rest of that response is still
// useful.
func queryCurrentPause(ctx context.Context, temporalClient TemporalClient, runID string) (backup.CurrentPause, error) {
	response, err := temporalClient.QueryWorkflow(ctx, backup.WorkflowID, runID, backup.CurrentPauseQuery)
	if err != nil {
		return backup.CurrentPause{}, err
	}

	var pause backup.CurrentPause
	if err := response.Get(&pause); err != nil {
		return backup.CurrentPause{}, err
	}

	return pause, nil
}

// errRunNotPaused is a synthetic (non-Temporal) error signalPausedRun returns
// when a resume/abort request targets a run that backup.CurrentPauseQuery
// reports is not currently paused. It flows through statusForTemporalError
// like any other resume/abort failure so callers get one consistent mapping.
var errRunNotPaused = errors.New("run is not currently paused")

// errEjectPauseCannotAbort is a synthetic (non-Temporal) error abortRun
// returns when an abort request targets an Eject pause (backup.PauseEject):
// every tape is already safely written by the time that pause fires, so
// workflows/backup's waitForIOCleared never listens for
// backup.OperatorAbortSignal — only resume applies. Rejecting this before any
// signal is sent avoids leaving an unconsumed abort signal buffered on the
// running workflow, where it could be wrongly consumed by a later, unrelated
// pause (see signalPausedRun's doc comment).
var errEjectPauseCannotAbort = errors.New("this pause cannot be aborted; only resume applies to an eject pause")

// statusForTemporalError classifies a Temporal RPC error into the HTTP
// status that best represents it, shared by every runsapi handler so every
// endpoint extends one consistent mapping instead of each hand-rolling its
// own: a missing resource is serviceerror.NotFound (404), a malformed
// client-supplied identifier/argument is serviceerror.InvalidArgument (400),
// a singleton-conflict submission is
// serviceerror.WorkflowExecutionAlreadyStarted (409 Conflict — runs are a
// singleton, SPEC §4.2, so a second submission while one is in-flight is a
// conflict with existing state, not a missing/malformed request), and
// anything else is 502 — this handler is a proxy over Temporal, so an
// unclassified failure is upstream, not the client's fault.
//
// submitRun passes this the error returned by runsubmit.Submit, which wraps
// (via %w, see runsubmit.TranslateSubmitError) rather than replaces the
// underlying serviceerror, so errors.As here still recovers it.
//
// signalPausedRun (resumeRun/abortRun) additionally passes this two synthetic,
// non-Temporal errors — errRunNotPaused and errEjectPauseCannotAbort — both
// mapped to 409 Conflict: the request is well-formed and the run exists, but
// the action conflicts with the run's current (pause) state, the same
// Conflict reasoning already used below for a singleton-submission clash.
func statusForTemporalError(err error) int {
	if errors.Is(err, errRunNotPaused) || errors.Is(err, errEjectPauseCannotAbort) {
		return http.StatusConflict
	}

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
