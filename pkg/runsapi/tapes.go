// This file implements GET /api/runs/{runID}/tapes and GET /api/tapes (issue
// #273): per-run and aggregate-across-runs tape/copy outcomes, reconstructed
// from a run's raw workflow history (history.go) by correlating the Load
// phase's per-tape barcodes with the Write phase's FormatTape/WriteTree/
// FinalizeTape/MeasureWriteHealth activities for that same barcode — never
// from persisted state (SPEC §4.2). The aggregate endpoint degrades per run
// (issue #273's requirement): a run whose history cannot be reconstructed
// contributes an entry to RunErrors instead of failing the whole listing.
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

// Tape outcome result values (issue #273 AC5/AC6).
const (
	// tapeOutcomeLoaded means the tape was loaded into a drive but the write
	// pipeline for it has not reached a terminal state yet in this history —
	// either the run is still in progress, or it ended (crashed/was
	// terminated) before finishing this tape.
	tapeOutcomeLoaded = "loaded"
	// tapeOutcomeWritten means FinalizeTape completed successfully for this
	// tape: it was formatted, written, and its LTFS index captured.
	tapeOutcomeWritten = "written"
	// tapeOutcomeFailed means FormatTape, WriteTree, or FinalizeTape failed,
	// timed out, or was canceled for this tape.
	tapeOutcomeFailed = "failed"
)

// WriteHealthInfo is the JSON projection of backup.WriteHealth (issue #273):
// the observational write-health verdict measured for one tape (SPEC §14),
// present only when a MeasureWriteHealth result was found for the tape's
// barcode.
type WriteHealthInfo struct {
	// Measured mirrors backup.WriteHealth.Measured: false means the
	// MeasureWriteHealth activity could not take a measurement at all
	// (writehealth.go — the run still succeeds), so every other field here is
	// a zero placeholder, not a measured value. Without this flag a
	// never-measured tape would be indistinguishable from one measured
	// unhealthy (Healthy == false either way).
	Measured            bool     `json:"measured"`
	ThroughputMBps      float64  `json:"throughputMBps"`
	FloorMBps           float64  `json:"floorMBps,omitempty"`
	FloorKnown          bool     `json:"floorKnown"`
	BelowFloor          bool     `json:"belowFloor"`
	Repositions         int64    `json:"repositions,omitempty"`
	RepositionsMeasured bool     `json:"repositionsMeasured"`
	TapeAlertFlags      []string `json:"tapeAlertFlags,omitempty"`
	// Healthy mirrors backup.WriteHealth.Healthy(): the tape streamed cleanly
	// (measured, at/above a known floor, zero measured repositions, no active
	// TapeAlert flags). Carried pre-computed so clients need not reimplement
	// the verdict. Always false when Measured is false — check Measured to
	// tell "unhealthy" apart from "unmeasured".
	Healthy bool `json:"healthy"`
}

// toWriteHealthInfo maps a decoded backup.WriteHealth to its JSON projection.
func toWriteHealthInfo(health backup.WriteHealth) *WriteHealthInfo {
	return &WriteHealthInfo{
		Measured:            health.Measured,
		ThroughputMBps:      health.ThroughputMBps,
		FloorMBps:           health.FloorMBps,
		FloorKnown:          health.FloorKnown,
		BelowFloor:          health.BelowFloor,
		Repositions:         health.Repositions,
		RepositionsMeasured: health.RepositionsMeasured,
		TapeAlertFlags:      health.TapeAlertFlags,
		Healthy:             health.Healthy(),
	}
}

// TapeOutcome is one physical tape this run loaded, with its outcome (issue
// #273 AC5): barcode, logical tape/copy index, storage slot, and result.
type TapeOutcome struct {
	Barcode    string `json:"barcode"`
	TapeIndex  int    `json:"tapeIndex"`
	CopyIndex  int    `json:"copyIndex"`
	DriveIndex int    `json:"driveIndex"`
	// Slot is the storage slot the blank tape was loaded from
	// (LoadedTape.SourceSlot).
	Slot int `json:"slot"`
	// Result is one of tapeOutcomeLoaded/tapeOutcomeWritten/tapeOutcomeFailed.
	Result string `json:"result"`
	// Error is the failure rendered as text, set only when Result ==
	// "failed".
	Error string `json:"error,omitempty"`
	// OverwroteNonBlank is true when this tape was found non-blank at load
	// and written over anyway because the run set
	// library.allowNonBlankTapes (SPEC §4.3 step 6).
	OverwroteNonBlank bool `json:"overwroteNonBlank,omitempty"`
	// WriteHealth is the observational write-health verdict (SPEC §14), nil
	// when no MeasureWriteHealth result was found for this barcode (not yet
	// measured, or the tape never reached the Write phase).
	WriteHealth *WriteHealthInfo `json:"writeHealth,omitempty"`
}

