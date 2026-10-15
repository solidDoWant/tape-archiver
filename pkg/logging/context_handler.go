package logging

import (
	"context"
	"log/slog"
)

// These keys must match the Temporal SDK's own logger tags: the web UI's log
// query and the vector shipper filter on them, and a rename silently empties the
// run-detail log panels (#303).
const (
	workflowIDKey = "WorkflowID"
	runIDKey      = "RunID"
)

type runTagsContextKey struct{}

type runTags struct {
	workflowID string
	runID      string
}

// ContextWithRunTags returns ctx carrying a WorkflowID/RunID that Setup's
// handler adds to every record logged with it.
func ContextWithRunTags(ctx context.Context, workflowID, runID string) context.Context {
	return context.WithValue(ctx, runTagsContextKey{}, runTags{workflowID: workflowID, runID: runID})
}

func runTagsFrom(ctx context.Context) (runTags, bool) {
	tags, ok := ctx.Value(runTagsContextKey{}).(runTags)

	return tags, ok
}

// contextHandler adds the run tags from the log context to each record.
type contextHandler struct {
	slog.Handler
}

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

// WithAttrs and WithGroup keep the wrapper so derived loggers retain run tags.
func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{Handler: h.Handler.WithGroup(name)}
}
