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

	commonpb "go.temporal.io/api/common/v1"
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
	// CancelWorkflow requests graceful cancellation of a run (POST
	// /api/runs/{runID}/cancel — cancelRun). Unlike SignalWorkflow's
	// resume/abort, which only apply to a run already paused for the operator,
	// this cancels any in-progress execution: Temporal delivers cancellation
	// into the workflow, whose deferred cleanup (session teardown, hold-tag
	// release, the failure/cancellation Discord alert) runs on a
	// workflow.NewDisconnectedContext so LTFS mounts are torn down and the run
	// closes in a defined, reported Canceled state (SPEC §10). It is the
	// graceful counterpart the workflow was built for — never TerminateWorkflow,
	// which would skip that cleanup and risk leaving mounts/tapes wedged.
	CancelWorkflow(ctx context.Context, workflowID string, runID string) error
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
	// DryRun is true when the run was submitted as a dry-run (the mhvtl
	// override, runsubmit.ApplyDryRun), read back from the run's Temporal memo
	// (runsubmit.MemoKeyDryRun). False when the memo is absent — runs submitted
	// before this memo existed simply read as production, which is the safe
	// default for an unlabelled run.
	DryRun bool `json:"dryRun"`
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

	// lastCompletedPhaseUnknown records that the last-completed-phase query
	// could not be answered this fetch (RPC failed / would not decode), as
	// opposed to a successful query reporting "" (nothing completed yet). It is
	// unexported — never serialized — and consulted only by streamRunEvents'
	// poll loop, which carries the last known phase forward on an unknown tick
	// rather than emitting a spurious "phase regressed to empty" update, the
	// same transient-blip handling CurrentPauseInfo.Unknown gets below.
	lastCompletedPhaseUnknown bool

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

// WithTemporalUI supplies the browsable Temporal Web UI base URL and the
// namespace runs execute in, so the SPA can deep-link a run to its workflow
// history in the Temporal UI (served by GET /api/config/ui). An empty baseURL
// disables the link: the endpoint reports it empty and the SPA shows no link.
func WithTemporalUI(baseURL, namespace string) Option {
	return func(h *handler) {
		h.temporalUIBaseURL = baseURL
		h.temporalNamespace = namespace
	}
}

// WithDeployConfig supplies the deployment's fixed library device targets and
// Discord webhook URL, which the guided config form sources read-only rather
// than exposing as per-run free-text inputs (issue #304 — see uiconfig.go).
// They are reported by GET /api/config/ui; any left empty simply arrives empty
// at the SPA, and the guided form's Review step then surfaces internal/config's
// own validation (changer must not be empty, at least one drive is required)
// rather than the server guessing a default.
func WithDeployConfig(changer string, drives []string, webhookURL string) Option {
	return func(h *handler) {
		h.deployChanger = changer
		h.deployDrives = drives
		h.deployWebhookURL = webhookURL
	}
}

// WithOpticalBurnerDrives supplies the deployment's fixed optical burner device
// paths (issue #317), the delivery analogue of the library drives in
// WithDeployConfig: a burner device path (e.g. /dev/sr0) is a property of the
// deployment/host, not a per-run choice, so the guided config form sources it
// read-only and the operator only toggles optical burn on/off and sets the copy
// count per run. Reported by GET /api/config/ui; an empty list simply arrives
// empty at the SPA. Unlike the library devices/webhook, this is applied server-
// side only when a submitted run actually enables optical burn (see
// applyDeployConfig), so an unset/disabled burn never gains a spurious block.
func WithOpticalBurnerDrives(drives []string) Option {
	return func(h *handler) {
		h.deployOpticalBurnerDrives = drives
	}
}

