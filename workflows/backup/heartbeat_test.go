package backup

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunWithHeartbeatReturnsWorkResult verifies the helper is transparent to
// work's outcome: it returns work's error unchanged (nil or not) and only after
// work has returned.
func TestRunWithHeartbeatReturnsWorkResult(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("work failed")

	tests := []struct {
		name    string
		workErr error
	}{
		{name: "success", workErr: nil},
		{name: "failure", workErr: sentinel},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ran := false

			err := runWithHeartbeat(t.Context(), time.Hour, func() {}, func() error {
				ran = true

				return test.workErr
			})

			assert.True(t, ran, "work must run")
			require.ErrorIs(t, err, test.workErr)
		})
	}
}

// TestRunWithHeartbeatTicks verifies a record is emitted while work is in flight,
// and that no further record lands after work returns. Work blocks until it has
// observed at least one heartbeat, so the assertion needs no fixed sleep.
func TestRunWithHeartbeatTicks(t *testing.T) {
	t.Parallel()

	var records atomic.Int64

	err := runWithHeartbeat(t.Context(), time.Millisecond, func() { records.Add(1) }, func() error {
		for records.Load() == 0 {
			time.Sleep(time.Millisecond)
		}

		return nil
	})
	require.NoError(t, err)

	got := records.Load()
	assert.GreaterOrEqual(t, got, int64(1), "at least one heartbeat must fire while work runs")

	// No record may land after work returned: the ticker must have stopped.
	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, got, records.Load(), "heartbeat must stop once work returns")
}

// TestRunWithHeartbeatDisabledInterval verifies a non-positive interval runs work
// with no heartbeats — the seam that keeps the helper usable without a clock.
func TestRunWithHeartbeatDisabledInterval(t *testing.T) {
	t.Parallel()

	var records atomic.Int64

	err := runWithHeartbeat(t.Context(), 0, func() { records.Add(1) }, func() error { return nil })
	require.NoError(t, err)
	assert.Zero(t, records.Load(), "a non-positive interval must not heartbeat")
}

// TestRunWithHeartbeatContextCancel verifies that when the context is cancelled
// the helper stops ticking and still waits for work to unwind before returning,
// so the work goroutine never outlives the call.
func TestRunWithHeartbeatContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	unwound := make(chan struct{})
	sentinel := errors.New("cancelled")

	go cancel()

	err := runWithHeartbeat(ctx, time.Millisecond, func() {}, func() error {
		// Wait for the cancellation the caller observes, then unwind.
		<-ctx.Done()
		close(unwound)

		return sentinel
	})

	require.ErrorIs(t, err, sentinel)

	select {
	case <-unwound:
	default:
		t.Fatal("runWithHeartbeat returned before work unwound")
	}
}
