// This file implements GET /api/runs/{runID}/phases (issue #273): the 11-phase
// pipeline timeline (SPEC §4.3), reconstructed from a run's raw workflow
// history (history.go) — status, start/end times, and a small set of
// observable per-phase facts (e.g. Resolve's archive count, Verify's
// matched-file count), never from persisted state (SPEC §4.2).
package runsapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	commonpb "go.temporal.io/api/common/v1"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// PhaseStatus is one phase's state in the timeline, per issue #273 AC1.
type PhaseStatus string

const (
	// PhasePending means the phase did not complete and nothing is running in
	// it: it has not started — or, on a failed run, it holds only partial
	// activity from earlier drive-sets of the interleaved tape path and the
	// phase as a whole never finished (see buildPhaseTimeline's rule).
	PhasePending PhaseStatus = "pending"
	// PhaseActive means the run is still open and this phase is in progress:
	// it is the pipeline frontier (the phase actually executing right now —
	// which includes an operator-in-the-loop pause within it; GET
	// /api/runs/{runID}'s currentPause reports the pause itself), or a later
	// phase already holding activity from an earlier drive-set of the
	// still-running interleaved tape path. The latter case does not apply while
	// the run is paused: a pause pins "active" to the frontier (its own phase)
	// alone, so a completed earlier-set/re-driven Eject does not spin alongside
	// the paused Write (issue #331) — see buildPhaseTimeline.
	PhaseActive PhaseStatus = "active"
	// PhaseCompleted means the run verifiably moved past this phase: the
	// whole run succeeded, or the phase precedes the pipeline frontier. This
	// is true even when the phase contains one or more individually failed
	// attempts (e.g. a Load/Write-failure pause that was resumed and
	// completed on retry) — SPEC §4.3's operator-in-the-loop retries are
	// normal, not a phase failure.
	PhaseCompleted PhaseStatus = "completed"
	// PhaseFailed means this phase is the one workflows/backup's
	// NotifyFailure activity (or, as a fallback, the terminal failure
	// message) named as the failing phase — regardless of any later-phase
	// activity earlier drive-sets left behind.
	PhaseFailed PhaseStatus = "failed"
)

// PhaseFact is one observable, phase-specific key/value fact (issue #273
// AC2), e.g. Resolve's archive count or Verify's matched-file count. Value is
// pre-formatted text (not a typed number) so the API never needs a second,
// parallel type per fact — the same shape documented in
// docs/configuration.md.
type PhaseFact struct {
	// Key is a stable machine-readable identifier for this fact, e.g.
	// "archives" — for a client that wants to key off specific facts rather
	// than display the list generically.
	Key string `json:"key"`
	// Label is a short human-readable name for display, e.g. "Archives".
	Label string `json:"label"`
	// Value is the fact's pre-formatted display value, e.g. "71".
	Value string `json:"value"`
	// Title is an optional exact/expanded form of Value for a client to
	// surface as hover text, e.g. the precise byte count behind a humanized
	// "5.6 GB". Omitted (empty) when Value is already exact.
	Title string `json:"title,omitempty"`
}

// PhaseInfo is one pipeline phase's timeline entry.
type PhaseInfo struct {
	// Name is the phase's stable name, one of workflows/backup's Phase*
	// constants (PhaseResolve .. PhaseDeliver).
	Name string `json:"name"`
	// Status is this phase's state (see PhaseStatus).
	Status PhaseStatus `json:"status"`
	// StartTime is when the phase's first activity was scheduled. Nil when
	// the phase never started (PhasePending), or started with zero recorded
	// activity (the rare PhaseFailed edge case where the phase failed before
	// scheduling any activity of its own, e.g. the tape path's drive-set
	// planning failing before a Load activity is dispatched).
	StartTime *time.Time `json:"startTime,omitempty"`
	// EndTime is when the phase's last activity reached a terminal state.
	// Nil while the phase is still in progress (PhaseActive), or for the
	// PhaseFailed edge case above.
	EndTime *time.Time `json:"endTime,omitempty"`
	// Facts are this phase's observable facts, in a fixed display order (see
	// facts.go). Empty when the phase has produced none yet (e.g. it never
	// started, or completed with nothing meaningful to report — Burn
	// disabled).
	Facts []PhaseFact `json:"facts,omitempty"`
	// Error is the failure rendered as text, set only when Status ==
	// PhaseFailed.
	Error string `json:"error,omitempty"`
}