// WithLibraryTopology supplies the physical library's topology (issue #305): the
// storage slot count and the cleaning / I/O-station slot numbers. GET
// /api/config/ui reports these so the guided config form renders a slot-grid
// picker bounded to the real library — a grid of storage slots 1..slotCount with
// the cleaning and I/O-station slots rendered non-selectable — instead of a
// free-form list of arbitrary slot numbers. Like the devices in WithDeployConfig,
// the topology is a property of the deployment's physical library, not a per-run
// choice; a zero slotCount simply arrives unconfigured at the SPA, whose picker
// then shows a "not configured" state (the operator can still set blank slots via
// JSON / paste mode).
func WithLibraryTopology(slotCount int, cleaningSlots, ioStationSlots []int) Option {
	return func(h *handler) {
		h.deploySlotCount = slotCount
		h.deployCleaningSlots = cleaningSlots
		h.deployIOStationSlots = ioStationSlots
	}
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

	// Built after options so it captures the final drain context
	// (WithDrainContext). The broker coalesces every SSE connection watching a
	// given run onto one shared Temporal poll loop — see events.go.
	h.broker = newRunBroker(h.fetchRunDetailUntilDrain, h.drain)

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
	mux.HandleFunc("POST /api/runs/{runID}/cancel", h.cancelRun)
	mux.HandleFunc("GET /api/events/runs/{runID}", h.streamRunEvents)
	// History-derived endpoints (issue #273): reconstructed on demand from
	// Temporal workflow history, never from a persisted catalog (SPEC §4.2).
	// See history.go's doc comment for why these read raw history events
	// rather than replay-based queries.
	mux.HandleFunc("GET /api/runs/{runID}/phases", h.getRunPhases)
	mux.HandleFunc("GET /api/runs/{runID}/config", h.getRunConfig)
	mux.HandleFunc("GET /api/runs/{runID}/tapes", h.getRunTapes)
	mux.HandleFunc("GET /api/runs/{runID}/delivery", h.getRunDelivery)
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
	// Server-provided deploy config the SPA needs to build outbound links
	// (currently the run overview's Temporal Web UI deep-link) — see
	// uiconfig.go.
	mux.HandleFunc("GET /api/config/ui", h.getUIConfig)

	// OpenAPI 3.1 description of this API plus a browsable docs page
	// (openapi.go). These delegate to a dedicated Huma-built mux that only ever
	// serves the spec/docs; the data routes above keep serving the real
	// traffic. Mounting only these specific paths (not the whole docs mux)
	// keeps Huma's inert stub operation routes off the served surface — see
	// openapi.go's doc comment.
	docs := newDocsHandler()
	mux.Handle("GET /api/docs", docs)
	mux.Handle("GET /api/openapi.json", docs)
	mux.Handle("GET /api/openapi.yaml", docs)
	mux.Handle("GET /api/openapi-3.0.json", docs)
	mux.Handle("GET /api/openapi-3.0.yaml", docs)
	mux.Handle("GET /api/schemas/{schema}", docs)

	return mux
}

type handler struct {
	temporalClient      TemporalClient
	getenv              func(string) string
	generateAgeIdentity func(context.Context) (identity, recipient string, err error)
	// drain is done when the hosting server has begun graceful shutdown —
	// see WithDrainContext. Defaults to context.Background() (never done).
	drain context.Context
	// broker coalesces every SSE connection watching the same run onto one
	// shared Temporal poll loop (events.go), rather than one poller per
	// connection. Built by newHandler after options are applied.
	broker *runBroker
	// temporalUIBaseURL is the browsable Temporal Web UI base URL (cmd/web's
	// TEMPORAL_UI_URL); empty when unconfigured, in which case GET
	// /api/config/ui reports it empty and the SPA omits the run overview's
	// Temporal-workflow deep-link. temporalNamespace is the namespace those
	// deep-links target (temporalclient.ResolveNamespace — the same profile
	// the client dials). Both are set via WithTemporalUI.
	temporalUIBaseURL string
	temporalNamespace string
	// deployChanger, deployDrives, and deployWebhookURL are the deployment's
	// fixed library device targets and Discord webhook URL, reported by GET
	// /api/config/ui so the guided config form can source them read-only
	// instead of as per-run free-text inputs (issue #304). Empty when
	// unconfigured; set via WithDeployConfig.
	deployChanger    string
	deployDrives     []string
	deployWebhookURL string
	// deployOpticalBurnerDrives is the deployment's fixed optical burner device
	// paths (issue #317), reported by GET /api/config/ui so the guided config
	// form sources them read-only instead of as a per-run free-text input — the
	// delivery analogue of deployDrives. Empty when unconfigured; set via
	// WithOpticalBurnerDrives. Unlike the fields above it is applied only when a
	// submitted run actually enables optical burn (see applyDeployConfig), so a
	// run with no opticalBurn block never gains a spurious one.
	deployOpticalBurnerDrives []string
	// deploySlotCount, deployCleaningSlots, and deployIOStationSlots are the
	// physical library's topology (issue #305), reported by GET /api/config/ui
	// so the guided config form can render a slot-grid picker bounded to the
	// real library — storage slots numbered 1..deploySlotCount, with the
	// cleaning and I/O-station slot numbers rendered non-selectable. All zero /
	// empty when the deployment did not declare a topology; set via
	// WithLibraryTopology.
	deploySlotCount      int
	deployCleaningSlots  []int
	deployIOStationSlots []int
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

	executions, err := listAllBackupExecutions(ctx, h.temporalClient)
	if err != nil {
		writeError(w, statusForTemporalError(err), fmt.Errorf("list workflow executions: %w", err))

		return
	}

	runs := make([]RunSummary, 0, len(executions))

	for _, execution := range executions {
		runs = append(runs, toRunSummary(execution))
	}

	sort.Slice(runs, func(i, j int) bool { return runs[i].StartTime.After(runs[j].StartTime) })

	writeJSON(w, http.StatusOK, RunsResponse{Runs: runs})
}

