package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"

	"github.com/solidDoWant/tape-archiver/internal/buildinfo"
)

// activityLogInterceptor logs each activity's start (with build version and
// worker identity) and end (outcome, duration).
//
// It must be registered after logTagsInterceptor so it runs inside it and its
// lines carry the run tags (#303).
type activityLogInterceptor struct {
	interceptor.WorkerInterceptorBase

	// worker is role@host/pid.
	worker string
	// version is captured once since it is constant for the process.
	version string
}

func newActivityLogInterceptor(role Role) *activityLogInterceptor {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown-host"
	}

	return &activityLogInterceptor{
		worker:  fmt.Sprintf("%s@%s/%d", role, hostname, os.Getpid()),
		version: buildinfo.ToolVersion(),
	}
}

// InterceptActivity wraps each activity with start/end logging.
func (w *activityLogInterceptor) InterceptActivity(
	ctx context.Context,
	next interceptor.ActivityInboundInterceptor,
) interceptor.ActivityInboundInterceptor {
	return &activityLogActivityInterceptor{
		ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{Next: next},
		worker:                         w.worker,
		version:                        w.version,
	}
}

type activityLogActivityInterceptor struct {
	interceptor.ActivityInboundInterceptorBase

	worker  string
	version string
}

// ExecuteActivity logs around the activity; the end line is deferred so it fires
// on every exit path.
func (i *activityLogActivityInterceptor) ExecuteActivity(
	ctx context.Context,
	in *interceptor.ExecuteActivityInput,
) (result interface{}, err error) {
	info := activity.GetInfo(ctx)
	name := info.ActivityType.Name

	slog.InfoContext(ctx, "activity started",
		"activity", name,
		"attempt", info.Attempt,
		"worker", i.worker,
		"taskQueue", info.TaskQueue,
		"version", i.version,
	)

	start := time.Now()

	defer func() {
		durationMS := time.Since(start).Milliseconds()

		if err != nil {
			slog.ErrorContext(ctx, "activity failed",
				"activity", name,
				"attempt", info.Attempt,
				"durationMs", durationMS,
				"worker", i.worker,
				"version", i.version,
				"error", err.Error(),
			)

			return
		}

		slog.InfoContext(ctx, "activity finished",
			"activity", name,
			"attempt", info.Attempt,
			"durationMs", durationMS,
		)
	}()

	return i.ActivityInboundInterceptorBase.ExecuteActivity(ctx, in)
}