// RunPhasesResponse is the GET /api/runs/{runID}/phases response body.
type RunPhasesResponse struct {
	RunID  string      `json:"runId"`
	Phases []PhaseInfo `json:"phases"`
}

// getRunPhases implements GET /api/runs/{runID}/phases.
func (h *handler) getRunPhases(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	runID := r.PathValue("runID")
	if runID == "" {
		writeError(w, http.StatusBadRequest, errors.New("runID is required"))

		return
	}

	history, err := fetchRunHistory(ctx, h.temporalClient, runID)
	if err != nil {
		writeHistoryError(ctx, w, h.temporalClient, runID, err)

		return
	}

	outcomes := deriveTapeOutcomes(history.Activities)

	writeJSON(w, http.StatusOK, RunPhasesResponse{RunID: runID, Phases: buildPhaseTimeline(history, outcomes)})
}

// buildPhaseTimeline reconstructs the 11-phase timeline from history.
//
// The pipeline runs its phases in order, but the Load/Write/Eject tape path
// interleaves per drive-set (SPEC §4.3 phases 6-8): a later phase (Eject) can
// hold real, completed activity from an earlier drive-set while an earlier
// phase (Write) is still in flight — or has permanently failed — for a later
// set. "Some later phase has activity" therefore proves nothing about this
// phase's outcome, so statuses derive from a single pipeline *frontier*
// (timelineFrontier) instead:
//
//   - failed: this phase is the one the run's own failure record names
//     (history.FailingPhase — the NotifyFailure activity's input, or the
//     terminal-message fallback). This wins over everything: later-phase
//     activity from an earlier drive-set never masks the failure. When a run
//     failed but no failing phase could be recovered (a foreign/very old
//     history), the phase of the latest-scheduled activity is reported failed
//     with the terminal failure text, best-effort.
//   - completed: the whole run succeeded, or the phase precedes the frontier
//     in pipeline order — the run verifiably moved past it. This covers
//     activity-less no-op phases (a disabled Burn) and phases containing
//     individually failed-but-retried attempts the run moved past (a
//     write-failure pause resumed onto fresh blanks): operator-in-the-loop
//     retries are normal, not a phase failure.
//   - active: the run is still open and this phase is the frontier itself, or
//     a later phase already holding activity from an earlier drive-set — the
//     interleaved tape path is genuinely still in progress as a unit, so a
//     partially-driven Eject is "active", never prematurely "completed". The
//     "later phase holding an earlier set's activity" case is suppressed while
//     the run is paused for the operator (derivePause), though: a pause is not
//     "in progress as a unit" — the run is stopped on one specific phase (the
//     frontier, which is always the pause's own phase). Without this, a
//     write-failure pause — which ejects the set's tapes (a completed Eject
//     activity) before pausing back on Write — would light BOTH Write (the
//     frontier) and the already-completed Eject "active" at once, two spinners
//     for one paused phase (issue #331). Only the frontier stays active while
//     paused; the earlier-set/re-driven Eject falls to pending below.
//   - pending: everything else. On a *failed* run this includes a
//     later-than-failing phase with earlier-drive-set activity (e.g. set 1's
//     completed Eject when set 2's Write failed): the phase as a whole never
//     completed and nothing is running anymore, so it reports pending with no
//     time window (a "pending" phase with a start time would contradict
//     itself); the per-set/per-tape reality stays fully visible through the
//     tape-outcome endpoints.
func buildPhaseTimeline(history runHistory, outcomes []TapeOutcome) []PhaseInfo {
	byPhase := make(map[string][]activityRecord, len(phaseOrder))

	for _, record := range history.Activities {
		phase, ok := phaseForActivity(record.Name, record.Input)
		if !ok {
			continue
		}

		byPhase[phase] = append(byPhase[phase], record)
	}

	frontier, frontierFailed, failureText := timelineFrontier(history, byPhase)

	// While the run is paused for the operator, only the frontier (the pause's
	// own phase) is active: a later phase that merely holds an earlier drive-set's
	// completed activity must not also spin. See the "active" bullet above and
	// issue #331. derivePause reports PauseNone for a closed run, so this is only
	// ever true on an open, currently-paused run.
	paused := derivePause(history).Kind != backup.PauseNone

	timeline := make([]PhaseInfo, 0, len(phaseOrder))

	for i, name := range phaseOrder {
		records := byPhase[name]
		sortByScheduleOrder(records)

		info := PhaseInfo{Name: name}

		switch {
		case frontierFailed && i == frontier:
			info.Status = PhaseFailed
			info.Error = failureText
			info.StartTime, info.EndTime = spanPointers(records)
		case history.Succeeded || i < frontier:
			info.Status = PhaseCompleted
			info.StartTime, info.EndTime = spanPointers(records)
		case !history.Closed && (i == frontier || (len(records) > 0 && !paused)):
			info.Status = PhaseActive
			info.StartTime, _ = spanPointers(records)
		default:
			info.Status = PhasePending
		}

		info.Facts = phaseFacts(name, info.Status, records, outcomes)

		timeline = append(timeline, info)
	}

	return timeline
}

