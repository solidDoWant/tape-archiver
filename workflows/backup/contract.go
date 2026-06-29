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

	// LastCompletedPhaseQuery is the Temporal query that returns the name of the
	// most recently completed workflow phase (SPEC §4.3), or an empty string if
	// no phase has completed yet. The workflow registers a handler for it so
	// operators can inspect progress without consulting the Temporal UI.
	LastCompletedPhaseQuery = "lastCompletedPhase"
)
