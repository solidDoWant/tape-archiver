package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/worker"
)

// manualClock is a deterministic clock for tracker tests: time advances only when
// the test calls advance, so idle-duration assertions do not depend on wall time.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func newManualClock() *manualClock {
	return &manualClock{t: time.Unix(1_700_000_000, 0)}
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t = c.t.Add(d)
}

// stubActivityInbound is the next interceptor in the chain: it runs a supplied
// function as the activity execution so tests can observe the tracker state while
// the activity is "in flight".
type stubActivityInbound struct {
	interceptor.ActivityInboundInterceptorBase
	onExecute func(context.Context, *interceptor.ExecuteActivityInput) (interface{}, error)
}

func (s *stubActivityInbound) ExecuteActivity(
	ctx context.Context,
	in *interceptor.ExecuteActivityInput,
) (interface{}, error) {
	return s.onExecute(ctx, in)
}

// TestIdleTrackerBumpsOnStartAndEnd covers the tracker's core semantics: an
// in-flight task reads as not-idle, and the idle countdown restarts from a task's
// completion (bumped on end, not only start), so a long task does not force an
// instant exit on return.
func TestIdleTrackerBumpsOnStartAndEnd(t *testing.T) {
	clock := newManualClock()
	tracker := newIdleTracker(clock.now)

	// Freshly seeded: idle since "now", so no elapsed idle time yet.
	inFlight, idleFor := tracker.state()
	assert.Equal(t, 0, inFlight)
	assert.Zero(t, idleFor)

	// Time passes with no work: idle duration accrues.
	clock.advance(10 * time.Minute)

	inFlight, idleFor = tracker.state()
	assert.Equal(t, 0, inFlight)
	assert.Equal(t, 10*time.Minute, idleFor)

	// A task starts: in-flight, and therefore never idle regardless of time.
	tracker.begin()
	clock.advance(time.Hour)

	inFlight, idleFor = tracker.state()
	assert.Equal(t, 1, inFlight)
	assert.Zero(t, idleFor)

	// The task completes: the countdown restarts from the completion time, so a
	// long task does not read as already-idle for its whole duration.
	tracker.end()

	inFlight, idleFor = tracker.state()
	assert.Equal(t, 0, inFlight)
	assert.Zero(t, idleFor)

	clock.advance(5 * time.Minute)

	_, idleFor = tracker.state()
	assert.Equal(t, 5*time.Minute, idleFor)
}

// TestIdleInterceptorTracksActivityExecution asserts the interceptor marks a task
// in-flight for the duration of its execution and restores the count afterwards, so
// the worker cannot idle-exit mid-activity.
func TestIdleInterceptorTracksActivityExecution(t *testing.T) {
	tracker := newIdleTracker(newManualClock().now)

	var inFlightDuringCall int

	next := &stubActivityInbound{
		onExecute: func(context.Context, *interceptor.ExecuteActivityInput) (interface{}, error) {
			inFlightDuringCall, _ = tracker.state()

			return "ok", nil
		},
	}

	inbound := newIdleInterceptor(tracker).InterceptActivity(t.Context(), next)

	result, err := inbound.ExecuteActivity(t.Context(), &interceptor.ExecuteActivityInput{})
	require.NoError(t, err)
	assert.Equal(t, "ok", result)

	// In-flight during the activity, back to zero once it returns.
	assert.Equal(t, 1, inFlightDuringCall)

	after, _ := tracker.state()
	assert.Equal(t, 0, after)
}

func TestShouldExit(t *testing.T) {
	const window = 15 * time.Minute

	tests := []struct {
		name     string
		inFlight int
		idleFor  time.Duration
		expected bool
	}{
		{name: "idle past the window exits", inFlight: 0, idleFor: 16 * time.Minute, expected: true},
		{name: "idle exactly at the window exits", inFlight: 0, idleFor: window, expected: true},
		{name: "idle within the window waits", inFlight: 0, idleFor: 5 * time.Minute, expected: false},
		{name: "in-flight never exits", inFlight: 1, idleFor: time.Hour, expected: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.expected, shouldExit(test.inFlight, test.idleFor, window))
		})
	}
}

func TestSecondsUntilExit(t *testing.T) {
	const window = 15 * time.Minute

	tests := []struct {
		name     string
		inFlight int
		idleFor  time.Duration
		expected float64
	}{
		{name: "freshly idle shows the full window", inFlight: 0, idleFor: 0, expected: 900},
		{name: "counts down as idle time accrues", inFlight: 0, idleFor: 5 * time.Minute, expected: 600},
		{name: "clamps at zero once past the window", inFlight: 0, idleFor: 20 * time.Minute, expected: 0},
		{name: "in-flight shows the full window", inFlight: 3, idleFor: 0, expected: 900},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.InDelta(t, test.expected, secondsUntilExit(test.inFlight, test.idleFor, window), 0.001)
		})
	}
}

// TestRunIdleExitFiresWhenIdle asserts the poll loop triggers the drain once the
// worker has been idle for the window, and refreshes the in-flight metric.
func TestRunIdleExitFiresWhenIdle(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics, err := newIdleMetrics(reg)
	require.NoError(t, err)

	tracker := newIdleTracker(nil)

	triggered := make(chan struct{})

	go runIdleExit(t.Context(), tracker, 20*time.Millisecond, 5*time.Millisecond, metrics, func() {
		close(triggered)
	})

	select {
	case <-triggered:
	case <-time.After(2 * time.Second):
		t.Fatal("idle-exit did not fire for an idle worker")
	}

	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.inFlight))
}

