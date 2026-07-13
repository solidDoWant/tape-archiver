import { useState } from 'react'
import { ApiError, apiFetch, describeNetworkError } from './api'
import ConfirmModal from './ConfirmModal'

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
  // confirming-abort: the Abort confirmation modal is open (Resume has no
  // confirmation step — it acts immediately).
  | { status: 'confirming-abort' }
  | { status: 'sending'; verb: 'resume' | 'abort' }
  | { status: 'error'; verb: 'resume' | 'abort'; error: string }

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
// §4.3 phases 6-8, §10) and offers Resume / Abort, calling POST
// /api/runs/{runID}/resume or /abort (pkg/runsapi). Resume acts immediately —
// it just continues a run the operator already decided to keep. Abort ends the
// run with no further tapes written, so (per CLAUDE.md's Hardware and Safety
// framing — a consequential, not-undoable action against real tape hardware) it
// is gated behind an explicit modal confirmation (the shared ConfirmModal, the
// same full-screen dialog CancelRunButton uses). It renders nothing when
// pause.kind is "" and pause.unknown is falsy (confirmed not paused); when
// pause.unknown is true it renders a warning instead of silently rendering
// nothing, since a run genuinely awaiting an operator must never look identical
// to a healthy, unpaused run.
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
      <div role="alert" className="rounded-xl border border-amber-line bg-amber-bg px-5 py-[18px]">
        <div className="flex items-center gap-2.5">
          <span aria-hidden="true" className="text-[15px]">
            ⏸
          </span>
          <span className="text-[13.5px] font-semibold text-amber">Pause status unavailable</span>
        </div>
        <p className="mt-[7px] max-w-[560px] text-[12.5px] text-text-dim">
          This run may be waiting on an operator. Check <code className="font-mono text-[11.5px]">tapectl status</code>{' '}
          or retry shortly.
        </p>
      </div>
    )
  }

  if (pause.kind === '') {
    return null
  }

  const sending = actionState.status === 'sending'

  // performAction POSTs the resume/abort and reflects the outcome. Resume calls
  // it directly on click; Abort calls it only after the confirmation modal is
  // accepted.
  const performAction = async (verb: 'resume' | 'abort') => {
    setActionState({ status: 'sending', verb })

    try {
      await apiFetch(`/api/runs/${encodeURIComponent(runId)}/${verb}`, { method: 'POST' })
      setActionState({ status: 'idle' })
    } catch (error) {
      const message = error instanceof ApiError ? error.message : describeNetworkError(error)
      setActionState({ status: 'error', verb, error: message })
    }
  }

  // The Abort flow (confirm → send → error) drives the modal; a resume failure
  // shows inline in the box below the buttons instead, since Resume has no modal
  // of its own to carry it.
  const abortActive =
    actionState.status === 'confirming-abort' ||
    (actionState.status === 'sending' && actionState.verb === 'abort') ||
    (actionState.status === 'error' && actionState.verb === 'abort')
  const abortSending = actionState.status === 'sending' && actionState.verb === 'abort'
  const abortError = actionState.status === 'error' && actionState.verb === 'abort' ? actionState.error : undefined
  const resumeError = actionState.status === 'error' && actionState.verb === 'resume' ? actionState.error : undefined

  return (
    <div role="status" className="rounded-xl border border-amber-line bg-amber-bg px-5 py-[18px]">
      <div className="flex items-center gap-2.5">
        <span aria-hidden="true" className="text-[15px]">
          ⏸
        </span>
        <span className="text-[13.5px] font-semibold text-amber">Operator action required</span>
      </div>

      <div className="mt-[7px] max-w-[560px] space-y-1 text-[12.5px] text-text-dim">
        <p className="font-medium text-text">{pauseKindLabel(pause.kind)}</p>

        {pause.phase ? <p><span className="font-semibold">Failing phase:</span> {pause.phase}</p> : null}

        {pause.affectedTapes && pause.affectedTapes.length > 0 ? (
          <p><span className="font-semibold">Affected tapes:</span> {pause.affectedTapes.join(', ')}</p>
        ) : null}

        {pause.reloadSlots && pause.reloadSlots.length > 0 ? (
          <p><span className="font-semibold">Reload fresh blanks into slots:</span> {pause.reloadSlots.join(', ')}</p>
        ) : null}

        {typeof pause.awaitingExport === 'number' && pause.awaitingExport > 0 ? (
          <p><span className="font-semibold">Tapes still awaiting export:</span> {pause.awaitingExport}</p>
        ) : null}

        {pause.devices && pause.devices.length > 0 ? <p><span className="font-semibold">Burner devices:</span> {pause.devices.join(', ')}</p> : null}

        {pause.errorSummary ? <p><span className="font-semibold">Reason:</span> {pause.errorSummary}</p> : null}
      </div>

      <div className="mt-3.5 flex gap-2.5">
        <button
          type="button"
          onClick={() => void performAction('resume')}
          disabled={sending}
          className="rounded-lg bg-text px-4 py-2 text-[12.5px] font-semibold text-bg transition-all enabled:cursor-pointer enabled:hover:opacity-[0.88] enabled:hover:shadow-[0_4px_12px_rgba(0,0,0,0.15)] enabled:active:translate-y-px enabled:active:opacity-[0.82] enabled:active:shadow-none disabled:opacity-50"
        >
          {actionState.status === 'sending' && actionState.verb === 'resume' ? 'Resuming…' : 'Resume'}
        </button>

        {pause.canAbort ? (
          <button
            type="button"
            onClick={() => setActionState({ status: 'confirming-abort' })}
            disabled={sending}
            className="rounded-lg border-[1.5px] border-border-strong bg-surface px-4 py-2 text-[12.5px] font-medium text-text transition-all enabled:cursor-pointer enabled:hover:border-red enabled:hover:bg-red-bg enabled:hover:text-red enabled:active:translate-y-px enabled:active:bg-red-bg disabled:opacity-50"
          >
            Abort
          </button>
        ) : null}
      </div>

      {resumeError ? (
        <div role="alert" className="mt-3.5 rounded-lg border border-red-line bg-red-bg p-2.5 text-[12px] text-red">
          {resumeError}
        </div>
      ) : null}

      {abortActive ? (
        <ConfirmModal
          title="Abort this paused run?"
          confirmLabel={abortSending ? 'Aborting…' : 'Abort run'}
          dismissLabel="Keep paused"
          tone="danger"
          sending={abortSending}
          error={abortError}
          onConfirm={() => void performAction('abort')}
          onDismiss={() => setActionState({ status: 'idle' })}
        >
          The run ends in a defined, reported state with no further tapes written or discs burned. This can’t be
          undone.
        </ConfirmModal>
      ) : null}
    </div>
  )
}

export default PauseActions