// RunTapesResponse is the GET /api/runs/{runID}/tapes response body.
type RunTapesResponse struct {
	RunID string        `json:"runId"`
	Tapes []TapeOutcome `json:"tapes"`
}

// AggregateTapeOutcome is one tape outcome in the GET /api/tapes aggregate
// listing, attributed back to its run (issue #273 AC6).
type AggregateTapeOutcome struct {
	TapeOutcome
	RunID        string    `json:"runId"`
	RunStartTime time.Time `json:"runStartTime"`
	RunStatus    string    `json:"runStatus"`
}

// RunError names a run whose tape outcomes could not be reconstructed
// (history.go's aged-out/not-found/upstream classification), reported
// alongside — never instead of — every other run's successfully derived
// tapes, so one bad run degrades the listing rather than failing it entirely
// (issue #273's explicit requirement).
type RunError struct {
	RunID string `json:"runId"`
	Error string `json:"error"`
}

// AggregateTapesResponse is the GET /api/tapes response body.
type AggregateTapesResponse struct {
	Tapes []AggregateTapeOutcome `json:"tapes"`
	// RunErrors lists runs (still within Temporal visibility) whose tape
	// outcomes could not be derived, each with why. Omitted (empty) when
	// every run in Temporal visibility degraded cleanly.
	RunErrors []RunError `json:"runErrors,omitempty"`
}

// listTapesConcurrency bounds how many runs' histories listTapes fetches at
// once. Backup runs are a serial singleton (SPEC §4.2), so Temporal
// visibility holds at most a few hundred executions in practice (see
// listPageSize's doc comment) — this only guards against issuing that many
// GetWorkflowHistory RPCs to Temporal all at once.
const listTapesConcurrency = 8

// defaultListTapesRunLimit is how many of the most recent runs GET /api/tapes
// reconstructs when the request does not say (the `limit` query parameter).
// Each run costs a full history fetch + parse, so the endpoint's work is
// O(runs × history size); bounding it to the newest runs by default keeps the
// common tapes-page/dashboard request cheap while `?limit=` (capped at
// listPageSize) lets a client deliberately reach further back.
const defaultListTapesRunLimit = 50

// getRunTapes implements GET /api/runs/{runID}/tapes.
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

