import { useEffect, useState } from 'react'
import { ApiError, apiFetch, describeNetworkError } from './api'
import ConfirmModal from './ConfirmModal'

export interface CancelRunButtonProps {
  runId: string
}

// sentSafetyNetMs bounds how long the button sits in the transitional
// "Canceling…" state after a successful POST before falling back to idle,
// mirroring PauseActions' identically-named backstop. Normally the run reaches
// its terminal Canceled status over the live SSE stream first and RunOverview
// unmounts this button entirely; this timer only matters if that never lands.
const sentSafetyNetMs = 8000

type CancelState =
  | { status: 'idle' }
  | { status: 'confirming' }
  | { status: 'sending' }
  // sent: the cancel POST succeeded but the run has not yet reported Canceled
  // over SSE. The button stays disabled/"Canceling…" during this gap so it
  // cannot invite a redundant second cancel of an already-cancelling run.
  | { status: 'sent' }
  | { status: 'error'; error: string }

// CancelRunButton is the run page's "stop this run now" control for a still
// in-progress run (RunOverview.tsx renders it only for a non-terminal run). It
// POSTs /api/runs/{runID}/cancel (pkg/runsapi's cancelRun), which requests
// graceful Temporal cancellation: unlike PauseActions' Resume/Abort — pause
// signals that only apply once a run is already paused for the operator — cancel
// applies to any running execution, paused or not, and lets the workflow's
// deferred cleanup (LTFS unmount, ZFS hold release, the failure/cancellation
// Discord alert) run so the run closes in a defined, reported Canceled state
// rather than being killed mid-flight (SPEC §10).
//
// Cancelling is consequential and not undoable, so it is gated behind an
// explicit modal confirmation (the shared ConfirmModal — a centered dialog over
// a blurred, dimmed backdrop that the operator must resolve by choosing
// "Yes, cancel run" or "Keep running"), rather than a native window.confirm which
// looks nothing like the page. Like PauseActions it does not re-fetch or hold its
// own copy of run state — the resulting Canceled status arrives over RunDetail's
// live SSE stream within one poll interval, same as any other state change.
function CancelRunButton({ runId }: CancelRunButtonProps) {
  const [state, setState] = useState<CancelState>({ status: 'idle' })

  // Fall back to idle if the run never reports Canceled over SSE (which would
  // otherwise unmount this button first) — see sentSafetyNetMs.
  useEffect(() => {
    if (state.status !== 'sent') {
      return
    }

    const timer = setTimeout(() => setState({ status: 'idle' }), sentSafetyNetMs)

    return () => clearTimeout(timer)
  }, [state.status])

  const confirmCancel = async () => {
    setState({ status: 'sending' })

    try {
      await apiFetch(`/api/runs/${encodeURIComponent(runId)}/cancel`, { method: 'POST' })
      setState({ status: 'sent' })
    } catch (error) {
      const message = error instanceof ApiError ? error.message : describeNetworkError(error)
      setState({ status: 'error', error: message })
    }
  }

  const open = state.status === 'confirming' || state.status === 'sending' || state.status === 'error'
  const sent = state.status === 'sent'

  return (
    <div className="flex flex-col items-end gap-1.5">
      <button
        type="button"
        disabled={sent}
        onClick={() => setState({ status: 'confirming' })}
        className="cursor-pointer rounded-lg border border-red-line bg-red-bg px-4 py-2 text-[12.5px] font-medium text-red transition-opacity hover:opacity-80 disabled:cursor-default disabled:opacity-60"
      >
        {sent ? 'Canceling…' : 'Cancel run'}
      </button>

      {open ? (
        <ConfirmModal
          title="Cancel this run?"
          confirmLabel={state.status === 'sending' ? 'Canceling…' : 'Yes, cancel run'}
          dismissLabel="Keep running"
          tone="danger"
          sending={state.status === 'sending'}
          error={state.status === 'error' ? state.error : undefined}
          onConfirm={() => void confirmCancel()}
          onDismiss={() => setState({ status: 'idle' })}
        >
          The run stops as soon as possible, tears down any tape mounts, and closes in a defined, reported state
          with no further tapes written. This can't be undone.
        </ConfirmModal>
      ) : null}
    </div>
  )
}

export default CancelRunButton