// timelineFrontier locates the pipeline frontier: the phaseOrder index the run
// has verifiably progressed *to* (every earlier phase completed to get there),
// whether the frontier phase failed there, and — when it did — the failure
// text to report on it.
//
//   - Succeeded run: one past the last phase, so every phase reads completed.
//   - Failed run with a recovered failing phase: that phase's index, failed,
//     with history.FailingSummary.
//   - Failed run with no recoverable failing phase (foreign/very old
//     history): the phase of the latest-scheduled activity, failed with the
//     terminal failure message, best-effort. With no phase activity at all
//     nothing is marked failed (frontier -1, all phases pending) — e.g. the
//     entry config-validation rejection, which fails before any phase runs.
//   - Open run: the phase of the latest-scheduled activity — with the
//     interleaved tape path this is the phase actually executing now, not the
//     furthest phase an earlier drive-set ever touched. -1 (all pending) when
//     nothing has been scheduled yet.
func timelineFrontier(history runHistory, byPhase map[string][]activityRecord) (frontier int, frontierFailed bool, failureText string) {
	if history.Succeeded {
		return len(phaseOrder), false, ""
	}

	if history.Closed && history.FailingPhase != "" {
		if index := phaseIndex(history.FailingPhase); index >= 0 {
			return index, true, history.FailingSummary
		}
	}

	latest := -1

	var latestEventID int64

	for i, name := range phaseOrder {
		for _, record := range byPhase[name] {
			if record.ScheduledEventID >= latestEventID {
				latestEventID = record.ScheduledEventID
				latest = i
			}
		}
	}

	if history.Closed {
		return latest, latest >= 0, history.FailureMessage
	}

	return latest, false, ""
}

// phaseIndex returns name's index in phaseOrder, or -1 when it is not one of
// the 11 pipeline phases (e.g. a failing-phase name recorded by a different,
// older version of the workflow code).
func phaseIndex(name string) int {
	for i, phase := range phaseOrder {
		if phase == name {
			return i
		}
	}

	return -1
}

// spanPointers returns records' activitySpan as pointers, each nil when the
// corresponding time is zero (activitySpan's "not yet known" sentinel).
func spanPointers(records []activityRecord) (start, end *time.Time) {
	s, e := activitySpan(records)
	if !s.IsZero() {
		start = &s
	}

	if !e.IsZero() {
		end = &e
	}

	return start, end
}

