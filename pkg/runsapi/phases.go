// GET /api/runs/{runID}/phases (issue #273): the 11-phase pipeline timeline
// (SPEC §4.3), reconstructed from workflow history, never persisted state (SPEC §4.2).
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

// PhaseStatus is one phase's state in the timeline.
type PhaseStatus string

const (
	// PhasePending means the phase has not started, or on a failed run never
	// finished as a whole.
	PhasePending PhaseStatus = "pending"
	// PhaseActive means the run is open and this phase is in progress (see
	// buildPhaseTimeline).
	PhaseActive PhaseStatus = "active"
	// PhaseCompleted means the run verifiably moved past this phase, even if
	// it contains failed-then-retried attempts.
	PhaseCompleted PhaseStatus = "completed"
	// PhaseFailed means this is the phase the run's failure record names.
	PhaseFailed PhaseStatus = "failed"
)

// PhaseFact is one observable phase-specific fact with a pre-formatted Value.
type PhaseFact struct {
	// Key is a stable machine-readable identifier, e.g. "archives".
	Key string `json:"key"`
	// Label is a short display name, e.g. "Archives".
	Label string `json:"label"`
	// Value is the pre-formatted display value, e.g. "71".
	Value string `json:"value"`
	// Title is an optional exact form of Value for hover text, e.g. a precise
	// byte count behind "5.6 GB".
	Title string `json:"title,omitempty"`
}

// PhaseInfo is one pipeline phase's timeline entry.
type PhaseInfo struct {
	// Name is one of workflows/backup's Phase* constants.
	Name string `json:"name"`
	// Status is this phase's state.
	Status PhaseStatus `json:"status"`
	// StartTime is when the phase's first activity was scheduled; nil if none was.
	StartTime *time.Time `json:"startTime,omitempty"`
	// EndTime is when the phase's last activity finished; nil while active.
	EndTime *time.Time `json:"endTime,omitempty"`
	// Facts are this phase's facts in fixed display order (see facts.go).
	Facts []PhaseFact `json:"facts,omitempty"`
	// Error is set only when Status is PhaseFailed.
	Error string `json:"error,omitempty"`
	// Waiting is set on an active phase whose activity is scheduled but has not
	// been picked up by a worker yet, distinguishing it from one executing.
	Waiting *PhaseWaiting `json:"waiting,omitempty"`
}

// RunPhasesResponse is the GET /api/runs/{runID}/phases response body.
type RunPhasesResponse struct {
	RunID  string      `json:"runId"`
	Phases []PhaseInfo `json:"phases"`
}

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

	timeline := buildPhaseTimeline(history, deriveTapeOutcomes(history.Activities))
	annotateWaiting(ctx, h.temporalClient, runID, history, timeline, time.Now())

	writeJSON(w, http.StatusOK, RunPhasesResponse{RunID: runID, Phases: timeline})
}

// buildPhaseTimeline derives statuses from a single pipeline frontier
// (timelineFrontier). The Load/Write/Eject path interleaves per drive-set, so a
// later phase can hold an earlier set's activity while an earlier phase is
// still running or has failed; later-phase activity proves nothing on its own.
//
//   - failed: the phase the run's failure record names; wins over everything.
//   - completed: the run succeeded, or the phase precedes the frontier.
//   - active: open run and the frontier, or a later phase holding an earlier
//     set's activity. The latter is suppressed while paused, so a write-failure
//     pause does not light both Write and its completed Eject (issue #331).
//   - pending: everything else, including a failed run's later phases that hold
//     earlier-set activity (no time window, to avoid a contradictory "pending
//     with a start time").
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

	// Only true on an open, currently-paused run (issue #331).
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

// timelineFrontier returns the phaseOrder index the run has progressed to,
// whether it failed there, and the failure text. A succeeded run returns one
// past the last phase. A failed run uses the recovered failing phase, falling
// back to the latest-scheduled activity's phase (-1 if none ran). An open run
// uses the latest-scheduled activity's phase, which with the interleaved tape
// path is the one executing now.
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

// phaseIndex returns -1 for a name not in phaseOrder (e.g. from an older workflow version).
func phaseIndex(name string) int {
	for i, phase := range phaseOrder {
		if phase == name {
			return i
		}
	}

	return -1
}

// spanPointers returns activitySpan with zero times as nil.
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

// activitySpan returns the earliest ScheduledTime and latest EndTime; end is
// zero while no record is terminal.
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

// phaseForActivity maps an activity type to its phase. NotifyFailure,
// ReleaseSnapshots and DestroyRunSnapshots return ok == false: the latter two
// run on every exit path and would otherwise stretch Resolve's window to the
// end of the run.
func phaseForActivity(name string, input *commonpb.Payloads) (string, bool) {
	switch name {
	case "ResolvePropertySources", "ListClaimSources", "ResolveK8sSources", "ResolveAndCheck", "CreateRunSnapshots", "HoldSnapshots":
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
		// The input names whether Load or Write failed.
		return writePathPausePhase(input), true
	default:
		return "", false
	}
}

// writeHistoryError maps a fetchRunHistory failure to 400/404, or to 410 Gone
// when the run still exists in Temporal visibility but its history has aged
// out of retention (issue #273). Visibility is Temporal's own index, not a new
// catalog (SPEC §4.2).
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
