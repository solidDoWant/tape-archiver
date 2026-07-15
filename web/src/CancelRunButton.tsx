import { useState } from 'react'
import { ApiError, apiFetch, describeNetworkError } from './api'
import ConfirmModal from './ConfirmModal'

export interface CancelRunButtonProps {
  runId: string
}

type CancelState =
  | { status: 'idle' }
  | { status: 'confirming' }
  | { status: 'sending' }
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

  const confirmCancel = async () => {
    setState({ status: 'sending' })

    try {
      await apiFetch(`/api/runs/${encodeURIComponent(runId)}/cancel`, { method: 'POST' })
      setState({ status: 'idle' })
    } catch (error) {
      const message = error instanceof ApiError ? error.message : describeNetworkError(error)
      setState({ status: 'error', error: message })
    }
  }

  const open = state.status === 'confirming' || state.status === 'sending' || state.status === 'error'

  return (
    <div className="flex flex-col items-end gap-1.5">
      <button
        type="button"
        onClick={() => setState({ status: 'confirming' })}
        className="cursor-pointer rounded-lg border border-red-line bg-red-bg px-4 py-2 text-[12.5px] font-medium text-red transition-opacity hover:opacity-80"
      >
        Cancel run
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