// TestRunIdleExitWaitsForInFlightActivity asserts the poll loop does not exit while
// an activity is in flight — no mid-activity termination — and fires once it ends.
func TestRunIdleExitWaitsForInFlightActivity(t *testing.T) {
	tracker := newIdleTracker(nil)
	tracker.begin() // an activity is running

	triggered := make(chan struct{})

	go runIdleExit(t.Context(), tracker, 20*time.Millisecond, 5*time.Millisecond, nil, func() {
		close(triggered)
	})

	// While in-flight the exit predicate is false regardless of elapsed time, so it
	// must not fire.
	select {
	case <-triggered:
		t.Fatal("idle-exit fired while an activity was in flight")
	case <-time.After(150 * time.Millisecond):
	}

	// Once the activity completes, the window elapses and the loop fires.
	tracker.end()

	select {
	case <-triggered:
	case <-time.After(2 * time.Second):
		t.Fatal("idle-exit did not fire after the activity completed")
	}
}

// TestRunIdleExitReturnsOnContextCancel asserts the loop stops without triggering
// when its context is cancelled (e.g. the worker stopped via SIGTERM instead).
func TestRunIdleExitReturnsOnContextCancel(t *testing.T) {
	tracker := newIdleTracker(nil)

	ctx, cancel := context.WithCancel(t.Context())

	triggered := make(chan struct{})
	returned := make(chan struct{})

	go func() {
		runIdleExit(ctx, tracker, time.Hour, 5*time.Millisecond, nil, func() { close(triggered) })
		close(returned)
	}()

	cancel()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("idle loop did not return after context cancel")
	}

	select {
	case <-triggered:
		t.Fatal("idle-exit fired despite a long window and cancelled context")
	default:
	}
}

func TestIdleMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics, err := newIdleMetrics(reg)
	require.NoError(t, err)
	require.NotNil(t, metrics)

	metrics.observe(2, 900)
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.inFlight))
	assert.Equal(t, 900.0, testutil.ToFloat64(metrics.secondsToExit))

	// A nil registry (metrics disabled) yields nil metrics whose observe is a safe
	// no-op.
	disabled, err := newIdleMetrics(nil)
	require.NoError(t, err)
	assert.Nil(t, disabled)
	assert.NotPanics(t, func() { disabled.observe(1, 1) })
}

// TestInstallIdleExitDisabled asserts idle-exit is not wired when the window is zero
// (the default): the interrupt channel is returned unchanged and no interceptor is
// added, so behavior is exactly as before.
func TestInstallIdleExitDisabled(t *testing.T) {
	var interruptCh <-chan interface{} = make(chan interface{})

	options := worker.Options{}

	out, err := installIdleExit(t.Context(), Config{Role: RoleControl, IdleExitAfter: 0}, &options, nil, interruptCh)
	require.NoError(t, err)
	assert.Equal(t, interruptCh, out)
	assert.Empty(t, options.Interceptors)
}

// TestInstallIdleExitInertForDataRole asserts a data worker ignores the setting: no
// interceptor is installed and the interrupt channel is unchanged, so the data
// worker lifecycle is unaffected.
func TestInstallIdleExitInertForDataRole(t *testing.T) {
	var interruptCh <-chan interface{} = make(chan interface{})

	options := worker.Options{}

	out, err := installIdleExit(t.Context(), Config{Role: RoleData, IdleExitAfter: time.Minute}, &options, nil, interruptCh)
	require.NoError(t, err)
	assert.Equal(t, interruptCh, out)
	assert.Empty(t, options.Interceptors)
}

// TestInstallIdleExitForwardsOSInterrupt asserts the merged channel handed to
// worker.Run drains when an OS interrupt arrives (the SIGTERM path), independent of
// the idle timer.
func TestInstallIdleExitForwardsOSInterrupt(t *testing.T) {
	interruptCh := make(chan interface{}, 1)
	options := worker.Options{}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// A long window ensures only the forwarded interrupt — not the idle timer —
	// closes the returned channel.
	out, err := installIdleExit(ctx, Config{Role: RoleControl, IdleExitAfter: time.Hour}, &options, nil, interruptCh)
	require.NoError(t, err)
	require.Len(t, options.Interceptors, 1)

	interruptCh <- struct{}{} // simulate an OS signal delivery

	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("OS interrupt did not drive the merged drain channel")
	}
}

// TestInstallIdleExitFiresWhenIdle asserts the fully wired control-worker path
// closes the merged drain channel once the idle window elapses (AC1 through the real
// wiring), installing exactly one interceptor.
func TestInstallIdleExitFiresWhenIdle(t *testing.T) {
	var interruptCh <-chan interface{} = make(chan interface{})

	options := worker.Options{}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	out, err := installIdleExit(ctx, Config{Role: RoleControl, IdleExitAfter: time.Millisecond}, &options, nil, interruptCh)
	require.NoError(t, err)
	require.Len(t, options.Interceptors, 1)

	// idlePollInterval is one second, so the first tick fires shortly after the
	// (already-elapsed) millisecond window.
	select {
	case <-out:
	case <-time.After(3 * time.Second):
		t.Fatal("idle-exit did not drive the merged drain channel")
	}
}
