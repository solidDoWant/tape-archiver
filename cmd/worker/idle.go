package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/worker"
)

// Idle-exit lets a control worker drain and exit once it has done no work for a
// configured window (WORKER_IDLE_EXIT_AFTER), so a KEDA-spawned Job can scale back
// to zero (parent #113; SPEC §4.1). It is control-role only: the data worker runs
// under systemd on the storage host with a fixed lifecycle and is unaffected.
//
// Only in-flight *activity* executions gate the exit. A workflow parked awaiting a
// data-queue activity must read as idle so the control worker can scale to zero
// while the long data-side write runs — and Temporal's workflow-execution
// interceptor spans the entire workflow lifetime (it blocks on the workflow
// coroutine across every workflow task until the workflow function returns), so
// tracking it would pin the worker alive for the whole run and defeat scale-to-zero.
// Respawn replay is cheap (<1s, measured in #114), so exiting mid-run is safe.

// idlePollInterval is how often the idle-exit loop re-evaluates the tracker and
// refreshes the idle metrics. It is well below any realistic idle window (default
// 15m), so the exit fires within a second of the window elapsing.
const idlePollInterval = time.Second

// idleTracker records the worker's in-flight activity count and the timestamp of
// the last task boundary (start or completion). It is safe for concurrent use by
// the interceptor (activity goroutines) and the idle poll loop. now is injectable
// so tests can drive the clock deterministically; it defaults to time.Now.
type idleTracker struct {
	mu         sync.Mutex
	inFlight   int
	lastActive time.Time
	now        func() time.Time
}

// newIdleTracker returns a tracker seeded as idle-since-now, so a worker that never
// runs a task still exits after the idle window. A nil now defaults to time.Now.
func newIdleTracker(now func() time.Time) *idleTracker {
	if now == nil {
		now = time.Now
	}

	return &idleTracker{now: now, lastActive: now()}
}

// begin records the start of an activity task: it increments the in-flight count
// and bumps the last-active timestamp.
func (t *idleTracker) begin() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.inFlight++
	t.lastActive = t.now()
}

// end records the completion of an activity task: it decrements the in-flight count
// and bumps the last-active timestamp, so the idle countdown restarts from the
// completion time (not the start) — a long task does not force an instant exit on
// return.
func (t *idleTracker) end() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.inFlight--
	t.lastActive = t.now()
}

// state returns the current in-flight count and how long the worker has been idle.
// While any task is in flight the worker is not idle, so idleFor is zero.
func (t *idleTracker) state() (inFlight int, idleFor time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.inFlight > 0 {
		return t.inFlight, 0
	}

	return 0, t.now().Sub(t.lastActive)
}

// idleInterceptor is a Temporal WorkerInterceptor that feeds an idleTracker from
// activity task execution. It tracks activities only, deliberately — see the
// package-level note above.
type idleInterceptor struct {
	interceptor.WorkerInterceptorBase
	tracker *idleTracker
}

// newIdleInterceptor returns an interceptor feeding tracker.
func newIdleInterceptor(tracker *idleTracker) *idleInterceptor {
	return &idleInterceptor{tracker: tracker}
}

// InterceptActivity wraps each activity execution so the tracker sees its start and
// end.
func (i *idleInterceptor) InterceptActivity(
	ctx context.Context,
	next interceptor.ActivityInboundInterceptor,
) interceptor.ActivityInboundInterceptor {
	return &idleActivityInterceptor{
		ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{Next: next},
		tracker:                        i.tracker,
	}
}

// idleActivityInterceptor bumps the tracker around a single activity execution.
type idleActivityInterceptor struct {
	interceptor.ActivityInboundInterceptorBase
	tracker *idleTracker
}

// ExecuteActivity marks the activity in-flight for the duration of its execution so
// the worker never idle-exits while an activity is running, and restarts the idle
// countdown when it completes.
func (i *idleActivityInterceptor) ExecuteActivity(
	ctx context.Context,
	in *interceptor.ExecuteActivityInput,
) (interface{}, error) {
	i.tracker.begin()
	defer i.tracker.end()

	return i.ActivityInboundInterceptorBase.ExecuteActivity(ctx, in)
}

// idleMetrics holds the Prometheus gauges that make idle-exit observable. It is nil
// when metrics are disabled (no registry), in which case observe is a no-op.
type idleMetrics struct {
	inFlight      prometheus.Gauge
	secondsToExit prometheus.Gauge
}