// maxVisibilityScan caps how many executions listAllBackupExecutions will
// accumulate across pages, a memory backstop far above any realistic backup
// history (runs are a singleton, SPEC §4.2, so executions accrue slowly). If a
// deployment ever exceeds it the scan stops and logs, rather than paging
// unbounded — a bounded, logged truncation, never a silent one.
const maxVisibilityScan = 10000

// maxVisibilityPages bounds how many visibility pages a single scan follows,
// independent of maxVisibilityScan's row cap. The row cap alone does not bound
// the loop when Temporal returns pages that carry a NextPageToken but few or
// no rows (the standard SQL visibility store can legitimately do this):
// len(all)/scanned never reaches the row cap, so the loop would page on until
// the token finally empties or the request deadline fires. This page cap is
// the backstop for that case — comfortably above maxVisibilityScan/listPageSize
// full pages, so it only ever trips on sparse/empty paging, not a genuinely
// large history.
const maxVisibilityPages = 100

// listAllBackupExecutions returns every visibility record for the singleton
// backup workflow, following NextPageToken across pages rather than reading
// only the first (a single ListWorkflow returns at most listPageSize, and the
// standard SQL visibility store does not guarantee newest-first ordering
// within a page, so reading one page could both truncate the history and drop
// genuinely-newest runs). The caller sorts the result; this only gathers it.
func listAllBackupExecutions(ctx context.Context, temporalClient TemporalClient) ([]*workflowpb.WorkflowExecutionInfo, error) {
	var (
		all   []*workflowpb.WorkflowExecutionInfo
		token []byte
		pages int
	)

	for {
		response, err := temporalClient.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Query:         workflowIDQuery(),
			PageSize:      listPageSize,
			NextPageToken: token,
		})
		if err != nil {
			return nil, err
		}

		all = append(all, response.GetExecutions()...)
		pages++

		token = response.GetNextPageToken()
		if len(token) == 0 {
			break
		}

		if len(all) >= maxVisibilityScan || pages >= maxVisibilityPages {
			slog.WarnContext(ctx, "runsapi: visibility scan hit its cap; older runs are omitted from this listing",
				"rows", len(all), "row_cap", maxVisibilityScan, "pages", pages, "page_cap", maxVisibilityPages)

			break
		}
	}

	return all, nil
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

	var (
		lastCompletedPhase        string
		lastCompletedPhaseUnknown bool
	)

	group.Go(func() error {
		phase, known := queryLastCompletedPhase(groupCtx, temporalClient, runID)
		lastCompletedPhase = phase
		lastCompletedPhaseUnknown = !known

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
		RunSummary:                toRunSummary(description.GetWorkflowExecutionInfo()),
		LastCompletedPhase:        lastCompletedPhase,
		lastCompletedPhaseUnknown: lastCompletedPhaseUnknown,
		CurrentPause:              pauseInfo,
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

	// Reject any trailing content after the single JSON object: a body like
	// `{...} {...}` or `{...} <garbage>` must not be silently accepted with
	// only its first value decoded — the same fail-closed handling
	// DisallowUnknownFields already gives an unexpected field.
	if decoder.More() {
		writeError(w, http.StatusBadRequest, errors.New("request body must contain a single JSON object"))

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

	// Overwrite the deploy-owned library devices and Discord webhook with this
	// deployment's own values (issue #304) before submit. Hiding the Form-mode
	// inputs (#309) only stopped one client path; sourcing the values here —
	// server-side, over whatever config was submitted — is what actually stops
	// any client (the config page's JSON/paste mode, or a raw POST) from
	// targeting a changer, drive, or webhook the host does not own. Runs before
	// the dry-run block below so a dry run's mhvtl override still wins: a dry run
	// must never touch real hardware.
	if applied := h.applyDeployConfig(cfg); applied {
		// Re-validate: an override replaces already-parsed fields with the
		// deployment's values, so a deployment that misconfigured them (e.g.
		// duplicate drive paths) surfaces here rather than reaching Temporal.
		if err := cfg.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid config after applying deploy-owned devices/webhook: %w", err))

			return
		}
	}

	// Enforce device ownership for production runs (CLAUDE.md Hardware and
	// Safety; issue #304): the deployment, not the submitter, must own the
	// physical library devices a real run touches. applyDeployConfig overrides
	// them only where the deployment configured them, so a deployment that
	// configured none would otherwise let a client-submitted config target
	// arbitrary real device nodes. A dry run is exempt — ApplyDryRun below
	// overrides every device to the mhvtl virtual library, so it can never
	// reach real hardware regardless of what was submitted.
	if !request.DryRun {
		if err := h.requireDeviceOwnership(cfg); err != nil {
			writeError(w, http.StatusBadRequest, err)

			return
		}
	}

	// The per-run blank-slot selection (library.blankSlots) is an operator
	// choice, but the deployment's library topology (issue #305) bounds it: a
	// slot must be a real storage slot, not out of range and not a reserved
	// cleaning / I/O-station slot. The guided Form's grid picker enforces this
	// client-side, but JSON / paste mode and raw POSTs bypass that, and
	// internal/config only rejects negative/duplicate slots — so enforce the
	// topology bound here too, the server-side analogue of the picker.
	if err := h.validateBlankSlotsAgainstTopology(cfg); err != nil {
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

	run, err := runsubmit.Submit(ctx, h.temporalClient, cfg, request.DryRun)
	if err != nil {
		writeError(w, statusForTemporalError(err), err)

		return
	}

	w.Header().Set("Location", "/api/runs/"+run.GetRunID())
	writeJSON(w, http.StatusCreated, SubmitRunResponse{WorkflowID: run.GetID(), RunID: run.GetRunID()})
}

// applyDeployConfig overwrites cfg's deploy-owned fields — the library
// changer/drive devices, the Discord webhook URL (issue #304), and the optical
// burner drives (issue #317) — with the values this deployment supplied via
// WithDeployConfig / WithOpticalBurnerDrives, and reports whether it changed
// anything. These are properties of the host, not per-run choices, so the
// deployment, not the submitter, is authoritative on them regardless of how the
// config was built.
//
// Each field is overridden only when the deployment configured it, so a
// deployment that sets none leaves the submitted config untouched (today's
// behavior), and one that sets some overrides exactly those — mirroring the
// per-field "not configured" the guided Form mode shows for an unset value. The
// burner drives additionally require the submitted run to enable optical burn
// (carry an opticalBurn block): a burn-off run never gains a spurious block.
// Slices are copied, not aliased, so the shared handler slices can never be
// mutated through the returned config.
func (h *handler) applyDeployConfig(cfg *config.Config) bool {
	changed := false

	if h.deployChanger != "" {
		cfg.Library.Changer = h.deployChanger
		changed = true
	}

	if len(h.deployDrives) > 0 {
		cfg.Library.Drives = append([]string{}, h.deployDrives...)
		changed = true
	}

	if h.deployWebhookURL != "" {
		cfg.Delivery.WebhookURL = h.deployWebhookURL
		changed = true
	}

	// Optical burner drives (issue #317) differ from the fields above: they are
	// applied only when the submitted run actually enables optical burn — i.e.
	// carries an opticalBurn block. A run with no block (burn off) must never
	// gain a spurious one, so we override the drives in place rather than
	// creating the section. Whether the block then burns anything is still the
	// operator's per-run choice (a positive copy count — OpticalBurn.Enabled).
	if len(h.deployOpticalBurnerDrives) > 0 && cfg.Delivery.OpticalBurn != nil {
		cfg.Delivery.OpticalBurn.Drives = append([]string{}, h.deployOpticalBurnerDrives...)
		changed = true
	}

	return changed
}

// requireDeviceOwnership rejects a production submit whose physical library
// devices — or delivery webhook — are not owned by this deployment (issue #304,
// CLAUDE.md Hardware and Safety). applyDeployConfig overrides the changer/drives
// (and, when optical burn is enabled, the burner drives) and the webhook only
// where the deployment configured them; where it configured none, the submitted
// config's own values survive, and a real run must not target hardware the host
// has not declared it owns nor deliver its escrow-key-bearing report to a
// client-supplied webhook. The remedy is spelled out in each message: configure
// the deployment's devices/webhook, or submit as a dry-run (which targets mhvtl
// and is exempt). Callers apply this only for non-dry-run submits.
func (h *handler) requireDeviceOwnership(cfg *config.Config) error {
	if h.deployChanger == "" {
		return errors.New("this deployment does not configure a library changer (set LIBRARY_CHANGER): a production run may not target a client-supplied changer — configure the deployment's devices, or submit as a dry-run")
	}

	if len(h.deployDrives) == 0 {
		return errors.New("this deployment does not configure library drives (set LIBRARY_DRIVES): a production run may not target client-supplied drives — configure the deployment's devices, or submit as a dry-run")
	}

	if cfg.Delivery.OpticalBurn.Enabled() && len(h.deployOpticalBurnerDrives) == 0 {
		return errors.New("this deployment does not configure optical burner drives (set OPTICAL_BURNER_DRIVES): a production run with optical burn enabled may not target client-supplied burner drives — configure the deployment's burner, disable optical burn, or submit as a dry-run")
	}

	// The Discord delivery webhook is deploy-owned too (issue #304): the run's
	// report embeds the age escrow private identity (pkg/report, SPEC §7), so a
	// production run must not deliver it to a client-supplied webhook. When the
	// deployment configured one, applyDeployConfig already replaced the client's
	// value with it (so this branch does not fire); when it did not, a
	// client-supplied webhook cannot be verified as owned and is refused.
	if h.deployWebhookURL == "" && cfg.Delivery.WebhookURL != "" {
		return errors.New("this deployment does not configure a delivery webhook (set DELIVERY_WEBHOOK_URL): a production run may not deliver its report — which embeds the escrow private key — to a client-supplied webhook — configure the deployment's webhook, remove delivery.webhookUrl, or submit as a dry-run")
	}

	return nil
}

// validateBlankSlotsAgainstTopology rejects a submitted config whose
// library.blankSlots fall outside the deployment's declared library topology
// (issue #305): a slot must be a real storage slot in [1, deploySlotCount] and
// not a reserved cleaning or I/O-station slot. It is the server-side analogue
// of the guided Form's slot-grid picker, closing the JSON / paste-mode and
// raw-POST paths the picker cannot cover.
//
// When the deployment declared no topology (deploySlotCount == 0 — unset or
// unparseable LIBRARY_SLOT_COUNT), the bound is unknown, so this is a no-op and
// only internal/config's own negative/duplicate checks apply — mirroring the
// Form, whose picker shows "not configured" and imposes no bound in that case.
func (h *handler) validateBlankSlotsAgainstTopology(cfg *config.Config) error {
	if h.deploySlotCount <= 0 {
		return nil
	}

	reserved := make(map[int]string, len(h.deployCleaningSlots)+len(h.deployIOStationSlots))
	for _, slot := range h.deployCleaningSlots {
		reserved[slot] = "a cleaning slot"
	}

	for _, slot := range h.deployIOStationSlots {
		reserved[slot] = "an I/O-station slot"
	}

	for i, slot := range cfg.Library.BlankSlots {
		if slot < 1 || slot > h.deploySlotCount {
			return fmt.Errorf("library.blankSlots[%d]: slot %d is outside the library's storage slots (1-%d)", i, slot, h.deploySlotCount)
		}

		if kind, ok := reserved[slot]; ok {
			return fmt.Errorf("library.blankSlots[%d]: slot %d is %s, not a storage slot", i, slot, kind)
		}
	}

	return nil
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

// cancelRun implements POST /api/runs/{runID}/cancel: request graceful
// Temporal cancellation of an in-progress run (TemporalClient.CancelWorkflow).
// Unlike resumeRun/abortRun, this is not a pause signal and needs no pause —
// it is the operator's "stop this run now" control for any still-running
// execution, paused or not. Cancellation is delivered into the workflow,
// whose deferred cleanup runs on a disconnected context (SPEC §10), so the run
// tears down its LTFS mounts, releases its ZFS hold, posts the
// failure/cancellation alert, and closes as Canceled rather than being killed
// mid-flight.
//
// It confirms via DescribeWorkflowExecution that the execution is still Running
// before requesting cancellation, so an already-closed run is rejected with a
// clear 409 (errRunNotInProgress) rather than the opaque error CancelWorkflow
// returns for a completed execution. Like signalPausedRun's pause check, this
// is NOT atomic with the CancelWorkflow that follows: a run can close in the
// gap between the two RPCs (it finished on its own, or another operator
// cancelled it), in which case CancelWorkflow's own error is surfaced. That is
// harmless — cancelling an already-closed run is a no-op — and the run's real
// closed state is authoritative over the live view (GET /api/runs/{runID} / the
// SSE stream) either way.
func (h *handler) cancelRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	runID := r.PathValue("runID")

	if runID == "" {
		writeError(w, http.StatusBadRequest, errors.New("runID is required"))

		return
	}

	description, err := h.temporalClient.DescribeWorkflowExecution(ctx, backup.WorkflowID, runID)
	if err != nil {
		switch status := statusForTemporalError(err); status {
		case http.StatusNotFound:
			writeError(w, status, fmt.Errorf("run %q not found", runID))
		default:
			writeError(w, status, fmt.Errorf("describe workflow execution: %w", err))
		}

		return
	}

	if status := description.GetWorkflowExecutionInfo().GetStatus(); status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		writeError(w, statusForTemporalError(errRunNotInProgress), fmt.Errorf("%w (run is %s)", errRunNotInProgress, toRunSummary(description.GetWorkflowExecutionInfo()).Status))

		return
	}

	if err := h.temporalClient.CancelWorkflow(ctx, backup.WorkflowID, runID); err != nil {
		writeError(w, statusForTemporalError(err), fmt.Errorf("cancel workflow: %w", err))

		return
	}

	writeJSON(w, http.StatusAccepted, ActionResponse{Status: "cancel requested"})
}

// queryLastCompletedPhase asks the workflow for its last completed phase via
// the agreed query (workflows/backup/contract.go). A query failure (e.g. no
// worker currently polling) is not fatal to the request — the execution
// status/timing already fetched via Describe is still valid — so it is
// logged and reported as "" rather than failing GET /api/runs/{runID}.
// The returned known flag is false when the query could not be answered (the
// RPC failed or its result would not decode), distinct from a successful query
// that reports an empty phase (nothing completed yet). streamRunEvents needs
// that distinction: a failed query must carry the last known phase forward
// rather than look like a regression to "" — the same reason queryCurrentPause
// surfaces CurrentPauseInfo.Unknown (see events.go's poll loop).
func queryLastCompletedPhase(ctx context.Context, temporalClient TemporalClient, runID string) (phase string, known bool) {
	response, err := temporalClient.QueryWorkflow(ctx, backup.WorkflowID, runID, backup.LastCompletedPhaseQuery)
	if err != nil {
		slog.WarnContext(ctx, "runsapi: last completed phase query failed", "run_id", runID, "error", err)

		return "", false
	}

	if err := response.Get(&phase); err != nil {
		slog.WarnContext(ctx, "runsapi: decode last completed phase query result failed", "run_id", runID, "error", err)

		return "", false
	}

	return phase, true
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

// errRunNotInProgress is a synthetic (non-Temporal) error cancelRun returns
// when a cancel request targets a run that DescribeWorkflowExecution reports has
// already closed (any status other than Running). Like errRunNotPaused, the
// request is well-formed and the run exists, but the action conflicts with the
// run's current state — there is no in-progress execution left to cancel — so
// it maps to 409 Conflict via statusForTemporalError.
var errRunNotInProgress = errors.New("run is not in progress; only a running run can be cancelled")

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
// signalPausedRun (resumeRun/abortRun) and cancelRun additionally pass this
// three synthetic, non-Temporal errors — errRunNotPaused,
// errEjectPauseCannotAbort, and errRunNotInProgress — all mapped to 409
// Conflict: the request is well-formed and the run exists, but the action
// conflicts with the run's current (pause or closed) state, the same Conflict
// reasoning already used below for a singleton-submission clash.
func statusForTemporalError(err error) int {
	if errors.Is(err, errRunNotPaused) || errors.Is(err, errEjectPauseCannotAbort) || errors.Is(err, errRunNotInProgress) {
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

	// A request that hit its own requestTimeout (the child context deadline
	// each handler sets) is an upstream slowness, not an upstream fault, so it
	// is a Gateway Timeout rather than the generic Bad Gateway below — 502
	// would misattribute a slow-but-healthy Temporal as a broken one. A
	// context.Canceled means the client itself went away mid-request; the
	// response is never read, but 499 (client closed request) classifies it
	// correctly in this proxy's own logs instead of blaming Temporal.
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}

	if errors.Is(err, context.Canceled) {
		return statusClientClosedRequest
	}

	return http.StatusBadGateway
}

// statusClientClosedRequest is the non-standard 499 status (originated by
// nginx) for "client closed the connection before the server answered". Go's
// net/http defines no constant for it; runsapi uses it only to classify a
// context.Canceled in its own logs — the client is already gone, so the code
// itself is never delivered.
const statusClientClosedRequest = 499

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

	summary.DryRun = dryRunFromMemo(execution.GetMemo())

	return summary
}

// dryRunFromMemo reads the dry-run flag from a run's Temporal memo
// (runsubmit.MemoKeyDryRun). A missing memo field, or any decode failure,
// reports false: an unlabelled or unreadable run is treated as production
// rather than mislabelled as a dry-run, and old runs predating the memo simply
// have no field.
func dryRunFromMemo(memo *commonpb.Memo) bool {
	payload, ok := memo.GetFields()[runsubmit.MemoKeyDryRun]
	if !ok {
		return false
	}

	var dryRun bool
	if err := converter.GetDefaultDataConverter().FromPayload(payload, &dryRun); err != nil {
		return false
	}

	return dryRun
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
//
// For the two upstream-fault statuses statusForTemporalError produces (502 Bad
// Gateway, 504 Gateway Timeout), err is the raw Temporal/gRPC error, which can
// embed internal endpoint/host/status detail — that is logged server-side but
// not returned to the client; the client gets a generic status message. Every
// other status (4xx validation errors, this package's own 503 availability
// messages) is the client's own actionable text and is returned as-is.
func writeError(w http.ResponseWriter, status int, err error) {
	if status == http.StatusBadGateway || status == http.StatusGatewayTimeout {
		slog.Error("runsapi: upstream request failed", "status", status, "error", err)
	}

	writeJSON(w, status, errorResponse{Error: clientFacingMessage(status, err)})
}

// clientFacingMessage returns the message safe to return to a client for err at
// the given status. For the two upstream-fault statuses statusForTemporalError
// produces (502 Bad Gateway, 504 Gateway Timeout) the raw Temporal/gRPC error
// can embed internal endpoint/host/status detail, so it is replaced with a
// generic status text; every other status (4xx validation, this package's own
// 503 availability messages) is the client's own actionable text and passes
// through. The raw error is the caller's to log. Shared by writeError and the
// per-run RunError the aggregate tape listing embeds in an otherwise-200 body,
// so the same masking applies whether the error is the whole response's status
// or one degraded row within it.
func clientFacingMessage(status int, err error) string {
	if status == http.StatusBadGateway || status == http.StatusGatewayTimeout {
		return http.StatusText(status)
	}

	return err.Error()
}
