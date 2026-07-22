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

// activityLogInterceptor emits a structured line at the start and end of every
// activity execution, on both worker roles. The start line stamps the
// tape-archiver build version and the worker identity onto the activity; the end
// line reports its outcome and duration. This answers "which binary produced this
// log, and how long did the step take?" for every activity without threading the
// version/worker through each call site — a log line that predates a deploy can be
// told apart from one that follows it by its version stamp alone.
//
// It must be registered AFTER logTagsInterceptor in worker.Options.Interceptors
// so it runs *inside* it: logTagsInterceptor seeds the context with the run's
// WorkflowID/RunID (#303), and these boundary lines are logged with that seeded
// context so they carry the same tags and surface in the web UI's run-detail log
// panel alongside the activity's own output.
type activityLogInterceptor struct {
	interceptor.WorkerInterceptorBase

	// worker identifies this worker process (role@host/pid). Computed once at
	// construction rather than per-activity because it never changes.
	worker string
	// version is buildinfo.ToolVersion(), captured once (it is constant for the
	// process) so the hot path does not re-read the build info per activity.
	version string
}

// newActivityLogInterceptor builds the interceptor, capturing the worker identity
// and build version once. The worker identity mirrors the shape of Temporal's
// default client identity (pid@host) but leads with the role so control- and
// data-worker lines are distinguishable at a glance.
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

// InterceptActivity wraps each activity execution with start/end logging.
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

// activityLogActivityInterceptor logs the boundaries of a single activity
// execution.
type activityLogActivityInterceptor struct {
	interceptor.ActivityInboundInterceptorBase

	worker  string
	version string
}

// ExecuteActivity logs one "activity started" line before the activity runs and
// one "activity finished"/"activity failed" line after. The end line is deferred
// so it fires on every exit path (success, error, or panic) and reports the
// wall-clock duration. Attempt is included on every line so retries are visible.
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
