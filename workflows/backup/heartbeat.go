package backup

import (
	"context"
	"time"

	"go.temporal.io/sdk/activity"
)

// The long-running data activities (Prepare, Generate PAR2, Report) run for
// minutes to hours over terabytes of staged data. Without a heartbeat Temporal
// only learns an attempt is dead when its multi-hour StartToCloseTimeout elapses,
// so a data worker that crashes or restarts mid-activity strands the run for up
// to a day. A periodic liveness heartbeat lets those activities carry a short
// HeartbeatTimeout instead: if the worker stops heartbeating, Temporal fails and
// reschedules the attempt within that window (SPEC §4.1 — the data queue runs on
// the storage host, which can restart independently of the control plane).

const (
	// activityHeartbeatInterval is how often the long data activities emit a
	// liveness heartbeat while they work. It is well below activityHeartbeatTimeout
	// so a brief scheduling hiccup on a busy worker does not trip a spurious
	// timeout that would restart hours of staging work.
	activityHeartbeatInterval = 30 * time.Second

	// activityHeartbeatTimeout is the HeartbeatTimeout set on the long data
	// activities. Temporal fails an attempt when no heartbeat arrives within this
	// window; the interval above gives four heartbeats of margin.
	activityHeartbeatTimeout = 2 * time.Minute
)

// runWithHeartbeat runs work while calling record every interval until work
// returns, then returns work's error. record is a liveness signal only — it
// carries no progress payload. The heartbeat goroutine never outlives work: on
// context cancellation runWithHeartbeat stops ticking and waits for work to
// unwind before returning, so it neither leaks the goroutine nor returns ahead of
// the cancelled work. A non-positive interval disables the ticker (work runs
// without heartbeats), keeping the helper usable from tests without a clock.
//
// record and work are injected so the mechanism is unit-testable without an
// activity context; withActivityHeartbeat wires the production Temporal heartbeat.
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
			// The activity context was cancelled (timeout, worker shutdown, or a
			// workflow cancel). Stop heartbeating and wait for work to observe the
			// cancellation and return so the goroutine never outlives this call.
			return <-done
		}
	}
}

// withActivityHeartbeat runs work under a periodic Temporal liveness heartbeat,
// the mechanism that lets the long data activities carry activityHeartbeatTimeout.
//
// The heartbeat is a liveness signal, not a progress signal: it detects a dead or
// unresponsive worker, not an in-process stall. A wedged child process or a
// blocked disk read leaves this ticker running, so it is not caught here —
// detecting that needs progress-tied heartbeats keyed to real throughput, a
// deliberate follow-up rather than part of this change.
func withActivityHeartbeat(ctx context.Context, work func() error) error {
	return runWithHeartbeat(ctx, activityHeartbeatInterval, func() {
		activity.RecordHeartbeat(ctx)
	}, work)
}
