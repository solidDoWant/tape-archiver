import { useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { ApiError, apiFetch, describeNetworkError } from './api'

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
// explicit modal confirmation — a centered dialog over a blurred, dimmed
// backdrop (ConfirmCancelModal) that the operator must resolve by choosing
// "Yes, cancel run" or "Keep running", rather than a native window.confirm (which
// looks nothing like the page) or an easy-to-mis-click inline prompt. Like
// PauseActions it does not re-fetch or hold its own copy of run state — the
// resulting Canceled status arrives over RunDetail's live SSE stream within one
// poll interval, same as any other state change.
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
        <ConfirmCancelModal
          sending={state.status === 'sending'}
          error={state.status === 'error' ? state.error : undefined}
          onConfirm={() => void confirmCancel()}
          onDismiss={() => setState({ status: 'idle' })}
        />
      ) : null}
    </div>
  )
}

interface ConfirmCancelModalProps {
  sending: boolean
  error?: string
  onConfirm: () => void
  onDismiss: () => void
}

// ConfirmCancelModal is the centered confirmation dialog, portalled to
// document.body so it overlays the whole viewport (not clipped by the run
// page's scroll/stacking context) above a dimmed, blurred backdrop. It is
// modal: background scroll is locked, focus moves into the dialog and is
// trapped between its two buttons, and Escape backs out (equivalent to "Keep
// running"). The backdrop deliberately does NOT dismiss on click — a
// destructive, irreversible action should be resolved by an explicit button
// press, not a stray click-off. Focus starts on the safe "Keep running" button
// so a reflexive Enter/Space never triggers the cancel.
function ConfirmCancelModal({ sending, error, onConfirm, onDismiss }: ConfirmCancelModalProps) {
  const dialogRef = useRef<HTMLDivElement>(null)
  const keepRunningRef = useRef<HTMLButtonElement>(null)

  // Move focus into the dialog on open, restoring it to the previously focused
  // element (the "Cancel run" trigger) on close.
  useEffect(() => {
    const previouslyFocused = document.activeElement as HTMLElement | null
    keepRunningRef.current?.focus()

    return () => previouslyFocused?.focus()
  }, [])

  // Lock background scroll while the modal is open, so the page behind the
  // scrim can't be scrolled away.
  useEffect(() => {
    const previousOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'

    return () => {
      document.body.style.overflow = previousOverflow
    }
  }, [])

  // Escape backs out (unless a request is already in flight), and Tab is
  // trapped within the dialog's own focusable controls so keyboard focus can
  // never wander to the dimmed page behind it.
  const handleKeyDown = (event: React.KeyboardEvent) => {
    if (event.key === 'Escape' && !sending) {
      event.preventDefault()
      onDismiss()

      return
    }

    if (event.key !== 'Tab') {
      return
    }

    const focusable = dialogRef.current?.querySelectorAll<HTMLElement>('button:not([disabled])')

    if (!focusable || focusable.length === 0) {
      return
    }

    const first = focusable[0]
    const last = focusable[focusable.length - 1]

    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault()
      last.focus()
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault()
      first.focus()
    }
  }

  return createPortal(
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/55 p-4 backdrop-blur-sm"
      onKeyDown={handleKeyDown}
    >
      <div
        ref={dialogRef}
        role="alertdialog"
        aria-modal="true"
        aria-labelledby="cancel-run-title"
        aria-describedby="cancel-run-body"
        className="w-full max-w-md rounded-2xl border border-border bg-surface p-6 shadow-2xl"
      >
        <h2 id="cancel-run-title" className="text-[17px] font-semibold tracking-tight text-text">
          Cancel this run?
        </h2>

        <p id="cancel-run-body" className="mt-2 text-[13px] leading-relaxed text-text-dim">
          The run stops as soon as possible, tears down any tape mounts, and closes in a defined, reported
          state with no further tapes written. This can’t be undone.
        </p>

        {error ? (
          <div
            role="alert"
            className="mt-4 rounded-lg border border-red-line bg-red-bg p-2.5 text-[12px] text-red"
          >
            {error}
          </div>
        ) : null}

        <div className="mt-6 flex justify-end gap-2.5">
          <button
            ref={keepRunningRef}
            type="button"
            onClick={onDismiss}
            disabled={sending}
            className="rounded-lg border border-border-strong bg-surface px-4 py-2 text-[12.5px] font-medium text-text transition-colors enabled:cursor-pointer hover:bg-surface-2 disabled:opacity-50"
          >
            Keep running
          </button>
          <button
            type="button"
            onClick={onConfirm}
            disabled={sending}
            className="rounded-lg bg-red px-4 py-2 text-[12.5px] font-semibold text-bg transition-opacity enabled:cursor-pointer hover:opacity-85 disabled:opacity-50"
          >
            {sending ? 'Cancelling…' : 'Yes, cancel run'}
          </button>
        </div>
      </div>
    </div>,
    document.body,
  )
}

export default CancelRunButton
