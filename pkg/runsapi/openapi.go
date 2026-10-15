package runsapi

// OpenAPI docs for /api/*, using Huma as a describe-only layer: operations are
// registered with inert stub handlers on a private docsMux, and newMux mounts
// only the spec/docs routes, so the real hand-written handlers keep serving.
// Response schemas reuse the real handlers' structs so they cannot drift;
// openapi_test.go fails if a served route lacks a documented operation.

import (
	"context"
	"net/http"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	humasse "github.com/danielgtaylor/huma/v2/sse"

	"github.com/solidDoWant/tape-archiver/internal/buildinfo"
)

// Doc/spec routes: the only parts of docsMux that newMux mounts.
const (
	openAPIPathBase = "/api/openapi" // Huma serves ".json"/".yaml" (+ 3.0 variants) off this.
	docsPath        = "/api/docs"
	schemasPath     = "/api/schemas" // Huma serves "/{schema}" off this.
)

// apiError mirrors the real {"error": "..."} envelope so documented errors
// match what clients receive, rather than Huma's RFC7807 default.
type apiError struct {
	status  int
	Message string `json:"error" doc:"Human-readable error message."`
}

func (e *apiError) Error() string  { return e.Message }
func (e *apiError) GetStatus() int { return e.status }

type runIDInput struct {
	RunID string `path:"runID" doc:"Temporal run ID (a UUID) identifying one execution of the singleton backup workflow." example:"9f8b1c2d-3e4f-5a6b-7c8d-9e0f1a2b3c4d"`
}

type listTapesInput struct {
	Limit int `query:"limit" default:"50" doc:"Maximum number of most-recent tape outcomes to return." example:"50"`
}

type driveMetricsHistoryInput struct {
	RunID   string `path:"runID" doc:"Temporal run ID (a UUID)."`
	Barcode string `path:"barcode" doc:"Tape barcode, as it appears in this run's tape outcomes." example:"TA0001L9"`
	Metric  string `query:"metric" default:"throughput" enum:"throughput,repositions,tapealerts,belowfloor" doc:"Which per-drive time series to return."`
}

type runLogsInput struct {
	RunID string `path:"runID" doc:"Temporal run ID (a UUID)."`
	Phase string `query:"phase" required:"false" doc:"Restrict to one pipeline phase (e.g. \"Write\"); omit for all phases."`
	Since string `query:"since" required:"false" format:"date-time" doc:"Only return lines at or after this RFC3339 timestamp."`
}

type submitRunInput struct {
	Body struct {
		Config map[string]any `json:"config" doc:"Run-config document to submit. GET /api/config/schema returns the authoritative JSON Schema (schemas/run-config.schema.json) for this object."`
		DryRun bool           `json:"dryRun" doc:"When true, submit as an mhvtl-backed dry run instead of targeting real hardware."`
	}
}

// Config-bearing bodies use an opaque object pointing at GET /api/config/schema.

type runsOutput struct{ Body RunsResponse }
type runDetailOutput struct{ Body RunDetail }
type runPhasesOutput struct{ Body RunPhasesResponse }
type runTapesOutput struct{ Body RunTapesResponse }
type runDeliveryOutput struct{ Body RunDeliveryResponse }
type aggregateTapesOutput struct{ Body AggregateTapesResponse }
type driveMetricsOutput struct{ Body DriveMetricsResponse }
type driveMetricsHistoryOutput struct{ Body DriveMetricHistoryResponse }
type runLogsOutput struct{ Body RunLogsResponse }
type ageKeygenOutput struct{ Body AgeKeygenResponse }
type uiConfigOutput struct{ Body uiConfigResponse }

type submitRunOutput struct {
	Location string `header:"Location" doc:"Path of the created run: GET /api/runs/{runID}."`
	Body     SubmitRunResponse
}

type actionOutput struct{ Body ActionResponse }

