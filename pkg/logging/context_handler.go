package logging

import (
	"context"
	"log/slog"
)

// Temporal-run-identity attribute keys. These MUST stay byte-for-byte identical
// to the tags the Temporal SDK injects via workflow.GetLogger/activity.GetLogger
// (go.temporal.io/sdk/internal/internal_logging_tags.go: "WorkflowID"/"RunID"),
// because the web UI's log query filters on RunID (pkg/runsapi/logs.go) and the
// vector shipper groups streams by both (scripts/web-dev-vector.yml
// _stream_fields=WorkflowID,RunID). A rename here silently empties the run-detail
// log panels — the exact bug this handler exists to fix (#303).
const (
	workflowIDKey = "WorkflowID"
	runIDKey      = "RunID"
)

// runTagsContextKey is the private context key under which ContextWithRunTags
// stashes the run identity. A dedicated unexported type avoids collisions with
// any other package's context values.
type runTagsContextKey struct{}

// runTags is the Temporal run identity lifted onto every log record emitted while
// an activity is executing.
type runTags struct {
	workflowID string
	runID      string
}

// ContextWithRunTags returns a child of ctx carrying the Temporal WorkflowID/RunID
// that the handler installed by Setup lifts onto every log record logged with that
// context. The worker's activity interceptor calls this once per activity so that
// bulk logging (plain slog.*Context calls in activities and pkg/* helpers) carries
// run identity without threading a logger through every call site. Empty values
// are stored as-is and simply omitted from records by the handler.
func ContextWithRunTags(ctx context.Context, workflowID, runID string) context.Context {
	return context.WithValue(ctx, runTagsContextKey{}, runTags{workflowID: workflowID, runID: runID})
}

// runTagsFrom extracts the run identity stored by ContextWithRunTags. ok is false
// when the context carries none (e.g. cmd/web request handling, or any log emitted
// outside an activity), in which case the handler adds nothing.
func runTagsFrom(ctx context.Context) (runTags, bool) {
	tags, ok := ctx.Value(runTagsContextKey{}).(runTags)

	return tags, ok
}

// contextHandler wraps a slog.Handler and, on every record, lifts the Temporal run
// identity carried by the log context (see ContextWithRunTags) onto the record as
// WorkflowID/RunID attributes. It is a no-op enrichment when the context carries no
// run tags, so process-global logging outside an activity is unaffected.
type contextHandler struct {
	slog.Handler
}

// Handle adds the WorkflowID/RunID attributes from ctx (when present and non-empty)
// before delegating to the wrapped handler.
func (h contextHandler) Handle(ctx context.Context, record slog.Record) error {
	if tags, ok := runTagsFrom(ctx); ok {
		if tags.workflowID != "" {
			record.AddAttrs(slog.String(workflowIDKey, tags.workflowID))
		}

		if tags.runID != "" {
			record.AddAttrs(slog.String(runIDKey, tags.runID))
		}
	}

	return h.Handler.Handle(ctx, record)
}

// WithAttrs and WithGroup preserve the wrapping so run-tag enrichment survives the
// derived loggers the Temporal SDK and callers build with slog.With/WithGroup.
func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{Handler: h.Handler.WithGroup(name)}
}
