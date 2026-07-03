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

	// OperatorEjectClearedSignal is the Temporal signal an operator sends to
	// resume a run paused in the Eject phase because the import/export station
	// filled (SPEC §4.3 phase 8). It carries no payload — its receipt is the
	// signal that the operator has removed the exported tapes and cleared the
	// station. `tapectl resume <workflow-id>` sends it. The workflow re-reads the
	// changer inventory and exports the remaining tapes into the freed slots. For
	// libraries that report the import/export access bit the workflow resumes
	// automatically without this signal; it is the fallback for those that do not.
	OperatorEjectClearedSignal = "operatorEjectCleared"
)