type runConfigOutput struct {
	Body struct {
		RunID  string         `json:"runId"`
		DryRun bool           `json:"dryRun"`
		Config map[string]any `json:"config" doc:"The run-config document originally submitted (secrets redacted). See GET /api/config/schema for its JSON Schema."`
	}
}

type configSchemaOutput struct {
	Body map[string]any `doc:"The committed run-config JSON Schema (schemas/run-config.schema.json), which describes the POST /api/runs config body."`
}

// meOutput re-declares pkg/webauth's JSON shape to avoid coupling to its internals.
type meOutput struct {
	Body struct {
		Subject string `json:"subject" doc:"OIDC subject (stable per-user identifier)."`
		Email   string `json:"email,omitempty"`
		Name    string `json:"name,omitempty"`
	}
}

type buildInfoOutput struct {
	Body struct {
		Version    string `json:"version" doc:"Server build version."`
		FooterHost string `json:"footerHost,omitempty" doc:"Optional deployment/host label shown in the UI footer."`
	}
}

type noContentInput struct{}

// newDocsHandler returns docsMux, which serves the generated spec and docs page.
func newDocsHandler() http.Handler {
	installHumaErrorModel()

	docsMux := http.NewServeMux()

	config := huma.DefaultConfig("Tape Archiver Web API", buildinfo.ToolVersion())
	config.OpenAPIPath = openAPIPathBase
	config.DocsPath = docsPath
	config.SchemasPath = schemasPath
	config.Info.Description = "JSON API served by cmd/web under /api/*: list and describe backup runs, " +
		"submit and control them, and read their history, tapes, drive metrics, and logs. " +
		"Every route is gated behind OIDC session authentication (a valid session cookie); " +
		"state-changing (POST) routes are additionally subject to a cross-site request guard. " +
		"Read-only views derive entirely from Temporal (visibility, queries, and workflow history) " +
		"with no UI-owned store."

	api := humago.New(docsMux, config)

	registerRunOperations(api)
	registerControlOperations(api)
	registerHistoryOperations(api)
	registerObservabilityOperations(api)
	registerConfigOperations(api)
	registerAuthOperations(api)

	return docsMux
}

// humaErrorModelOnce guards the process-global huma.NewError write, which
// would race when tests build docs handlers in parallel.
var humaErrorModelOnce sync.Once

// installHumaErrorModel is safe process-wide because Huma here only builds
// docs, never serves real requests.
func installHumaErrorModel() {
	humaErrorModelOnce.Do(func() {
		huma.NewError = func(status int, message string, _ ...error) huma.StatusError {
			return &apiError{status: status, Message: message}
		}
	})
}

// stub is never invoked; Huma only needs its I/O types.
func stub[I, O any](context.Context, *I) (*O, error) { return new(O), nil }

const (
	tagRuns    = "Runs"
	tagControl = "Run control"
	tagHistory = "Run history"
	tagObserve = "Metrics & logs"
	tagConfig  = "Config"
	tagAuth    = "Auth"
)

func registerRunOperations(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-runs",
		Method:      http.MethodGet,
		Path:        "/api/runs",
		Summary:     "List backup runs",
		Description: "Every execution of the singleton backup workflow, newest first, from Temporal visibility.",
		Tags:        []string{tagRuns},
	}, stub[noContentInput, runsOutput])

	huma.Register(api, huma.Operation{
		OperationID: "get-run",
		Method:      http.MethodGet,
		Path:        "/api/runs/{runID}",
		Summary:     "Describe a run",
		Description: "Detail for one execution: status/timing plus the last completed phase and the current operator pause (if any).",
		Tags:        []string{tagRuns},
	}, stub[runIDInput, runDetailOutput])

	huma.Register(api, huma.Operation{
		OperationID:   "submit-run",
		Method:        http.MethodPost,
		Path:          "/api/runs",
		Summary:       "Submit a run",
		Description:   "Validate and submit a run config (optionally as a dry run), the same submission path `tapectl run` uses. Deploy-owned devices/webhook are enforced server-side.",
		Tags:          []string{tagRuns},
		DefaultStatus: http.StatusCreated,
	}, stub[submitRunInput, submitRunOutput])
}

