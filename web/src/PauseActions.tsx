import { useState } from 'react'
import { apiFetch, ApiError, describeNetworkError } from './api'

// CurrentPauseInfo mirrors pkg/runsapi.CurrentPauseInfo (the GET
// /api/runs/{runID} and SSE JSON projection of workflows/backup's
// CurrentPause query result): which operator-in-the-loop pause, if any, is
// blocking a run right now, and enough context to act on it. Kind is "" when
// the run is not paused. unknown is true when the server's CurrentPauseQuery
// itself failed (e.g. no worker currently polling) rather than confirming
// the run isn't paused — kind === '' alone cannot distinguish the two, so a
// caller must check unknown before treating an empty kind as "not paused".
// canAbort mirrors the server's own abortRun rejection rule (pkg/runsapi's
// pauseAcceptsAbort) — always read from here rather than re-deriving from
// kind, so this component can never drift from what the server actually
// accepts.
export interface CurrentPauseInfo {
  kind: string
  phase?: string
  affectedTapes?: string[]
  reloadSlots?: number[]
  awaitingExport?: number
  devices?: string[]
  errorSummary?: string
  canAbort?: boolean
  unknown?: boolean
}

export interface PauseActionsProps {
  runId: string
  pause: CurrentPauseInfo
}

type ActionState =
  | { status: 'idle' }
  | { status: 'sending'; verb: 'resume' | 'abort' }
  | { status: 'error'; error: string }

// pauseKindLabel renders a pause Kind ("eject" | "write-failure" | "burn")
// as operator-facing text.
function pauseKindLabel(kind: string): string {
  switch (kind) {
    case 'eject':
      return 'Eject — import/export station full'
    case 'write-failure':
      return 'Load/Write failure'
    case 'burn':
      return 'Burn phase'
    default:
      return kind
  }
}

// PauseActions shows why a run is currently paused (SPEC §4.3 phase 8, SPEC
// §4.3 phases 6-8, §10) and offers Resume / Abort, each gated behind a
// confirmation (CLAUDE.md's Hardware and Safety framing: these are
// consequential, hard-to-reverse actions against real tape hardware) before
// calling POST /api/runs/{runID}/resume or /abort (pkg/runsapi). It renders
// nothing when pause.kind is "" and pause.unknown is falsy (confirmed not
// paused); when pause.unknown is true it renders a warning instead of
// silently rendering nothing, since a run genuinely awaiting an operator
// must never look identical to a healthy, unpaused run.
//
// It intentionally does not re-fetch or hold its own copy of the run's
// state: RunDetail.tsx (its only caller) already receives live pause state
// over the SSE stream (GET /api/events/runs/{runID}) — the poll loop behind
// that stream compares CurrentPause on every tick (pkg/runsapi/events.go),
// so a resume/abort this component sends shows up as a fresh "update" event
// within one poll interval, same as any other state change, without a
// manual refresh or a second fetch call here.
function PauseActions({ runId, pause }: PauseActionsProps) {
  const [actionState, setActionState] = useState<ActionState>({ status: 'idle' })

  if (pause.unknown) {
    return (
      <div
        role="alert"
        className="rounded border border-amber-500 bg-amber-50 p-3 text-amber-900 dark:border-amber-400 dark:bg-amber-950 dark:text-amber-100"
      >
        Pause status unavailable right now — this run may be waiting on an
        operator. Check <code>tapectl status</code> or retry shortly.
      </div>
    )
  }

  if (pause.kind === '') {
    return null
  }

  const sending = actionState.status === 'sending'

  const handleAction = async (verb: 'resume' | 'abort') => {
    const confirmed = window.confirm(
      verb === 'resume'
        ? 'Resume this paused run? The operator-cleared blocking condition must actually be cleared first (SPEC §4.3).'
        : 'Abort this paused run? It ends in a defined, reported state with no further tapes written. This cannot be undone.',
    )

    if (!confirmed) {
      return
    }

    setActionState({ status: 'sending', verb })

    try {
      await apiFetch(`/api/runs/${encodeURIComponent(runId)}/${verb}`, { method: 'POST' })
      setActionState({ status: 'idle' })
    } catch (error) {
      const message = error instanceof ApiError ? error.message : describeNetworkError(error)
      setActionState({ status: 'error', error: message })
    }
  }

  return (
    <div
      role="status"
      className="flex flex-col gap-2 rounded border border-amber-500 bg-amber-50 p-3 text-amber-900 dark:border-amber-400 dark:bg-amber-950 dark:text-amber-100"
    >
      <p className="font-semibold">Paused: {pauseKindLabel(pause.kind)}</p>

      {pause.phase ? <p>Failing phase: {pause.phase}</p> : null}

      {pause.affectedTapes && pause.affectedTapes.length > 0 ? (
        <p>Affected tapes: {pause.affectedTapes.join(', ')}</p>
      ) : null}

      {pause.reloadSlots && pause.reloadSlots.length > 0 ? (
        <p>Reload fresh blanks into slots: {pause.reloadSlots.join(', ')}</p>
      ) : null}

      {typeof pause.awaitingExport === 'number' && pause.awaitingExport > 0 ? (
        <p>Tapes still awaiting export: {pause.awaitingExport}</p>
      ) : null}

      {pause.devices && pause.devices.length > 0 ? <p>Burner devices: {pause.devices.join(', ')}</p> : null}

      {pause.errorSummary ? <p>Reason: {pause.errorSummary}</p> : null}

      <div className="flex gap-2 pt-1">
        <button
          type="button"
          onClick={() => void handleAction('resume')}
          disabled={sending}
          className="rounded bg-green-700 px-3 py-1.5 font-medium text-white disabled:opacity-50"
        >
          {actionState.status === 'sending' && actionState.verb === 'resume'
            ? 'Resuming…'
            : 'Resume'}
        </button>

        {pause.canAbort ? (
          <button
            type="button"
            onClick={() => void handleAction('abort')}
            disabled={sending}
            className="rounded bg-red-700 px-3 py-1.5 font-medium text-white disabled:opacity-50"
          >
            {actionState.status === 'sending' && actionState.verb === 'abort'
              ? 'Aborting…'
              : 'Abort'}
          </button>
        ) : null}
      </div>

      {actionState.status === 'error' ? (
        <div
          role="alert"
          className="rounded border border-red-600 bg-red-50 p-2 text-red-900 dark:border-red-500 dark:bg-red-950 dark:text-red-100"
        >
          {actionState.error}
        </div>
      ) : null}
    </div>
  )
}

export default PauseActions