// newIdleMetrics registers the idle-exit gauges against reg. A nil reg (metrics
// disabled) yields a nil *idleMetrics. A registration error is returned so worker
// startup fails loudly rather than running without the promised observability.
func newIdleMetrics(reg prometheus.Registerer) (*idleMetrics, error) {
	if reg == nil {
		return nil, nil
	}

	metrics := &idleMetrics{
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "tape_archiver",
			Subsystem: "worker",
			Name:      "idle_in_flight_tasks",
			Help:      "Activity tasks currently executing on this worker; the idle-exit countdown only advances while this is zero.",
		}),
		secondsToExit: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "tape_archiver",
			Subsystem: "worker",
			Name:      "idle_seconds_until_exit",
			Help:      "Seconds until the worker self-exits on idle: the full idle window while any task is in flight, counting down to zero once idle.",
		}),
	}

	for _, collector := range []prometheus.Collector{metrics.inFlight, metrics.secondsToExit} {
		if err := reg.Register(collector); err != nil {
			return nil, fmt.Errorf("register idle-exit metric: %w", err)
		}
	}

	return metrics, nil
}

// observe records the current in-flight count and countdown. It is a no-op on a nil
// receiver (metrics disabled).
func (m *idleMetrics) observe(inFlight int, secondsUntilExit float64) {
	if m == nil {
		return
	}

	m.inFlight.Set(float64(inFlight))
	m.secondsToExit.Set(secondsUntilExit)
}

// secondsUntilExit is the countdown exported to Prometheus: the full idle window
// while any task is in flight, otherwise the time remaining before exit, clamped at
// zero.
func secondsUntilExit(inFlight int, idleFor, idleExitAfter time.Duration) float64 {
	remaining := idleExitAfter
	if inFlight == 0 {
		remaining = idleExitAfter - idleFor
	}

	if remaining < 0 {
		remaining = 0
	}

	return remaining.Seconds()
}

// shouldExit reports whether the idle window has fully elapsed with no task in
// flight.
func shouldExit(inFlight int, idleFor, idleExitAfter time.Duration) bool {
	return inFlight == 0 && idleFor >= idleExitAfter
}

// runIdleExit polls the tracker every pollInterval, refreshes the idle metrics, and
// calls trigger exactly once when the worker has been idle for idleExitAfter. It
// returns after triggering or when ctx is cancelled (e.g. the worker stopped for
// another reason). trigger must initiate the graceful drain — never os.Exit — so
// in-flight tasks finish before the process exits (see run in main.go).
func runIdleExit(
	ctx context.Context,
	tracker *idleTracker,
	idleExitAfter, pollInterval time.Duration,
	metrics *idleMetrics,
	trigger func(),
) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			inFlight, idleFor := tracker.state()
			metrics.observe(inFlight, secondsUntilExit(inFlight, idleFor, idleExitAfter))

			if shouldExit(inFlight, idleFor, idleExitAfter) {
				slog.Info("worker idle window elapsed; draining and exiting",
					"idle_exit_after", idleExitAfter.String())
				trigger()

				return
			}
		}
	}
}

// installIdleExit wires idle-exit into the worker for a control worker with a
// positive idle window, returning the interrupt channel worker.Run should block on:
// the OS interrupt merged with the idle trigger, so idle-exit drives the identical
// graceful-drain path as SIGTERM. For the data role or a zero window it is a no-op —
// options is untouched and the original interruptCh is returned unchanged. ctx
// bounds the background idle loop; the caller must cancel it when run returns.
func installIdleExit(
	ctx context.Context,
	cfg Config,
	options *worker.Options,
	reg prometheus.Registerer,
	interruptCh <-chan interface{},
) (<-chan interface{}, error) {
	if cfg.IdleExitAfter <= 0 {
		return interruptCh, nil
	}

	if cfg.Role != RoleControl {
		// Idle-exit is a control-worker feature (KEDA scale-to-zero). The data
		// worker's lifecycle is fixed, so the setting is inert there rather than
		// silently changing its behavior.
		slog.Warn("WORKER_IDLE_EXIT_AFTER is set but ignored for the data role",
			"idle_exit_after", cfg.IdleExitAfter.String())

		return interruptCh, nil
	}

	metrics, err := newIdleMetrics(reg)
	if err != nil {
		return nil, fmt.Errorf("set up idle-exit metrics: %w", err)
	}

	tracker := newIdleTracker(nil)
	options.Interceptors = append(options.Interceptors, newIdleInterceptor(tracker))

	// The idle loop and an OS interrupt both close the same drain channel handed to
	// worker.Run, giving idle-exit and SIGTERM one shared graceful-drain path.
	drain := make(chan interface{})
	trigger := sync.OnceFunc(func() { close(drain) })

	go func() {
		select {
		case <-interruptCh:
			trigger()
		case <-ctx.Done():
		}
	}()

	go runIdleExit(ctx, tracker, cfg.IdleExitAfter, idlePollInterval, metrics, trigger)

	slog.Info("idle-exit enabled", "idle_exit_after", cfg.IdleExitAfter.String())

	return drain, nil
}
