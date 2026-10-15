// Package backup implements the tape-archiver backup workflow and its
// activities. This file holds the names the worker and its clients (e.g.
// cmd/tapectl) must agree on.
package backup

const (
	// WorkflowID is the fixed workflow ID every run submits under. Runs are a
	// singleton on purpose: the backup model is serial (one data worker, one staging
	// area — SPEC §4.2). Anything listing runs via visibility (pkg/runsapi, cmd/web)
	// uses it too.
	WorkflowID = "backup"

	// WorkflowType is the workflow type name the control worker registers.
	WorkflowType = "Backup"

	// TaskQueue is the control queue the workflow runs on (SPEC §4.1).
	TaskQueue = "control"

	// DataTaskQueue is the queue of the data worker on the storage host
	// (SPEC §4.1), where bulk-data activities run.
	DataTaskQueue = "data"

	// LastCompletedPhaseQuery returns the most recently completed phase name
	// (SPEC §4.3), or "" if none.
	LastCompletedPhaseQuery = "lastCompletedPhase"

	// OperatorResumeSignal resumes a paused run (SPEC §4.3) after the operator
	// cleared the blocker: removed exported tapes from a full I/O station, or
	// swapped failed tapes for fresh blanks in the same slots. An Eject pause also
	// auto-resumes on libraries that report the I/O access bit; a write-path pause
	// resumes only via this signal. Sent by `tapectl resume`; no payload.
	OperatorResumeSignal = "operatorResume"

	// CurrentPauseQuery returns which operator pause, if any, is blocking the run
	// (Eject, tape path, or Burn); the zero CurrentPause means not paused.
	CurrentPauseQuery = "currentPause"

	// OperatorAbortSignal ends a run paused on a Load/Write failure (SPEC §4.3) in a
	// defined, reported state with no further tapes written. Sent by
	// `tapectl abort`; no payload.
	OperatorAbortSignal = "operatorAbort"
)