func registerControlOperations(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID:   "resume-run",
		Method:        http.MethodPost,
		Path:          "/api/runs/{runID}/resume",
		Summary:       "Resume a paused run",
		Description:   "Send the operator resume signal to a run currently paused for the operator. 409 if the run is not paused.",
		Tags:          []string{tagControl},
		DefaultStatus: http.StatusAccepted,
	}, stub[runIDInput, actionOutput])

	huma.Register(api, huma.Operation{
		OperationID:   "abort-run",
		Method:        http.MethodPost,
		Path:          "/api/runs/{runID}/abort",
		Summary:       "Abort a paused run",
		Description:   "Send the operator abort signal to a paused run. 409 if the run is not paused or the pause is an eject pause (which only accepts resume).",
		Tags:          []string{tagControl},
		DefaultStatus: http.StatusAccepted,
	}, stub[runIDInput, actionOutput])

	huma.Register(api, huma.Operation{
		OperationID:   "cancel-run",
		Method:        http.MethodPost,
		Path:          "/api/runs/{runID}/cancel",
		Summary:       "Cancel a running run",
		Description:   "Request graceful Temporal cancellation of any in-progress run, paused or not; its deferred cleanup runs and it closes as Canceled. 409 if the run is not running.",
		Tags:          []string{tagControl},
		DefaultStatus: http.StatusAccepted,
	}, stub[runIDInput, actionOutput])

	humasse.Register(api, huma.Operation{
		OperationID: "stream-run-events",
		Method:      http.MethodGet,
		Path:        "/api/events/runs/{runID}",
		Summary:     "Stream a run's live state (SSE)",
		Description: "Server-Sent Events over the same state as GET /api/runs/{runID}: an `update` event on first poll and on every state delta, then a final `done` event when the run reaches a terminal status.",
		Tags:        []string{tagRuns},
	}, map[string]any{
		"update": RunDetail{},
		"done":   RunDetail{},
	}, func(context.Context, *runIDInput, humasse.Sender) {})
}

func registerHistoryOperations(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-run-phases",
		Method:      http.MethodGet,
		Path:        "/api/runs/{runID}/phases",
		Summary:     "Run phase timeline",
		Description: "The run's phase timeline with per-phase facts, reconstructed on demand from Temporal workflow history.",
		Tags:        []string{tagHistory},
	}, stub[runIDInput, runPhasesOutput])

	huma.Register(api, huma.Operation{
		OperationID: "get-run-config",
		Method:      http.MethodGet,
		Path:        "/api/runs/{runID}/config",
		Summary:     "Run's submitted config",
		Description: "The run config originally submitted for this run (secrets redacted), reconstructed from workflow history.",
		Tags:        []string{tagHistory},
	}, stub[runIDInput, runConfigOutput])

	huma.Register(api, huma.Operation{
		OperationID: "get-run-tapes",
		Method:      http.MethodGet,
		Path:        "/api/runs/{runID}/tapes",
		Summary:     "Run's tape outcomes",
		Description: "Per-tape load/write outcomes for this run, including write-health measurements, from workflow history.",
		Tags:        []string{tagHistory},
	}, stub[runIDInput, runTapesOutput])

	huma.Register(api, huma.Operation{
		OperationID: "get-run-delivery",
		Method:      http.MethodGet,
		Path:        "/api/runs/{runID}/delivery",
		Summary:     "Run's delivery outcome",
		Description: "The run's delivery (report/optical-burn) outcome, reconstructed from workflow history.",
		Tags:        []string{tagHistory},
	}, stub[runIDInput, runDeliveryOutput])

	huma.Register(api, huma.Operation{
		OperationID: "list-tapes",
		Method:      http.MethodGet,
		Path:        "/api/tapes",
		Summary:     "Recent tapes across runs",
		Description: "The most recent tape outcomes aggregated across runs, each annotated with its run. Per-run errors are reported inline rather than failing the whole response.",
		Tags:        []string{tagHistory},
	}, stub[listTapesInput, aggregateTapesOutput])
}

