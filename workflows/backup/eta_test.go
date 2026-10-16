package backup

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// etaPhase is a stretch of constant-rate progress, sampled every heartbeat.
type etaPhase struct {
	duration       time.Duration
	bytesPerSecond int64
}

// runETAPhases drives estimator through phases on an injected clock, sampling
// every activityHeartbeatInterval like the trackers do, and returns each
// sample's ETA plus the final done bytes.
func runETAPhases(estimator *etaEstimator, start time.Time, total int64, phases []etaPhase) ([]time.Duration, int64) {
	var (
		etas []time.Duration
		done int64
	)

	now := start

	for _, phase := range phases {
		for elapsed := time.Duration(0); elapsed < phase.duration; elapsed += activityHeartbeatInterval {
			now = now.Add(activityHeartbeatInterval)
			done += phase.bytesPerSecond * int64(activityHeartbeatInterval/time.Second)
			etas = append(etas, estimator.estimate(now, done, total))
		}
	}

	return etas, done
}

func TestETAEstimator(t *testing.T) {
	t.Parallel()

	const total = 100_000

	tests := []struct {
		name   string
		phases []etaPhase
		want   time.Duration
	}{
		{
			name: "no samples yet",
			want: 0,
		},
		{
			name:   "steady rate",
			phases: []etaPhase{{10 * time.Minute, 10}},
			// 94,000 bytes left at 10 B/s.
			want: 9400 * time.Second,
		},
		{
			name:   "rate drop is fully reflected after one window",
			phases: []etaPhase{{10 * time.Minute, 10}, {etaWindow, 1}},
			// 93,700 bytes left at the new 1 B/s; the run-wide average (7 B/s)
			// would claim ~3h43m.
			want: 93_700 * time.Second,
		},
		{
			name:   "rate increase is fully reflected after one window",
			phases: []etaPhase{{10 * time.Minute, 1}, {etaWindow, 10}},
			// 96,400 bytes left at the new 10 B/s.
			want: 9640 * time.Second,
		},
		{
			name:   "stall across the whole window reports no ETA",
			phases: []etaPhase{{10 * time.Minute, 10}, {etaWindow, 0}},
			want:   0,
		},
		{
			name:   "complete reports no ETA",
			phases: []etaPhase{{1000 * time.Second, 100}},
			want:   0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			start := time.Unix(0, 0)
			estimator := newETAEstimator(start, etaWindow)

			etas, done := runETAPhases(estimator, start, total, test.phases)

			assert.Equal(t, test.want, estimator.estimate(start.Add(time.Duration(len(etas))*activityHeartbeatInterval), done, total))
		})
	}
}

// TestETAEstimatorRateDropGrows pins the #427 regression: after a slowdown the
// ETA must climb toward the new rate's projection rather than stay pinned near
// the run-wide average.
func TestETAEstimatorRateDropGrows(t *testing.T) {
	t.Parallel()

	const total = 100_000

	start := time.Unix(0, 0)
	estimator := newETAEstimator(start, etaWindow)

	fastPhase := etaPhase{10 * time.Minute, 10}

	etas, _ := runETAPhases(estimator, start, total, []etaPhase{fastPhase, {etaWindow, 1}})

	fastSamples := int(fastPhase.duration / activityHeartbeatInterval)
	require.Greater(t, len(etas), fastSamples)

	fast, slow := etas[:fastSamples], etas[fastSamples:]

	previous := fast[len(fast)-1]
	for index, eta := range slow {
		assert.Greater(t, eta, previous, "sample %d of the slow phase must raise the ETA", index)
		previous = eta
	}
}

func TestETAEstimatorStallWithinWindow(t *testing.T) {
	t.Parallel()

	start := time.Unix(0, 0)
	estimator := newETAEstimator(start, etaWindow)

	etas, _ := runETAPhases(estimator, start, 100_000, []etaPhase{{10 * time.Minute, 10}, {2 * time.Minute, 0}})

	// Partway into a stall the window still holds progress, so the ETA grows
	// (6 B/s over the window) instead of collapsing to a tiny value.
	assert.Equal(t, 15_667*time.Second, etas[len(etas)-1])
}

func TestETAEstimatorSameInstantSample(t *testing.T) {
	t.Parallel()

	start := time.Unix(0, 0)
	estimator := newETAEstimator(start, etaWindow)

	// A sample at the start instant gives no elapsed time to form a rate.
	assert.Zero(t, estimator.estimate(start, 0, 400))

	assert.Equal(t, 70*time.Second, estimator.estimate(start.Add(10*time.Second), 50, 400))
	// Re-sampling the same instant refreshes the newest sample in place.
	assert.Equal(t, 30*time.Second, estimator.estimate(start.Add(10*time.Second), 100, 400))
}
