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
	// PhasePending means the phase has not started: no activity belonging to
	// it has been scheduled, and it is not the phase that failed the run.
	PhasePending PhaseStatus = "pending"
	// PhaseActive means the phase is the furthest one with any scheduled
	// activity and the run has not yet moved past it or failed there — it is
	// currently in progress (which includes an operator-in-the-loop pause
	// within it; GET /api/runs/{runID}'s currentPause reports the pause
	// itself, this endpoint only reports which phase it is in).
	PhaseActive PhaseStatus = "active"
	// PhaseCompleted means the run moved past this phase: a later phase has
	// scheduled activity, or the whole run succeeded. This is true even when
	// the phase itself contains one or more individually failed attempts
	// (e.g. a Load/Write-failure pause that was resumed and completed on
	// retry) — SPEC §4.3's operator-in-the-loop retries are normal, not a
	// phase failure.
	PhaseCompleted PhaseStatus = "completed"
	// PhaseFailed means this phase is the one workflows/backup's
	// NotifyFailure activity (or, as a fallback, the terminal failure
	// message) named as the failing phase, and the run never moved past it.
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

// buildPhaseTimeline reconstructs the 11-phase timeline from history. Because
// workflows/backup runs its phases strictly in order (workflow.go's single
// sequential loop; the Load/Write/Eject tape path is itself sequential across
// drive-sets, pausing rather than branching on a failure), a phase's status is
// fully determined by two things: whether *any later* phase has scheduled
// activity (or the run succeeded outright) — meaning the run moved past this
// phase — and whether this phase is the one workflows/backup's own failure
// alert named as having failed.
func buildPhaseTimeline(history runHistory, outcomes []TapeOutcome) []PhaseInfo {
	byPhase := make(map[string][]activityRecord, len(phaseOrder))

	for _, record := range history.Activities {
		phase, ok := phaseForActivity(record.Name, record.Input)
		if !ok {
			continue
		}

		byPhase[phase] = append(byPhase[phase], record)
	}

	lastWithActivity := -1

	for i, name := range phaseOrder {
		if len(byPhase[name]) > 0 {
			lastWithActivity = i
		}
	}

	timeline := make([]PhaseInfo, 0, len(phaseOrder))

	for i, name := range phaseOrder {
		records := byPhase[name]
		sortByScheduleOrder(records)

		info := PhaseInfo{Name: name}

		switch {
		case len(records) == 0 && history.FailingPhase == name:
			info.Status = PhaseFailed
			info.Error = history.FailingSummary
		case len(records) == 0 && (i < lastWithActivity || history.Succeeded):
			// The run moved past this phase without it scheduling any
			// activity of its own — it ran as a no-op (Burn with optical
			// burning disabled, burnpath.go; or an empty tape plan's
			// Load/Write/Eject). Completed, but with no time window to
			// report.
			info.Status = PhaseCompleted
		case len(records) == 0:
			info.Status = PhasePending
		case i < lastWithActivity || history.Succeeded:
			info.Status = PhaseCompleted
			info.StartTime, info.EndTime = spanPointers(records)
		case history.FailingPhase == name:
			info.Status = PhaseFailed
			info.Error = history.FailingSummary
			info.StartTime, info.EndTime = spanPointers(records)
		default:
			info.Status = PhaseActive
			info.StartTime, _ = spanPointers(records)
		}

		info.Facts = phaseFacts(name, info.Status, records, outcomes)

		timeline = append(timeline, info)
	}

	return timeline
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
