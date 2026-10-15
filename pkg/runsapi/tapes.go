// GET /api/runs/{runID}/tapes and GET /api/tapes (issue #273): tape outcomes
// reconstructed from workflow history by joining Load's barcodes with the
// Write-phase activities, never persisted state (SPEC §4.2). The aggregate
// endpoint degrades per run instead of failing the whole listing.
package runsapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

const (
	// tapeOutcomeLoaded: loaded, but its write pipeline has not finished (still
	// running, or the run ended first).
	tapeOutcomeLoaded = "loaded"
	// tapeOutcomeWritten: FinalizeTape completed.
	tapeOutcomeWritten = "written"
	// tapeOutcomeFailed: FormatTape, WriteTree, or FinalizeTape failed.
	tapeOutcomeFailed = "failed"
)

// WriteHealthInfo is the JSON projection of backup.WriteHealth (SPEC §14).
type WriteHealthInfo struct {
	// Measured is false when no measurement could be taken; the other fields
	// are then zero placeholders, so "unmeasured" is distinguishable from
	// "unhealthy".
	Measured       bool    `json:"measured"`
	ThroughputMBps float64 `json:"throughputMBps"`
	// ThroughputUnmeasurable marks a write too small to judge streaming;
	// throughputMBps is then a zero placeholder, not a reading.
	ThroughputUnmeasurable bool     `json:"throughputUnmeasurable"`
	FloorMBps              float64  `json:"floorMBps,omitempty"`
	FloorKnown             bool     `json:"floorKnown"`
	BelowFloor             bool     `json:"belowFloor"`
	Repositions            int64    `json:"repositions,omitempty"`
	RepositionsMeasured    bool     `json:"repositionsMeasured"`
	TapeAlertFlags         []string `json:"tapeAlertFlags,omitempty"`
	// Healthy is backup.WriteHealth.Healthy(), pre-computed; always false when
	// Measured is false.
	Healthy bool `json:"healthy"`
}

func toWriteHealthInfo(health backup.WriteHealth) *WriteHealthInfo {
	return &WriteHealthInfo{
		Measured:               health.Measured,
		ThroughputMBps:         health.ThroughputMBps,
		ThroughputUnmeasurable: health.ThroughputUnmeasurable,
		FloorMBps:              health.FloorMBps,
		FloorKnown:             health.FloorKnown,
		BelowFloor:             health.BelowFloor,
		Repositions:            health.Repositions,
		RepositionsMeasured:    health.RepositionsMeasured,
		TapeAlertFlags:         health.TapeAlertFlags,
		Healthy:                health.Healthy(),
	}
}

// TapeOutcome is one physical tape this run loaded, with its outcome.
type TapeOutcome struct {
	Barcode    string `json:"barcode"`
	TapeIndex  int    `json:"tapeIndex"`
	CopyIndex  int    `json:"copyIndex"`
	DriveIndex int    `json:"driveIndex"`
	// Slot is the storage slot the tape was loaded from.
	Slot int `json:"slot"`
	// Result is one of tapeOutcomeLoaded/tapeOutcomeWritten/tapeOutcomeFailed.
	Result string `json:"result"`
	// Error is set only when Result is "failed".
	Error string `json:"error,omitempty"`
	// OverwroteNonBlank is true when a non-blank tape was written over because
	// the run set library.allowNonBlankTapes (SPEC §4.3 step 6).
	OverwroteNonBlank bool `json:"overwroteNonBlank,omitempty"`
	// WriteHealth is nil when no MeasureWriteHealth result exists for the tape.
	WriteHealth *WriteHealthInfo `json:"writeHealth,omitempty"`
}

// RunTapesResponse is the GET /api/runs/{runID}/tapes response body.
type RunTapesResponse struct {
	RunID string        `json:"runId"`
	Tapes []TapeOutcome `json:"tapes"`
}

// AggregateTapeOutcome is one tape outcome in GET /api/tapes, attributed to its run.
type AggregateTapeOutcome struct {
	TapeOutcome
	RunID        string    `json:"runId"`
	RunStartTime time.Time `json:"runStartTime"`
	RunStatus    string    `json:"runStatus"`
	// DryRun marks virtual mhvtl tapes so they are not mistaken for real inventory.
	DryRun bool `json:"dryRun"`
}

