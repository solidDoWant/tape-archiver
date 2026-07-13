import { useEffect, useState } from 'react'
import { ApiError, apiFetch, describeNetworkError } from './api'
import ConfirmModal from './ConfirmModal'

// pauseSignature is a stable identity for a pause's actionable content. The
// transient "Resuming…/Aborting…" state (ActionState.sent) is cleared whenever
// it changes, so a run that re-pauses after a resume — because the operator did
// not actually fix the problem — drops the transitional line and shows the
// fresh pause's buttons, rather than staying stuck on "Resuming…".
function pauseSignature(pause: CurrentPauseInfo): string {
  return [
    pause.kind,
    pause.phase ?? '',
    (pause.affectedTapes ?? []).join(','),
    (pause.reloadSlots ?? []).join(','),
    pause.awaitingExport ?? '',
    (pause.devices ?? []).join(','),
    pause.errorSummary ?? '',
  ].join('|')
}

// sentSafetyNetMs bounds how long the card may sit in the transitional
// "Resuming…/Aborting…" state before it restores the buttons on its own. The
// signature reset above handles every re-pause the live stream can distinguish;
// this is the backstop for the one it cannot — an identical re-pause the
// server's ~2s poll never catches a not-paused frame between — so the operator
// can never be permanently locked out of acting. Comfortably longer than a poll
// interval so a normal resume/abort resolves (and unmounts this card) first.
const sentSafetyNetMs = 8000

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
  // sent: the signal was accepted, but the pause is still showing because the
  // live pause state (SSE, re-polled every ~2s server-side and only pushed on
  // change) has not yet observed the workflow act on it. The card holds a
  // "Resuming…/Aborting…" transitional state through that gap so the operator
  // sees their action took, rather than the buttons snapping back to idle while
  // the card lingers. It leaves this state three ways: the pause clears (the
  // parent unmounts this card), a different pause frame arrives (the
  // pauseSignature reset restores the buttons for the new pause — e.g. a
  // re-pause because the operator did not fix the problem), or the
  // sentSafetyNetMs timer fires (the backstop for an identical re-pause the
  // poll can't distinguish).
  | { status: 'sent'; verb: 'resume' | 'abort' }
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
// It does not re-fetch the run's state or decide on its own when the pause
// clears — whether the card shows at all is driven by the live pause prop
// RunDetail.tsx feeds it from the SSE stream (GET /api/events/runs/{runID}),
// whose server-side poll loop compares CurrentPause every ~2s and pushes an
// "update" only on change (pkg/runsapi/events.go). So a resume/abort clears
// the card only once that live state observes the workflow act on the signal —
// up to a poll interval later, plus however long the signal takes to process
// (longer for abort, which must reach the run's terminal state). To keep that
// gap from reading as "nothing happened", the component holds a transient
// "sent" flag after a successful signal and shows a "Resuming…/Aborting…" line
// in place of the buttons until the live state resolves the pause and this card
// unmounts.
function PauseActions({ runId, pause }: PauseActionsProps) {
  const [actionState, setActionState] = useState<ActionState>({ status: 'idle' })

  // When a new pause frame arrives (the live stream pushes currentPause only on
  // change), drop any post-action transient state so the card reflects the
  // fresh pause — this is what recovers the buttons when a run re-pauses after a
  // resume that did not fix the problem. Only the post-action states (sent /
  // error) are reset; an in-progress confirm/send is left alone. This is
  // React's "adjust state during render on prop change" pattern (guarded so it
  // runs once per change), not an effect.
  const signature = pauseSignature(pause)
  const [seenSignature, setSeenSignature] = useState(signature)
  if (signature !== seenSignature) {
    setSeenSignature(signature)
    if (actionState.status === 'sent' || actionState.status === 'error') {
      setActionState({ status: 'idle' })
    }
  }

  // Safety net for an identical re-pause the poll can't distinguish (see
  // sentSafetyNetMs): restore the buttons after a bounded wait so the operator
  // is never locked out. A normal resume/abort clears the pause and unmounts
  // this card well before the timer fires; the reset above handles every
  // distinguishable change sooner.
  useEffect(() => {
    if (actionState.status !== 'sent') {
      return
    }

    const timer = setTimeout(() => setActionState({ status: 'idle' }), sentSafetyNetMs)

    return () => clearTimeout(timer)
  }, [actionState])

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
      // Hold a 'sent' state (not 'idle') so the card shows a transitional
      // "Resuming…/Aborting…" until the live pause state catches up and the
      // parent unmounts this card — see the ActionState.sent comment.
      setActionState({ status: 'sent', verb })
    } catch (error) {
      const message = error instanceof ApiError ? error.message : describeNetworkError(error)
      setActionState({ status: 'error', verb, error: message })
    }
  }

  // The Abort flow (confirm → send → error) drives the modal; a resume failure
  // shows inline in the box below the buttons instead, since Resume has no modal
  // of its own to carry it. 'sent' is deliberately NOT part of abortActive: once
  // the abort signal is accepted the modal closes and the card itself shows the
  // "Aborting…" transitional state.
  const abortActive =
    actionState.status === 'confirming-abort' ||
    (actionState.status === 'sending' && actionState.verb === 'abort') ||
    (actionState.status === 'error' && actionState.verb === 'abort')
  const abortSending = actionState.status === 'sending' && actionState.verb === 'abort'
  const abortError = actionState.status === 'error' && actionState.verb === 'abort' ? actionState.error : undefined
  const resumeError = actionState.status === 'error' && actionState.verb === 'resume' ? actionState.error : undefined

  // Once a signal has been accepted, the card shows a transitional line in place
  // of the action buttons until the live state resolves the pause.
  const sentVerb = actionState.status === 'sent' ? actionState.verb : null

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

      {sentVerb ? (
        <div className="mt-3.5 flex items-center gap-2 text-[12.5px] text-text-dim">
          <span aria-hidden="true" className="inline-block h-3.5 w-3.5 animate-spin rounded-full border-[1.5px] border-border-strong border-t-transparent" />
          {sentVerb === 'resume' ? 'Resuming — waiting for the run to continue…' : 'Aborting — ending the run…'}
        </div>
      ) : (
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
      )}

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
