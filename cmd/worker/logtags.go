package main

import (
	"context"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"

	"github.com/solidDoWant/tape-archiver/pkg/logging"
)

// logTagsInterceptor seeds each activity's context with WorkflowID/RunID so
// pkg/logging tags every slog.*Context record. Without it, activity logging via
// the global slog default is untagged and the web UI's run log panel, which
// filters by RunID, shows nothing for successful runs (#303).
type logTagsInterceptor struct {
	interceptor.WorkerInterceptorBase
}

// InterceptActivity wraps each activity so its context carries the run identity.
func (logTagsInterceptor) InterceptActivity(
	ctx context.Context,
	next interceptor.ActivityInboundInterceptor,
) interceptor.ActivityInboundInterceptor {
	return &logTagsActivityInterceptor{
		ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{Next: next},
	}
}

type logTagsActivityInterceptor struct {
	interceptor.ActivityInboundInterceptorBase
}

// ExecuteActivity tags ctx with the same IDs workflow.GetLogger uses, so the web
// UI's RunID filter matches.
func (i *logTagsActivityInterceptor) ExecuteActivity(
	ctx context.Context,
	in *interceptor.ExecuteActivityInput,
) (interface{}, error) {
	info := activity.GetInfo(ctx)
	ctx = logging.ContextWithRunTags(ctx, info.WorkflowExecution.ID, info.WorkflowExecution.RunID)

	return i.ActivityInboundInterceptorBase.ExecuteActivity(ctx, in)
}