// listTapes implements GET /api/tapes: tape outcomes across the most recent
// runs still in Temporal visibility (issue #273 AC6), driving the Tapes page
// and the dashboard's library-history summary. The `limit` query parameter
// bounds how many of the newest runs are reconstructed
// (defaultListTapesRunLimit when absent, capped at listPageSize) since each
// run costs a full history fetch. Each run's history is fetched independently
// and degrades on its own failure (RunError, which applies within whatever
// limit is in effect) rather than failing the whole response — a listing must
// never 500 because one old or foreign/stub run's history cannot be
// reconstructed.
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

	// Newest runs first, then apply the run limit. Sorting happens here in Go
	// for the same reason listRuns sorts client-side (runsapi.go): standard
	// SQL-backed Temporal visibility rejects ORDER BY, so this is the only
	// portable ordering.
	sort.Slice(executions, func(i, j int) bool {
		return executions[i].GetStartTime().AsTime().After(executions[j].GetStartTime().AsTime())
	})

	if len(executions) > limit {
		executions = executions[:limit]
	}

	var (
		mutex sync.Mutex
		// Initialized (not a nil slice) so an empty result serializes as
		// "tapes": [] rather than "tapes": null — a fresh deployment with no
		// runs, or a limit window in which every run's history has aged out,
		// otherwise emits null and crashes a web client that maps over it. This
		// matches the per-run endpoint, whose deriveTapeOutcomes always returns
		// a non-nil slice.
		tapes = []AggregateTapeOutcome{}
		errs  []RunError
	)

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(listTapesConcurrency)

	for _, execution := range executions {
		runID := execution.GetExecution().GetRunId()
		startTime := toRunSummary(execution).StartTime
		status := execution.GetStatus().String()

		group.Go(func() error {
			history, err := fetchRunHistory(groupCtx, h.temporalClient, runID)

			mutex.Lock()
			defer mutex.Unlock()

			if err != nil {
				// Mask an upstream-fault (502/504-class) error the same way
				// writeError does before it reaches the client: fetchRunHistory's
				// raw Temporal/gRPC error can embed internal endpoint/host detail,
				// and this one is embedded in a per-run RunError inside an
				// otherwise-200 body rather than a status, so without this it would
				// bypass the masking every other endpoint applies (the per-run
				// getRunTapes masks it via writeHistoryError → writeError). The raw
				// error is logged server-side for diagnosis.
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
				})
			}

			return nil
		})
	}

	// group.Wait's error is always nil: every goroutine above returns nil
	// unconditionally and records its own failure in errs instead, so one
	// run's unreconstructable history degrades that run's entry rather than
	// aborting the others still in flight or failing this request (issue
	// #273's explicit degrade-per-run requirement).
	_ = group.Wait()

	// A done request context means the whole request timed out (requestTimeout)
	// or the client disconnected mid-flight — one infra failure, not N
	// independent per-run failures. Every in-flight history fetch then returned a
	// context error that was recorded as a per-run RunError above, so without
	// this check the handler would emit 200 with a pile of "context deadline
	// exceeded" rows and a possibly-empty tapes list, misreporting a gateway
	// timeout as a successful-but-degraded listing. Surface it as the status
	// statusForTemporalError maps it to (504 for the deadline, 499 for a client
	// disconnect), the same classification writeError gives a whole-response
	// Temporal failure.
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

		return tapes[i].CopyIndex < tapes[j].CopyIndex
	})
	sort.Slice(errs, func(i, j int) bool { return errs[i].RunID < errs[j].RunID })

	writeJSON(w, http.StatusOK, AggregateTapesResponse{Tapes: tapes, RunErrors: errs})
}

// listTapesRunLimit parses GET /api/tapes' `limit` query parameter: how many
// of the most recent runs to reconstruct. Absent/empty means
// defaultListTapesRunLimit; anything else must be a positive integer, capped
// at listPageSize (the most runs one visibility page returns anyway).
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

// deriveTapeOutcomes reconstructs every physical tape this run loaded and its
// outcome, by joining every completed Load activity's result (each a batch of
// backup.LoadedTape, library.go) with the FormatTape/WriteTree/FinalizeTape/
// MeasureWriteHealth activities recorded for that same barcode
// (activityRecord.Barcode, pre-extracted by history.go). One entry per
// barcode: a barcode identifies one physical tape for the run's whole
// lifetime (a Load/Write-failure retry loads a *fresh* blank — a different
// physical tape with its own barcode — onto the same slot, SPEC §4.3, so it
// naturally appears as its own separate entry here rather than overwriting
// the failed attempt).
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

		return outcomes[i].CopyIndex < outcomes[j].CopyIndex
	})

	return outcomes
}

// tapeOutcomeFor resolves one loaded tape's outcome by walking its write
// pipeline in order: FormatTape, then WriteTree, then FinalizeTape. The first
// of the three that failed/timed out/was canceled decides a "failed" outcome;
// FinalizeTape completing decides "written"; finding none of the three at all
// (Load succeeded but nothing progressed further — an in-progress or
// interrupted run) leaves the default "loaded".
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
		var health backup.WriteHealth
		if err := decodePayloads(record.Result, &health); err == nil {
			outcome.WriteHealth = toWriteHealthInfo(health)
		}
	}

	return outcome
}

// activityByBarcode finds the activity named name whose pre-extracted
// Barcode matches. At most one such activity is ever scheduled per (name,
// barcode) pair: FormatTape/WriteTree run at most once per physical tape
// (workflows/backup/session.go's noRetryOpts, MaximumAttempts: 1), and
// FinalizeTape's own internal retries (MaximumAttempts: 3) share a single
// ScheduledEventId, hence a single activityRecord.
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
