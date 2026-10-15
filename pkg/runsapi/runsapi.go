// Package runsapi implements the JSON HTTP API cmd/web serves under /api/runs.
// Submission reuses pkg/runsubmit so the CLI and this API cannot drift; reads
// are views over Temporal with no UI-owned store (SPEC §4.2).
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

// requestTimeout bounds every Temporal RPC a handler makes: the http.Server
// timeouts do not cancel an in-flight handler's context.
const requestTimeout = 10 * time.Second

// listPageSize bounds a single visibility page; runs are a serial singleton,
// so history stays small.
const listPageSize = 1000

const maxSubmitBodyBytes = 4 << 20 // 4 MiB

// TemporalClient is the subset of client.Client the runs API needs.
type TemporalClient interface {
	ListWorkflow(ctx context.Context, request *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error)
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	QueryWorkflow(ctx context.Context, workflowID string, runID string, queryType string, args ...interface{}) (converter.EncodedValue, error)
	SignalWorkflow(ctx context.Context, workflowID string, runID string, signalName string, arg interface{}) error
	// CancelWorkflow is used instead of TerminateWorkflow so the workflow's
	// disconnected-context cleanup (LTFS teardown, hold release, alert) runs
	// (SPEC §10).
	CancelWorkflow(ctx context.Context, workflowID string, runID string) error
	// GetWorkflowHistory is read raw (not via replay-based queries) so closed
	// runs predating the deployed workflow code cannot panic on nondeterminism.
	GetWorkflowHistory(ctx context.Context, workflowID string, runID string, isLongPoll bool, filterType enumspb.HistoryEventFilterType) client.HistoryEventIterator
	// DescribeTaskQueue reports pollers, to tell whether any worker is serving a waiting activity.
	DescribeTaskQueue(ctx context.Context, taskQueue string, taskQueueType enumspb.TaskQueueType) (*workflowservice.DescribeTaskQueueResponse, error)
	runsubmit.TemporalClient
}

var _ TemporalClient = client.Client(nil)

// RunSummary is one execution of the backup workflow, as returned in the GET
// /api/runs list.
type RunSummary struct {
	WorkflowID string     `json:"workflowId"`
	RunID      string     `json:"runId"`
	Status     string     `json:"status"`
	StartTime  time.Time  `json:"startTime"`
	CloseTime  *time.Time `json:"closeTime,omitempty"`
	// DryRun is read from the run's memo; runs without the memo read as
	// production, the safe default.
	DryRun bool `json:"dryRun"`
}

// RunsResponse is the GET /api/runs response body.
type RunsResponse struct {
	Runs []RunSummary `json:"runs"`
}

// RunDetail is one execution's detail, as returned by GET /api/runs/{runID}.
type RunDetail struct {
	RunSummary

	// LastCompletedPhase is "" when nothing has completed yet or the phase
	// could not be determined; LastCompletedPhaseUnknown distinguishes the two.
	LastCompletedPhase string `json:"lastCompletedPhase"`

	// LastCompletedPhaseUnknown is set when both the live query and the history
	// fallback failed, so a client never renders a read failure as "not started"
	// (issue #323).
	LastCompletedPhaseUnknown bool `json:"lastCompletedPhaseUnknown,omitempty"`

	// CurrentPause is the operator pause blocking the run, if any. Kind == ""
	// alone does not mean "not paused"; see CurrentPauseInfo.Unknown.
	CurrentPause CurrentPauseInfo `json:"currentPause"`
}

// CurrentPauseInfo is the camelCase JSON projection of backup.CurrentPause.
type CurrentPauseInfo struct {
	// Kind is "eject", "write-failure", "burn", or "" (not paused).
	Kind string `json:"kind"`
	// Phase is the failing phase for a write-failure pause ("Load" or "Write").
	Phase string `json:"phase,omitempty"`
	// AffectedTapes are the tapes to swap (write-failure) or remove (eject).
	AffectedTapes []string `json:"affectedTapes,omitempty"`
	// ReloadSlots are slots to restock with blanks before resuming.
	ReloadSlots []int `json:"reloadSlots,omitempty"`
	// AwaitingExport is the count of written tapes still to export.
	AwaitingExport int `json:"awaitingExport,omitempty"`
	// Devices are the optical burners needing a fresh blank disc.
	Devices []string `json:"devices,omitempty"`
	// ErrorSummary is the pause reason; empty for eject and plain disc swaps.
	ErrorSummary string `json:"errorSummary,omitempty"`
	// CanAbort comes from pauseAcceptsAbort, the same rule abortRun enforces,
	// so the UI cannot drift from the API.
	CanAbort bool `json:"canAbort,omitempty"`
	// Unknown is true when neither the live query nor the history fallback
	// could determine the pause state (issue #329). Clients must not treat it
	// as "not paused".
	Unknown bool `json:"unknown,omitempty"`
}

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

