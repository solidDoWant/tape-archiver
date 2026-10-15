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

// Idle-exit lets a control worker drain and exit after WORKER_IDLE_EXIT_AFTER of
// no work so a KEDA-spawned Job scales to zero (#113; SPEC §4.1).
//
// Only in-flight activities gate the exit. Temporal's workflow interceptor spans
// the whole workflow lifetime, so tracking workflows would pin the worker alive
// while a parked workflow awaits the long data-side write. Respawn replay is
// cheap (<1s, #114), so exiting mid-run is safe.

const idlePollInterval = time.Second

// idleTracker records in-flight activities and the last start/completion time.
type idleTracker struct {
	mu         sync.Mutex
	inFlight   int
	lastActive time.Time
	now        func() time.Time
}

// newIdleTracker returns a tracker seeded as idle-since-now, so a worker that never
// runs a task still exits after the idle window.
func newIdleTracker(now func() time.Time) *idleTracker {
	if now == nil {
		now = time.Now
	}

	return &idleTracker{now: now, lastActive: now()}
}

func (t *idleTracker) begin() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.inFlight++
	t.lastActive = t.now()
}

// end restarts the countdown from completion, so a long task does not force an
// instant exit on return.
func (t *idleTracker) end() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.inFlight--
	t.lastActive = t.now()
}

// state returns the in-flight count and idle duration (zero while busy).
func (t *idleTracker) state() (inFlight int, idleFor time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.inFlight > 0 {
		return t.inFlight, 0
	}

	return 0, t.now().Sub(t.lastActive)
}

// idleInterceptor feeds an idleTracker from activity executions only (see above).
type idleInterceptor struct {
	interceptor.WorkerInterceptorBase
	tracker *idleTracker
}

func newIdleInterceptor(tracker *idleTracker) *idleInterceptor {
	return &idleInterceptor{tracker: tracker}
}

// InterceptActivity wraps each activity so the tracker sees its start and end.
func (i *idleInterceptor) InterceptActivity(
	ctx context.Context,
	next interceptor.ActivityInboundInterceptor,
) interceptor.ActivityInboundInterceptor {
	return &idleActivityInterceptor{
		ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{Next: next},
		tracker:                        i.tracker,
	}
}

type idleActivityInterceptor struct {
	interceptor.ActivityInboundInterceptorBase
	tracker *idleTracker
}

// ExecuteActivity marks the activity in flight while it runs.
func (i *idleActivityInterceptor) ExecuteActivity(
	ctx context.Context,
	in *interceptor.ExecuteActivityInput,
) (interface{}, error) {
	i.tracker.begin()
	defer i.tracker.end()

	return i.ActivityInboundInterceptorBase.ExecuteActivity(ctx, in)
}

// idleMetrics holds the idle-exit gauges; nil when metrics are disabled.
type idleMetrics struct {
	inFlight      prometheus.Gauge
	secondsToExit prometheus.Gauge
}

// newIdleMetrics registers the gauges against reg; a nil reg yields nil.
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

func (m *idleMetrics) observe(inFlight int, secondsUntilExit float64) {
	if m == nil {
		return
	}

	m.inFlight.Set(float64(inFlight))
	m.secondsToExit.Set(secondsUntilExit)
}

// secondsUntilExit is the full window while busy, else the remaining time
// clamped at zero.
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

func shouldExit(inFlight int, idleFor, idleExitAfter time.Duration) bool {
	return inFlight == 0 && idleFor >= idleExitAfter
}

// runIdleExit polls the tracker and calls trigger once the idle window elapses.
// trigger must start the graceful drain, never os.Exit, so in-flight tasks finish.
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

// installIdleExit returns the channel worker.Run should block on: interruptCh
// merged with the idle trigger, so idle-exit shares SIGTERM's drain path. It is a
// no-op for the data role or a zero window. The caller must cancel ctx on return.
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
		// The data worker's lifecycle is fixed (systemd), so the setting is inert.
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