// RunError names a run whose tape outcomes could not be reconstructed.
type RunError struct {
	RunID string `json:"runId"`
	Error string `json:"error"`
}

// AggregateTapesResponse is the GET /api/tapes response body.
type AggregateTapesResponse struct {
	Tapes []AggregateTapeOutcome `json:"tapes"`
	// RunErrors lists runs whose tape outcomes could not be derived.
	RunErrors []RunError `json:"runErrors,omitempty"`
}

// listTapesConcurrency bounds concurrent GetWorkflowHistory RPCs.
const listTapesConcurrency = 8

// defaultListTapesRunLimit bounds how many newest runs GET /api/tapes
// reconstructs by default, since each costs a full history fetch.
const defaultListTapesRunLimit = 50

func (h *handler) getRunTapes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	runID := r.PathValue("runID")
	if runID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("runID is required"))

		return
	}

	history, err := fetchRunHistory(ctx, h.temporalClient, runID)
	if err != nil {
		writeHistoryError(ctx, w, h.temporalClient, runID, err)

		return
	}

	writeJSON(w, http.StatusOK, RunTapesResponse{RunID: runID, Tapes: deriveTapeOutcomes(history.Activities)})
}

// listTapes implements GET /api/tapes over the newest `limit` runs. A run whose
// history cannot be reconstructed becomes a RunError rather than failing the
// response.
func (h *handler) listTapes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	limit, err := listTapesRunLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)

		return
	}

	executions, err := listAllBackupExecutions(ctx, h.temporalClient)
	if err != nil {
		writeError(w, statusForTemporalError(err), fmt.Errorf("list workflow executions: %w", err))

		return
	}

	// Sorted in Go: SQL-backed Temporal visibility rejects ORDER BY.
	sort.Slice(executions, func(i, j int) bool {
		return executions[i].GetStartTime().AsTime().After(executions[j].GetStartTime().AsTime())
	})

	if len(executions) > limit {
		executions = executions[:limit]
	}

	var (
		mutex sync.Mutex
		// Non-nil so an empty result serializes as [] rather than null.
		tapes = []AggregateTapeOutcome{}
		errs  []RunError
	)

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(listTapesConcurrency)

	for _, execution := range executions {
		runID := execution.GetExecution().GetRunId()
		summary := toRunSummary(execution)
		startTime := summary.StartTime
		status := execution.GetStatus().String()
		dryRun := summary.DryRun

		group.Go(func() error {
			history, err := fetchRunHistory(groupCtx, h.temporalClient, runID)

			mutex.Lock()
			defer mutex.Unlock()

			if err != nil {
				// Mask upstream errors as writeError does: they can embed internal
				// host detail and would otherwise leak via a 200 body.
				status := statusForTemporalError(err)
				if status == http.StatusBadGateway || status == http.StatusGatewayTimeout {
					slog.ErrorContext(groupCtx, "runsapi: reconstruct run history for tape listing failed", "run_id", runID, "error", err)
				}

				errs = append(errs, RunError{RunID: runID, Error: clientFacingMessage(status, err)})

				return nil
			}

			for _, outcome := range deriveTapeOutcomes(history.Activities) {
				tapes = append(tapes, AggregateTapeOutcome{
					TapeOutcome:  outcome,
					RunID:        runID,
					RunStartTime: startTime,
					RunStatus:    status,
					DryRun:       dryRun,
				})
			}

			return nil
		})
	}

	// Always nil: goroutines record failures in errs so one run degrades alone.
	_ = group.Wait()

	// A done request context is one infra failure (timeout or disconnect), not
	// N per-run failures; report it as such instead of a degraded 200.
	if err := ctx.Err(); err != nil {
		writeError(w, statusForTemporalError(err), fmt.Errorf("list tapes: %w", err))

		return
	}

	sort.Slice(tapes, func(i, j int) bool {
		if !tapes[i].RunStartTime.Equal(tapes[j].RunStartTime) {
			return tapes[i].RunStartTime.After(tapes[j].RunStartTime)
		}

		if tapes[i].TapeIndex != tapes[j].TapeIndex {
			return tapes[i].TapeIndex < tapes[j].TapeIndex
		}

		if tapes[i].CopyIndex != tapes[j].CopyIndex {
			return tapes[i].CopyIndex < tapes[j].CopyIndex
		}

		// A retry loads a fresh blank with the same indexes; barcode keeps the
		// order deterministic.
		return tapes[i].Barcode < tapes[j].Barcode
	})
	sort.Slice(errs, func(i, j int) bool { return errs[i].RunID < errs[j].RunID })

	writeJSON(w, http.StatusOK, AggregateTapesResponse{Tapes: tapes, RunErrors: errs})
}

