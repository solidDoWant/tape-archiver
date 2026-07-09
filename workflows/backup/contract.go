// Package backup defines the contract for the tape-archiver backup workflow:
// the Temporal type name it registers under, the task queue it runs on, and the
// queries it answers. These constants are the single source of truth shared
// between the worker that implements the workflow and clients (e.g. cmd/tapectl)
// that submit and inspect runs, so the two cannot drift.
//
// The workflow implementation itself is owned by a separate issue; this file
// holds only the names both sides must agree on.
package backup

const (
	// WorkflowID is the fixed Temporal workflow ID every backup run submits
	// under. Runs are a singleton on purpose: the backup model is serial (one
	// data worker on one storage host, one disk staging area — SPEC §4.2), so
	// all runs must be mutually exclusive. cmd/tapectl submits new runs under
	// this ID, and it is the single source of truth for anything that lists or
	// describes past/current executions via Temporal visibility (e.g.
	// pkg/runsapi, cmd/web's GET /api/runs and /api/runs/{runID}) so those
	// surfaces cannot drift from what tapectl actually submits under.
	WorkflowID = "backup"

	// WorkflowType is the Temporal workflow type name a backup run is started
	// under. The control worker registers the workflow under this name.
	WorkflowType = "Backup"

	// TaskQueue is the Temporal task queue the backup workflow runs on. The
	// workflow orchestrates a run from the control role (SPEC §4.1), so it runs
	// on the control queue; it dispatches bulk-data activities to the data
	// queue from there.
	TaskQueue = "control"

	// DataTaskQueue is the Temporal task queue the bulk-data activities run on.
	// The data worker runs on the storage host (SPEC §4.1) where the source data
	// lives; the control-side workflow dispatches data-phase activities here by
	// setting it as the activity task queue.
	DataTaskQueue = "data"

	// LastCompletedPhaseQuery is the Temporal query that returns the name of the
	// most recently completed workflow phase (SPEC §4.3), or an empty string if
	// no phase has completed yet. The workflow registers a handler for it so
	// operators can inspect progress without consulting the Temporal UI.
	LastCompletedPhaseQuery = "lastCompletedPhase"

	// OperatorResumeSignal is the Temporal signal an operator sends to resume a
	// paused run. It resumes both operator-in-the-loop pauses (SPEC §4.3): the
	// Eject phase paused because the import/export station filled (phase 8), and
	// the tape path paused because a Load or Write failed for one drive-set. It
	// carries no payload — its receipt means the operator has cleared the blocking
	// condition (removed the exported tapes, or swapped the suspect tapes for fresh
	// blanks in the same slots) and the run may continue. `tapectl resume` sends
	// it. On an Eject pause the workflow re-reads the changer
	// inventory and exports the remaining tapes into the freed slots; on a
	// write-path pause it re-drives only the failed tapes onto the fresh blanks.
	// For an Eject pause on libraries that report the import/export access bit the
	// workflow resumes automatically without this signal; it is the fallback for
	// those that do not, and the sole resume path for a write-path pause.
	OperatorResumeSignal = "operatorResume"

	// CurrentPauseQuery is the Temporal query that returns which operator-in-the-
	// loop pause, if any, is currently blocking the run (SPEC §4.3 phase 8, §4.3
	// phases 6-8, §10): the Eject phase's I/O-station-full pause, a Load/Write
	// failure on the tape path, or a Burn-phase pause. It returns the zero-value
	// CurrentPause (Kind PauseNone) when the run is not currently paused. The
	// workflow registers a handler for it so an operator (or the web UI —
	// docs/web-ui-design.md §3) can see that a run is paused and why without
	// consulting the Temporal UI event history. It is purely additive: the pause
	// logic itself, and the OperatorResumeSignal/OperatorAbortSignal contract
	// above, are unchanged by its existence.
	CurrentPauseQuery = "currentPause"

	// OperatorAbortSignal is the Temporal signal an operator sends to abort a run
	// paused because a Load or Write failed for one drive-set (SPEC §4.3): instead
	// of swapping in fresh blanks and resuming, the operator ends the run in a
	// defined, reported state with no further tapes written. It carries no payload.
	// `tapectl abort` sends it.
	OperatorAbortSignal = "operatorAbort"
)
