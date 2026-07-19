package main

import (
	"context"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"

	"github.com/solidDoWant/tape-archiver/pkg/logging"
)

// Run-log attribution: the web UI's run-detail log panel filters VictoriaLogs by
// RunID (pkg/runsapi/logs.go), but only lines emitted through the Temporal SDK's
// context loggers carry that tag — all bulk activity/helper logging goes through
// the process-global slog default, which knows nothing about the run. Successful
// runs trip none of the SDK error-path loggers, so every one of their lines lands
// untagged and the panel shows its empty state (#303).
//
// logTagsInterceptor closes that gap without threading a logger through every
// call site: it seeds the activity's context with the run identity so that
// pkg/logging's contextHandler lifts WorkflowID/RunID onto every record logged
// with that context (i.e. every slog.*Context call in the activity and the pkg/*
// helpers it calls). It is registered for both worker roles.
type logTagsInterceptor struct {
	interceptor.WorkerInterceptorBase
}

// InterceptActivity wraps each activity execution so its context carries the run
// identity for the duration of the call.
func (logTagsInterceptor) InterceptActivity(
	ctx context.Context,
	next interceptor.ActivityInboundInterceptor,
) interceptor.ActivityInboundInterceptor {
	return &logTagsActivityInterceptor{
		ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{Next: next},
	}
}

// logTagsActivityInterceptor seeds the run identity onto the context of a single
// activity execution.
type logTagsActivityInterceptor struct {
	interceptor.ActivityInboundInterceptorBase
}

// ExecuteActivity reads the run identity from the activity info and stashes it on
// the context passed to the activity, so downstream slog.*Context logging is
// tagged with the same WorkflowID/RunID the SDK's own context loggers use. The
// values match workflow.GetLogger's tags exactly (WorkflowExecution.ID/.RunID),
// so the web UI's existing RunID filter matches these lines.
func (i *logTagsActivityInterceptor) ExecuteActivity(
	ctx context.Context,
	in *interceptor.ExecuteActivityInput,
) (interface{}, error) {
	info := activity.GetInfo(ctx)
	ctx = logging.ContextWithRunTags(ctx, info.WorkflowExecution.ID, info.WorkflowExecution.RunID)

	return i.ActivityInboundInterceptorBase.ExecuteActivity(ctx, in)
}
