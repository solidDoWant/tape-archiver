import { useState } from 'react'
import { apiFetch, ApiError } from './api'

// CurrentPauseInfo mirrors pkg/runsapi.CurrentPauseInfo (the GET
// /api/runs/{runID} and SSE JSON projection of workflows/backup's
// CurrentPause query result): which operator-in-the-loop pause, if any, is
// blocking a run right now, and enough context to act on it. Kind is "" when
// the run is not paused.
export interface CurrentPauseInfo {
  kind: string
  phase?: string
  affectedTapes?: string[]
  reloadSlots?: number[]
  awaitingExport?: number
  devices?: string[]
  errorSummary?: string
}

export interface PauseActionsProps {
  runId: string
  pause: CurrentPauseInfo
}

type ActionState =
  | { status: 'idle' }
  | { status: 'sending'; verb: 'resume' | 'abort' }
  | { status: 'error'; error: string }

// pausesAbortApplies lists the pause kinds workflows/backup's
// OperatorAbortSignal actually applies to (write-failure, burn) — an Eject
// pause never listens for it (every tape is already safely written by the
// time it pauses), and pkg/runsapi's POST /api/runs/{runID}/abort rejects it
// with 409 for that reason. Hiding the Abort button for an eject pause here
// is a UX nicety on top of that server-side rejection, not a substitute for
// it — the button is a courtesy to keep an operator from getting a
// confusing 409, not the source of truth for what is allowed.
const pausesAbortApplies = new Set(['write-failure', 'burn'])

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

// describeNetworkError renders a non-ApiError failure (fetch itself
// rejecting, e.g. the server is unreachable) as operator-facing text,
// mirroring SubmitRunForm.tsx's equivalent handling.
function describeNetworkError(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error)

  return `Could not reach the server: ${message}`
}

// PauseActions shows why a run is currently paused (SPEC §4.3 phase 8, SPEC
// §4.3 phases 6-8, §10) and offers Resume / Abort, each gated behind a
// confirmation (CLAUDE.md's Hardware and Safety framing: these are
// consequential, hard-to-reverse actions against real tape hardware) before
// calling POST /api/runs/{runID}/resume or /abort (pkg/runsapi). It renders
// nothing when pause.kind is "" (not paused).
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
          {sending && actionState.status === 'sending' && actionState.verb === 'resume'
            ? 'Resuming…'
            : 'Resume'}
        </button>

        {pausesAbortApplies.has(pause.kind) ? (
          <button
            type="button"
            onClick={() => void handleAction('abort')}
            disabled={sending}
            className="rounded bg-red-700 px-3 py-1.5 font-medium text-white disabled:opacity-50"
          >
            {sending && actionState.status === 'sending' && actionState.verb === 'abort'
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
