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

// manualClock advances only when the test calls advance.
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

// stubActivityInbound runs onExecute as the activity execution.
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

func TestIdleTrackerBumpsOnStartAndEnd(t *testing.T) {
	clock := newManualClock()
	tracker := newIdleTracker(clock.now)

	inFlight, idleFor := tracker.state()
	assert.Equal(t, 0, inFlight)
	assert.Zero(t, idleFor)

	clock.advance(10 * time.Minute)

	inFlight, idleFor = tracker.state()
	assert.Equal(t, 0, inFlight)
	assert.Equal(t, 10*time.Minute, idleFor)

	tracker.begin()
	clock.advance(time.Hour)

	inFlight, idleFor = tracker.state()
	assert.Equal(t, 1, inFlight)
	assert.Zero(t, idleFor)

	// The countdown restarts from completion, not start.
	tracker.end()

	inFlight, idleFor = tracker.state()
	assert.Equal(t, 0, inFlight)
	assert.Zero(t, idleFor)

	clock.advance(5 * time.Minute)

	_, idleFor = tracker.state()
	assert.Equal(t, 5*time.Minute, idleFor)
}

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

func TestRunIdleExitWaitsForInFlightActivity(t *testing.T) {
	tracker := newIdleTracker(nil)
	tracker.begin()

	triggered := make(chan struct{})

	go runIdleExit(t.Context(), tracker, 20*time.Millisecond, 5*time.Millisecond, nil, func() {
		close(triggered)
	})

	select {
	case <-triggered:
		t.Fatal("idle-exit fired while an activity was in flight")
	case <-time.After(150 * time.Millisecond):
	}

	tracker.end()

	select {
	case <-triggered:
	case <-time.After(2 * time.Second):
		t.Fatal("idle-exit did not fire after the activity completed")
	}
}

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

	disabled, err := newIdleMetrics(nil)
	require.NoError(t, err)
	assert.Nil(t, disabled)
	assert.NotPanics(t, func() { disabled.observe(1, 1) })
}

func TestInstallIdleExitDisabled(t *testing.T) {
	var interruptCh <-chan interface{} = make(chan interface{})

	options := worker.Options{}

	out, err := installIdleExit(t.Context(), Config{Role: RoleControl, IdleExitAfter: 0}, &options, nil, interruptCh)
	require.NoError(t, err)
	assert.Equal(t, interruptCh, out)
	assert.Empty(t, options.Interceptors)
}

func TestInstallIdleExitInertForDataRole(t *testing.T) {
	var interruptCh <-chan interface{} = make(chan interface{})

	options := worker.Options{}

	out, err := installIdleExit(t.Context(), Config{Role: RoleData, IdleExitAfter: time.Minute}, &options, nil, interruptCh)
	require.NoError(t, err)
	assert.Equal(t, interruptCh, out)
	assert.Empty(t, options.Interceptors)
}

func TestInstallIdleExitForwardsOSInterrupt(t *testing.T) {
	interruptCh := make(chan interface{}, 1)
	options := worker.Options{}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// A long window ensures only the forwarded interrupt closes the channel.
	out, err := installIdleExit(ctx, Config{Role: RoleControl, IdleExitAfter: time.Hour}, &options, nil, interruptCh)
	require.NoError(t, err)
	require.Len(t, options.Interceptors, 1)

	interruptCh <- struct{}{}

	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("OS interrupt did not drive the merged drain channel")
	}
}

func TestInstallIdleExitFiresWhenIdle(t *testing.T) {
	var interruptCh <-chan interface{} = make(chan interface{})

	options := worker.Options{}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	out, err := installIdleExit(ctx, Config{Role: RoleControl, IdleExitAfter: time.Millisecond}, &options, nil, interruptCh)
	require.NoError(t, err)
	require.Len(t, options.Interceptors, 1)

	select {
	case <-out:
	case <-time.After(3 * time.Second):
		t.Fatal("idle-exit did not drive the merged drain channel")
	}
}