// pauseAcceptsAbort reports whether OperatorAbortSignal applies to kind. An
// Eject pause does not listen for abort: every tape is already written.
func pauseAcceptsAbort(kind backup.PauseKind) bool {
	return kind != backup.PauseNone && kind != backup.PauseEject
}

// ActionResponse is the response body for the resume/abort/cancel endpoints.
// It confirms the request was sent, not that the run has processed it.
type ActionResponse struct {
	Status string `json:"status"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// SubmitRunRequest is the POST /api/runs request body. Config stays raw so
// config.Parse performs the decode with its strict validation.
type SubmitRunRequest struct {
	Config json.RawMessage `json:"config"`
	DryRun bool            `json:"dryRun"`
}

// SubmitRunResponse is the POST /api/runs response body on success.
type SubmitRunResponse struct {
	WorkflowID string `json:"workflowId"`
	RunID      string `json:"runId"`
}

// Option configures New's handler beyond its required Temporal client.
type Option func(*handler)

// WithDrainContext supplies a context cancelled when the server begins
// graceful shutdown, so long-lived SSE streams end promptly (issue #270).
func WithDrainContext(ctx context.Context) Option {
	return func(h *handler) { h.drain = ctx }
}

// WithTemporalUI supplies the Temporal Web UI base URL and namespace for run
// deep-links; an empty baseURL disables the link.
func WithTemporalUI(baseURL, namespace string) Option {
	return func(h *handler) {
		h.temporalUIBaseURL = baseURL
		h.temporalNamespace = namespace
	}
}

// WithDeployConfig supplies the deployment-owned library devices and Discord
// webhook URL, which override any submitted values (issue #304).
func WithDeployConfig(changer string, drives []string, webhookURL string) Option {
	return func(h *handler) {
		h.deployChanger = changer
		h.deployDrives = drives
		h.deployWebhookURL = webhookURL
	}
}

// WithOpticalBurnerDrives supplies the deployment-owned optical burner device
// paths (issue #317), applied only to runs that enable optical burn.
func WithOpticalBurnerDrives(drives []string) Option {
	return func(h *handler) {
		h.deployOpticalBurnerDrives = drives
	}
}

// WithLibraryTopology supplies the library's storage slot count and reserved
// cleaning / I/O-station slots, used to bound blank-slot selection (issue #305).
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

// newHandler takes an injectable getenv so tests can drive the dry-run env
// gate without mutating the process environment.
func newHandler(temporalClient TemporalClient, getenv func(string) string, opts ...Option) *handler {
	h := &handler{
		temporalClient:      temporalClient,
		getenv:              getenv,
		generateAgeIdentity: agewrap.GenerateIdentity,
		// Never drained unless WithDrainContext is supplied.
		drain: context.Background(),
		// Captured once so request goroutines never read the test-mutated global.
		heartbeatInterval: sseHeartbeatInterval,
	}

	for _, opt := range opts {
		opt(h)
	}

	// Built after options so it captures the final drain context.
	h.broker = newRunBroker(h.fetchRunDetailUntilDrain, h.drain)

	return h
}

func newMux(h *handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs", h.listRuns)
	mux.HandleFunc("GET /api/runs/{runID}", h.getRun)
	mux.HandleFunc("POST /api/runs", h.submitRun)
	mux.HandleFunc("POST /api/runs/{runID}/resume", h.resumeRun)
	mux.HandleFunc("POST /api/runs/{runID}/abort", h.abortRun)
	mux.HandleFunc("POST /api/runs/{runID}/cancel", h.cancelRun)
	mux.HandleFunc("GET /api/events/runs/{runID}", h.streamRunEvents)
	// Derived on demand from raw workflow history (issue #273, SPEC §4.2).
	mux.HandleFunc("GET /api/runs/{runID}/phases", h.getRunPhases)
	mux.HandleFunc("GET /api/runs/{runID}/config", h.getRunConfig)
	mux.HandleFunc("GET /api/runs/{runID}/tapes", h.getRunTapes)
	mux.HandleFunc("GET /api/runs/{runID}/delivery", h.getRunDelivery)
	mux.HandleFunc("GET /api/tapes", h.listTapes)
	// VictoriaMetrics drive metrics (issue #275).
	mux.HandleFunc("GET /api/runs/{runID}/metrics/drives", h.getRunDriveMetrics)
	mux.HandleFunc("GET /api/runs/{runID}/metrics/drives/{barcode}/history", h.getRunDriveMetricsHistory)
	// VictoriaLogs log panel (issue #274).
	mux.HandleFunc("GET /api/runs/{runID}/logs", h.getRunLogs)
	// Config page support (issue #279).
	mux.HandleFunc("GET /api/config/schema", h.getConfigSchema)
	mux.HandleFunc("POST /api/age/keygen", h.generateAgeKeypair)
	// Deploy config the SPA needs for outbound links and the guided form.
	mux.HandleFunc("GET /api/config/ui", h.getUIConfig)

	// Only the spec/docs paths are mounted, keeping Huma's stub operation
	// routes off the served surface (see openapi.go).
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
	// drain is done once graceful shutdown begins (WithDrainContext).
	drain context.Context
	// broker coalesces SSE connections for the same run onto one poll loop.
	broker *runBroker
	// heartbeatInterval is captured at construction to avoid racing tests that restore the global.
	heartbeatInterval time.Duration
	// temporalUI* drive the run overview's Temporal deep-link; empty disables it.
	temporalUIBaseURL string
	temporalNamespace string
	// Deploy-owned devices and webhook (issue #304); empty when unconfigured.
	deployChanger    string
	deployDrives     []string
	deployWebhookURL string
	// deployOpticalBurnerDrives (issue #317) applies only to runs enabling optical burn.
	deployOpticalBurnerDrives []string
	// Library topology (issue #305); zero when undeclared.
	deploySlotCount      int
	deployCleaningSlots  []int
	deployIOStationSlots []int
}

// listRuns implements GET /api/runs, newest first.
//
// Sorting happens here because the standard SQL visibility store rejects
// ORDER BY (only Elasticsearch accepts it).
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

// maxVisibilityScan is a memory backstop on rows; hitting it is logged.
const maxVisibilityScan = 10000

// maxVisibilityPages bounds the loop when SQL visibility returns sparse/empty
// pages that still carry a NextPageToken, which the row cap alone would not.
const maxVisibilityPages = 100

// listAllBackupExecutions follows NextPageToken across pages: SQL visibility
// does not order within a page, so reading only the first could drop the
// newest runs.
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

// fetchRunDetail is the shared "current state" core of getRun and the SSE
// stream, so the two cannot drift.
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
			// Not fatal; marked unknown and resolved from history below. A
			// paused run must never render as healthy on a query blip.
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

	// A failed live query almost always means no control worker is polling
	// (KEDA scaled to zero during a pause, issue #329). History needs no
	// worker and is authoritative, so a successful derivation clears Unknown.
	if currentPauseUnknown || lastCompletedPhaseUnknown {
		if history, historyErr := fetchRunHistory(ctx, temporalClient, runID); historyErr != nil {
			slog.WarnContext(ctx, "runsapi: history fallback for run detail failed", "run_id", runID, "error", historyErr)
		} else {
			if currentPauseUnknown {
				currentPause = derivePause(history)
				currentPauseUnknown = false
			}

			if lastCompletedPhaseUnknown {
				lastCompletedPhase = deriveLastCompletedPhase(history)
				lastCompletedPhaseUnknown = false
			}
		}
	}

	pauseInfo := toCurrentPauseInfo(currentPause)
	pauseInfo.Unknown = currentPauseUnknown

	return RunDetail{
		RunSummary:                toRunSummary(description.GetWorkflowExecutionInfo()),
		LastCompletedPhase:        lastCompletedPhase,
		LastCompletedPhaseUnknown: lastCompletedPhaseUnknown,
		CurrentPause:              pauseInfo,
	}, nil
}

func writeRunDetailError(w http.ResponseWriter, runID string, err error) {
	switch status := statusForTemporalError(err); status {
	case http.StatusNotFound:
		writeError(w, status, fmt.Errorf("run %q not found", runID))
	case http.StatusBadRequest:
		// A malformed run ID comes back as InvalidArgument, not NotFound.
		writeError(w, status, fmt.Errorf("invalid run ID %q: %w", runID, err))
	default:
		writeError(w, status, fmt.Errorf("describe workflow execution: %w", err))
	}
}

// submitRun implements POST /api/runs with the same validation and dry-run
// override as `tapectl run`. All client errors return 400 before any
// Temporal RPC.
func (h *handler) submitRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	// MaxBytesReader gives a distinguishable error rather than a silent
	// truncation surfacing as "unexpected EOF".
	body := http.MaxBytesReader(w, r.Body, maxSubmitBodyBytes)

	var request SubmitRunRequest

	decoder := json.NewDecoder(body)
	// Fail closed: a misspelled "dryRun" must not silently submit a real run.
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

	// Enforced server-side so no client path can target devices or a webhook
	// the host does not own (issue #304). Runs before the dry-run override so
	// a dry run still never touches real hardware.
	if applied := h.applyDeployConfig(cfg); applied {
		// Re-validate: a misconfigured deployment value must not reach Temporal.
		if err := cfg.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid config after applying deploy-owned devices/webhook: %w", err))

			return
		}
	}

	// Dry runs are exempt: ApplyDryRun redirects every device to mhvtl.
	if !request.DryRun {
		if err := h.requireDeviceOwnership(cfg); err != nil {
			writeError(w, http.StatusBadRequest, err)

			return
		}
	}

	if err := h.validateBlankSlotsAgainstTopology(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)

		return
	}

	if request.DryRun {
		// No operator watches this process's stderr, so the advisory is dropped.
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

// applyDeployConfig overwrites cfg's deploy-owned fields with each value the
// deployment configured, reporting whether anything changed. Slices are copied
// so the shared handler slices cannot be mutated through cfg.
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

	// Only override an existing opticalBurn block; a burn-off run must never
	// gain one.
	if len(h.deployOpticalBurnerDrives) > 0 && cfg.Delivery.OpticalBurn != nil {
		cfg.Delivery.OpticalBurn.Drives = append([]string{}, h.deployOpticalBurnerDrives...)
		changed = true
	}

	return changed
}

// requireDeviceOwnership rejects a production submit that would use devices
// or a webhook this deployment has not declared (issue #304).
func (h *handler) requireDeviceOwnership(cfg *config.Config) error {
	if h.deployChanger == "" {
		return errors.New("this deployment does not configure a library changer (set LIBRARY_CHANGER): a production run may not target a client-supplied changer — configure the deployment's devices, or submit as a dry-run")
	}

	if len(h.deployDrives) == 0 {
		return errors.New("this deployment does not configure library drives (set LIBRARY_DRIVES): a production run may not target client-supplied drives — configure the deployment's devices, or submit as a dry-run")
	}

	// Checks drives even on a disabled (copies: 0) block, which would otherwise
	// carry a client device path into a production config.
	if len(h.deployOpticalBurnerDrives) == 0 && cfg.Delivery.OpticalBurn != nil && len(cfg.Delivery.OpticalBurn.Drives) > 0 {
		return errors.New("this deployment does not configure optical burner drives (set OPTICAL_BURNER_DRIVES): a production run may not target client-supplied burner drives — configure the deployment's burner, remove the delivery.opticalBurn block, or submit as a dry-run")
	}

	// The report embeds the age escrow private identity (SPEC §7), so it must
	// not go to a client-supplied webhook.
	if h.deployWebhookURL == "" && cfg.Delivery.WebhookURL != "" {
		return errors.New("this deployment does not configure a delivery webhook (set DELIVERY_WEBHOOK_URL): a production run may not deliver its report — which embeds the escrow private key — to a client-supplied webhook — configure the deployment's webhook, remove delivery.webhookUrl, or submit as a dry-run")
	}

	return nil
}

// validateBlankSlotsAgainstTopology rejects blank slots outside
// [1, deploySlotCount] or on a reserved slot (issue #305). It is a no-op when
// no topology is configured.
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

func (h *handler) resumeRun(w http.ResponseWriter, r *http.Request) {
	h.signalPausedRun(w, r, backup.OperatorResumeSignal, "resume", func(backup.CurrentPause) error {
		return nil
	})
}

func (h *handler) abortRun(w http.ResponseWriter, r *http.Request) {
	h.signalPausedRun(w, r, backup.OperatorAbortSignal, "abort", func(pause backup.CurrentPause) error {
		if !pauseAcceptsAbort(pause.Kind) {
			return errEjectPauseCannotAbort
		}

		return nil
	})
}

// signalPausedRun confirms the run is paused (and allow accepts the pause
// kind) before signalling.
//
// The check is not atomic with the signal, so a late signal can still be
// buffered if the pause resolves in between; workflows/backup drains stale
// signals at every pause site (issues #154/#216/#254), so it cannot leak onto
// a later pause.
func (h *handler) signalPausedRun(w http.ResponseWriter, r *http.Request, signalName, verb string, allow func(backup.CurrentPause) error) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	runID := r.PathValue("runID")

	if runID == "" {
		writeError(w, http.StatusBadRequest, errors.New("runID is required"))

		return
	}

	pause, err := h.currentPauseForAction(ctx, runID)
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

// cancelRun requests graceful Temporal cancellation of a running run, paused
// or not (SPEC §10). A closed run gets a clear 409; a run closing between the
// Describe and the Cancel surfaces CancelWorkflow's own (harmless) error.
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

// queryLastCompletedPhase returns known=false when the query fails, distinct
// from a successful empty phase, so the SSE loop can carry the last known
// phase forward instead of regressing to "".
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

// currentPauseForAction falls back to the history-derived pause when the live
// query fails: a paused run usually has no control worker polling (KEDA scaled
// to zero, issue #329), yet resume/abort must still work. Sending the signal
// itself wakes a worker.
func (h *handler) currentPauseForAction(ctx context.Context, runID string) (backup.CurrentPause, error) {
	pause, err := queryCurrentPause(ctx, h.temporalClient, runID)
	if err == nil {
		return pause, nil
	}

	slog.WarnContext(ctx, "runsapi: current pause query failed for action; falling back to history", "run_id", runID, "error", err)

	history, historyErr := fetchRunHistory(ctx, h.temporalClient, runID)
	if historyErr != nil {
		// Return the live-query error so status classification is unchanged.
		return backup.CurrentPause{}, err
	}

	return derivePause(history), nil
}

var errRunNotPaused = errors.New("run is not currently paused")

// errEjectPauseCannotAbort is rejected before signalling so an unconsumed
// abort is never buffered for a later, unrelated pause to consume.
var errEjectPauseCannotAbort = errors.New("this pause cannot be aborted; only resume applies to an eject pause")

var errRunNotInProgress = errors.New("run is not in progress; only a running run can be cancelled")

// statusForTemporalError maps an error to an HTTP status: NotFound 404,
// InvalidArgument 400, AlreadyStarted and this package's state-conflict errors
// 409, timeouts 504, client cancellation 499, anything else 502 (upstream).
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

	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}

	if errors.Is(err, context.Canceled) {
		return statusClientClosedRequest
	}

	return http.StatusBadGateway
}

// statusClientClosedRequest is nginx's non-standard 499; it only ever reaches
// logs since the client is gone.
const statusClientClosedRequest = 499

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

// dryRunFromMemo reports false for a missing or undecodable memo, so an
// unlabelled run is never mislabelled as a dry-run.
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

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("runsapi: encode JSON response failed", "error", err)
	}
}

// writeError logs upstream (502/504) errors server-side and returns only a
// generic message for them; see clientFacingMessage.
func writeError(w http.ResponseWriter, status int, err error) {
	if status == http.StatusBadGateway || status == http.StatusGatewayTimeout {
		slog.Error("runsapi: upstream request failed", "status", status, "error", err)
	}

	writeJSON(w, status, errorResponse{Error: clientFacingMessage(status, err)})
}

// clientFacingMessage hides raw Temporal/gRPC errors (which can leak internal
// host detail) behind generic text for 502/504; other statuses pass through.
func clientFacingMessage(status int, err error) string {
	if status == http.StatusBadGateway || status == http.StatusGatewayTimeout {
		return http.StatusText(status)
	}

	return err.Error()
}
