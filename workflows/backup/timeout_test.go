package backup

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// floorSpeedWriteDuration is the time a full tape of the given native capacity takes
// when streamed at exactly its generation's speed-matching floor — the longest a
// legitimate streaming write can take. The per-tape timeout must never be shorter than
// this, or a healthy floor-speed write is killed mid-tape (#146 AC1).
func floorSpeedWriteDuration(capacityBytes int64, floorMBps float64) time.Duration {
	return time.Duration(float64(capacityBytes) / (floorMBps * bytesPerMB) * float64(time.Second))
}

// TestPerTapeWriteTimeoutCoversFloorSpeedWrite is #146 AC1: a full tape of a supported
// LTO generation, streamed at exactly its documented speed-matching floor, must complete
// without being killed by the per-tape activity timeout — while the ceiling stays
// bounded (not unbounded).
func TestPerTapeWriteTimeoutCoversFloorSpeedWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		capacityBytes int64
		floorMBps     float64
	}{
		{name: "LTO-7", capacityBytes: 6_000_000_000_000, floorMBps: 100},
		{name: "LTO-8", capacityBytes: 12_000_000_000_000, floorMBps: 112},
		{name: "LTO-9", capacityBytes: 18_000_000_000_000, floorMBps: 180},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			floorWrite := floorSpeedWriteDuration(test.capacityBytes, test.floorMBps)
			got := perTapeWriteTimeout(test.capacityBytes)

			// The ceiling must be at least a full floor-speed write, so a healthy
			// streaming write is never killed mid-tape.
			assert.GreaterOrEqual(t, got, floorWrite,
				"per-tape ceiling must cover a full tape written at the generation floor")

			// It equals that floor-speed duration scaled by the safety factor.
			assert.Equal(t, floorWrite*writeTimeoutSafetyFactor, got,
				"per-tape ceiling is the floor-speed write duration times the safety factor")

			// It stays bounded — the issue requires a ceiling, not an unbounded
			// activity. A week is far above any real floor-speed single-tape write.
			assert.Less(t, got, 7*24*time.Hour, "the per-tape ceiling must remain bounded")
		})
	}
}

// TestPerTapeWriteTimeoutUnknownGenerationFallback asserts an unclassifiable or
// non-positive capacity falls back to the fixed, bounded defaultWriteTapeTimeout rather
// than an unbounded activity (#146: timeouts must remain bounded).
func TestPerTapeWriteTimeoutUnknownGenerationFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		capacityBytes int64
	}{
		{name: "below LTO-5", capacityBytes: 100},
		{name: "zero capacity", capacityBytes: 0},
		{name: "negative capacity", capacityBytes: -1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := perTapeWriteTimeout(test.capacityBytes)
			assert.Equal(t, defaultWriteTapeTimeout, got,
				"an unknown/invalid capacity falls back to the bounded default")
			assert.Greater(t, got, time.Duration(0), "the fallback is a real, positive bound")
		})
	}
}

// TestWriteSessionTimeoutLeavesFinalizeHeadroom is #146 AC2: the session ExecutionTimeout
// is strictly greater than the per-tape WriteTree ceiling — by exactly writeSessionHeadroom
// — so a WriteTree that legally runs near its ceiling still leaves room for FinalizeTape's
// index write instead of starving it.
func TestWriteSessionTimeoutLeavesFinalizeHeadroom(t *testing.T) {
	t.Parallel()

	capacities := []struct {
		name          string
		capacityBytes int64
	}{
		{name: "LTO-7", capacityBytes: 6_000_000_000_000},
		{name: "LTO-8", capacityBytes: 12_000_000_000_000},
		{name: "LTO-9", capacityBytes: 18_000_000_000_000},
		{name: "unknown generation", capacityBytes: 100},
	}

	for _, capacity := range capacities {
		t.Run(capacity.name, func(t *testing.T) {
			t.Parallel()

			perTape := perTapeWriteTimeout(capacity.capacityBytes)
			session := writeSessionTimeout(capacity.capacityBytes)

			require.Greater(t, session, perTape,
				"the session must outlast a full-length per-tape write so finalize is not starved")
			assert.Equal(t, writeSessionHeadroom, session-perTape,
				"the session exceeds the per-tape ceiling by exactly the finalize headroom")
			assert.Positive(t, writeSessionHeadroom, "the headroom must be a real, positive budget")
		})
	}
}
