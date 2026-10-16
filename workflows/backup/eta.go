package backup

import (
	"time"
)

// etaWindow is how far back the ETA's rate looks: long enough to smooth over
// per-file jitter, short enough that a rate change shows within minutes (#427).
const etaWindow = 5 * time.Minute

type etaSample struct {
	at   time.Time
	done int64
}

// etaEstimator projects time remaining from the progress rate over the last
// etaWindow, not the phase-wide average, so a slowdown is reflected promptly.
// It is not safe for concurrent use; callers hold their own lock.
type etaEstimator struct {
	window  time.Duration
	samples []etaSample
}

// newETAEstimator seeds a zero-progress sample at start so the first estimate
// has a baseline.
func newETAEstimator(start time.Time, window time.Duration) *etaEstimator {
	return &etaEstimator{window: window, samples: []etaSample{{at: start}}}
}

// estimate records done bytes at now and returns the projected time to reach
// total, or zero when there is no rate yet or progress has stalled across the
// whole window (an ETA would be meaningless, not small).
func (e *etaEstimator) estimate(now time.Time, done, total int64) time.Duration {
	e.observe(now, done)

	oldest, newest := e.samples[0], e.samples[len(e.samples)-1]

	remaining := total - done
	span := newest.at.Sub(oldest.at)
	progressed := newest.done - oldest.done

	if remaining <= 0 || span <= 0 || progressed <= 0 {
		return 0
	}

	eta := time.Duration(float64(remaining) / float64(progressed) * float64(span))

	return eta.Round(time.Second)
}

// observe appends a sample and drops those no longer needed, keeping the
// newest sample at or before the window start as the rate's anchor.
func (e *etaEstimator) observe(now time.Time, done int64) {
	if last := &e.samples[len(e.samples)-1]; !now.After(last.at) {
		// A same-instant (or clock-skewed) sample only refreshes the newest.
		last.done = done
	} else {
		e.samples = append(e.samples, etaSample{at: now, done: done})
	}

	cutoff := now.Add(-e.window)

	drop := 0
	for drop+1 < len(e.samples) && !e.samples[drop+1].at.After(cutoff) {
		drop++
	}

	e.samples = e.samples[drop:]
}