// activitySpan returns a phase's overall [start, end) window: the earliest
// ScheduledTime and the latest terminal EndTime among records. end is the zero
// time when no record has reached a terminal state yet.
func activitySpan(records []activityRecord) (start, end time.Time) {
	for _, record := range records {
		if start.IsZero() || record.ScheduledTime.Before(start) {
			start = record.ScheduledTime
		}

		if !record.EndTime.IsZero() && record.EndTime.After(end) {
			end = record.EndTime
		}
	}

	return start, end
}

// phaseForActivity maps an activity's Temporal type name (and, for the one
// activity shared between two phases, its decoded input) to the phase it
// belongs to. Returns ok == false for an activity that is not attributed to
// any single phase's time window:
//   - NotifyFailure (failure.go): the cross-cutting failure alert, consumed
//     directly by populateFailingPhase (history.go), not phase-scoped.
//   - ReleaseSnapshots (hold.go): the deferred snapshot-hold release that runs
//     on *every* exit path, including long after Resolve (success or
//     failure) — attributing it to Resolve's window would stretch Resolve's
//     end time to the very end of the run.
func phaseForActivity(name string, input *commonpb.Payloads) (string, bool) {
	switch name {
	case "ResolveK8sSources", "ResolveAndCheck", "HoldSnapshots":
		return backup.PhaseResolve, true
	case "PrepareArchives":
		return backup.PhasePrepare, true
	case "Pack":
		return backup.PhasePack, true
	case "GeneratePAR2":
		return backup.PhaseGeneratePAR2, true
	case "Verify":
		return backup.PhaseVerify, true
	case "Load":
		return backup.PhaseLoad, true
	case "FormatTape", "WriteTree", "FinalizeTape", "MeasureWriteHealth", "TeardownSession":
		return backup.PhaseWrite, true
	case "Eject", "IOStationStatus", "NotifyOperatorPause":
		return backup.PhaseEject, true
	case "BuildReport":
		return backup.PhaseReport, true
	case "BurnDisc", "VerifyDisc", "NotifyBurnPause", "RebuildDeliveredReport":
		return backup.PhaseBurn, true
	case "Deliver":
		return backup.PhaseDeliver, true
	case "NotifyWritePathPause":
		// writepause.go's WritePathPauseInput{Phase: "Load"|"Write", ...} names
		// which of the two sub-phases actually failed; decode it so the pause
		// alert lands in the right phase's window instead of always Write.
		return writePathPausePhase(input), true
	default:
		return "", false
	}
}

// writeHistoryError classifies a fetchRunHistory failure into an HTTP
// response, distinguishing three cases (issue #273 AC3/AC7):
//   - a malformed run ID: mapped exactly like every other endpoint's existing
//     InvalidArgument handling (statusForTemporalError), 400.
//   - a run ID Temporal has no record of at all: 404.
//   - a run ID that IS a real execution of the singleton backup workflow
//     (still present in Temporal visibility — proof it once existed) but
//     whose event history has fallen out of Temporal's retention window: 410
//     Gone, distinguishable from the 404 above. This distinction is derived
//     on demand from Temporal's own existing visibility index (the same one
//     GET /api/runs already reads), never a new catalog (SPEC §4.2).
func writeHistoryError(ctx context.Context, w http.ResponseWriter, temporalClient TemporalClient, runID string, err error) {
	status := statusForTemporalError(err)
	if status != http.StatusNotFound {
		writeError(w, status, fmt.Errorf("fetch run %q history: %w", runID, err))

		return
	}

	existed, checkErr := runExistsInVisibility(ctx, temporalClient, runID)
	if checkErr == nil && existed {
		writeError(w, http.StatusGone, fmt.Errorf(
			"run %q exists but its Temporal workflow history has aged out of the retention window "+
				"and can no longer be reconstructed", runID))

		return
	}

	writeError(w, http.StatusNotFound, fmt.Errorf("run %q not found", runID))
}