func registerObservabilityOperations(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-run-drive-metrics",
		Method:      http.MethodGet,
		Path:        "/api/runs/{runID}/metrics/drives",
		Summary:     "Per-drive metrics for a run",
		Description: "Latest per-drive throughput/reposition/tape-alert metrics for this run, proxied from VictoriaMetrics. 503 when VictoriaMetrics is not configured.",
		Tags:        []string{tagObserve},
	}, stub[runIDInput, driveMetricsOutput])

	huma.Register(api, huma.Operation{
		OperationID: "get-run-drive-metrics-history",
		Method:      http.MethodGet,
		Path:        "/api/runs/{runID}/metrics/drives/{barcode}/history",
		Summary:     "One drive metric time series",
		Description: "A single metric's time series for one tape/drive in this run, proxied from VictoriaMetrics. 404 if the barcode is not part of the run; 503 when VictoriaMetrics is not configured.",
		Tags:        []string{tagObserve},
	}, stub[driveMetricsHistoryInput, driveMetricsHistoryOutput])

	huma.Register(api, huma.Operation{
		OperationID: "get-run-logs",
		Method:      http.MethodGet,
		Path:        "/api/runs/{runID}/logs",
		Summary:     "Run logs",
		Description: "Structured log lines for this run, optionally filtered by phase and start time, proxied from VictoriaLogs. 503 when VictoriaLogs is not configured or unreachable.",
		Tags:        []string{tagObserve},
	}, stub[runLogsInput, runLogsOutput])
}

func registerConfigOperations(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-config-schema",
		Method:      http.MethodGet,
		Path:        "/api/config/schema",
		Summary:     "Run-config JSON Schema",
		Description: "The committed run-config JSON Schema, for client-side validation of the POST /api/runs config body.",
		Tags:        []string{tagConfig},
	}, stub[noContentInput, configSchemaOutput])

	huma.Register(api, huma.Operation{
		OperationID: "generate-age-keypair",
		Method:      http.MethodPost,
		Path:        "/api/age/keygen",
		Summary:     "Generate an age keypair",
		Description: "Generate a fresh age post-quantum keypair for the config page's escrow-recipient field. The response is marked no-store.",
		Tags:        []string{tagConfig},
	}, stub[noContentInput, ageKeygenOutput])

	huma.Register(api, huma.Operation{
		OperationID: "get-ui-config",
		Method:      http.MethodGet,
		Path:        "/api/config/ui",
		Summary:     "Deploy-provided UI config",
		Description: "Server-provided, deploy-owned values the SPA needs: the Temporal UI base URL/namespace for deep-links, and the fixed library/delivery device topology the guided config form sources read-only.",
		Tags:        []string{tagConfig},
	}, stub[noContentInput, uiConfigOutput])
}

func registerAuthOperations(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-me",
		Method:      http.MethodGet,
		Path:        "/api/me",
		Summary:     "Current identity",
		Description: "The authenticated operator's identity from their session. Marked no-store.",
		Tags:        []string{tagAuth},
	}, stub[noContentInput, meOutput])

	huma.Register(api, huma.Operation{
		OperationID: "get-build-info",
		Method:      http.MethodGet,
		Path:        "/api/build-info",
		Summary:     "Server build info",
		Description: "Server build version and optional footer host. This is the one /api route that is not session-gated, so the login page can render the footer.",
		Tags:        []string{tagAuth},
	}, stub[noContentInput, buildInfoOutput])
}