// listTapesRunLimit parses `limit`: empty means the default, otherwise a
// positive integer capped at listPageSize.
func listTapesRunLimit(raw string) (int, error) {
	if raw == "" {
		return defaultListTapesRunLimit, nil
	}

	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 {
		return 0, fmt.Errorf("limit must be a positive integer, got %q", raw)
	}

	if limit > listPageSize {
		return listPageSize, nil
	}

	return limit, nil
}

// deriveTapeOutcomes returns one entry per loaded barcode. A failed tape's
// retry loads a fresh blank with its own barcode, so it appears as a separate
// entry (SPEC §4.3).
func deriveTapeOutcomes(activities []activityRecord) []TapeOutcome {
	var loaded []backup.LoadedTape

	for _, record := range activities {
		if record.Name != "Load" || !record.Completed {
			continue
		}

		var batch []backup.LoadedTape
		if err := decodePayloads(record.Result, &batch); err != nil {
			continue
		}

		loaded = append(loaded, batch...)
	}

	outcomes := make([]TapeOutcome, 0, len(loaded))

	for _, tape := range loaded {
		outcomes = append(outcomes, tapeOutcomeFor(activities, tape))
	}

	sort.Slice(outcomes, func(i, j int) bool {
		if outcomes[i].TapeIndex != outcomes[j].TapeIndex {
			return outcomes[i].TapeIndex < outcomes[j].TapeIndex
		}

		if outcomes[i].CopyIndex != outcomes[j].CopyIndex {
			return outcomes[i].CopyIndex < outcomes[j].CopyIndex
		}

		// Deterministic tiebreak for retries on the same slot.
		return outcomes[i].Barcode < outcomes[j].Barcode
	})

	return outcomes
}

// tapeOutcomeFor walks FormatTape, WriteTree, FinalizeTape in order: the first
// failure decides "failed", a completed FinalizeTape decides "written",
// otherwise the tape stays "loaded".
func tapeOutcomeFor(activities []activityRecord, tape backup.LoadedTape) TapeOutcome {
	barcode := string(tape.Barcode)

	outcome := TapeOutcome{
		Barcode:           barcode,
		TapeIndex:         tape.TapeIndex,
		CopyIndex:         tape.CopyIndex,
		DriveIndex:        tape.DriveIndex,
		Slot:              tape.SourceSlot,
		Result:            tapeOutcomeLoaded,
		OverwroteNonBlank: tape.OverwroteNonBlank,
	}

	for _, name := range []string{"FormatTape", "WriteTree", "FinalizeTape"} {
		record, ok := activityByBarcode(activities, name, barcode)
		if !ok {
			break
		}

		if record.Failure() {
			outcome.Result = tapeOutcomeFailed
			outcome.Error = record.ErrorText

			return outcome
		}

		if name == "FinalizeTape" && record.Completed {
			outcome.Result = tapeOutcomeWritten
		}
	}

	if record, ok := activityByBarcode(activities, "MeasureWriteHealth", barcode); ok && record.Completed {
		// The result is a TapeMeasurement wrapping Health. Decoding straight into
		// WriteHealth silently yields the zero value (issue #338).
		var measurement backup.TapeMeasurement
		if err := decodePayloads(record.Result, &measurement); err == nil {
			outcome.WriteHealth = toWriteHealthInfo(measurement.Health)
		}
	}

	return outcome
}

// activityByBarcode relies on at most one activity per (name, barcode):
// FormatTape/WriteTree never retry, and FinalizeTape's retries share one
// ScheduledEventId.
func activityByBarcode(activities []activityRecord, name, barcode string) (activityRecord, bool) {
	if barcode == "" {
		return activityRecord{}, false
	}

	for _, record := range activities {
		if record.Name == name && record.Barcode == barcode {
			return record, true
		}
	}

	return activityRecord{}, false
}
