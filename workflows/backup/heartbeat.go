package backup

import (
	"context"
	"time"

	"go.temporal.io/sdk/activity"
)

// Long data activities (Prepare, PAR2, Report) run for hours. A liveness
// heartbeat lets them carry a short HeartbeatTimeout so a crashed or restarted
// data worker is detected in minutes instead of after the multi-hour
// StartToCloseTimeout (SPEC §4.1).

const (
	// activityHeartbeatInterval is well below the timeout so a scheduling hiccup
	// does not restart hours of staging work.
	activityHeartbeatInterval = 30 * time.Second

	activityHeartbeatTimeout = 2 * time.Minute
)

// runWithHeartbeat runs work, calling record every interval until it returns.
// On cancellation it stops ticking and waits for work to unwind, so the
// goroutine never outlives the call. A non-positive interval disables the
// ticker.
func runWithHeartbeat(ctx context.Context, interval time.Duration, record func(), work func() error) error {
	done := make(chan error, 1)
	go func() { done <- work() }()

	if interval <= 0 {
		return <-done
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			record()
		case <-ctx.Done():
			return <-done
		}
	}
}

// withActivityHeartbeat runs work under a periodic Temporal heartbeat. It
// detects a dead worker, not an in-process stall: a wedged child process keeps
// the ticker running.
func withActivityHeartbeat(ctx context.Context, work func() error) error {
	return runWithHeartbeat(ctx, activityHeartbeatInterval, func() {
		activity.RecordHeartbeat(ctx)
	}, work)
}
