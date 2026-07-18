import { useEffect, useId, useRef, type KeyboardEvent, type ReactNode } from 'react'
import { createPortal } from 'react-dom'

export interface ConfirmModalProps {
  // title/children are the dialog heading and body (the consequences of the
  // action). confirmLabel/dismissLabel are the two buttons' text.
  title: string
  confirmLabel: string
  dismissLabel: string
  children: ReactNode
  // tone styles the confirm button: 'danger' (a destructive action — red) or
  // 'primary' (the app's primary fill). Defaults to 'danger'.
  tone?: 'danger' | 'primary'
  // sending disables both buttons and is the caller's cue to render an
  // in-progress confirmLabel while the request is in flight.
  sending?: boolean
  // error, when set, is shown inside the dialog (so the operator can retry or
  // back out) rather than dismissing it.
  error?: string
  onConfirm: () => void
  onDismiss: () => void
}

// ConfirmModal is the app's shared confirmation dialog for a consequential,
// not-undoable action: a centered card over a dimmed, blurred, full-screen
// backdrop, rather than a native window.confirm (which looks nothing like the
// page). CancelRunButton and PauseActions' Resume/Abort all render it, so those
// confirmations look and behave identically.
//
// It is modal: background scroll is locked, focus moves into the dialog and is
// trapped between its two buttons, and Escape backs out (equivalent to the
// dismiss button). The backdrop deliberately does NOT dismiss on click — a
// destructive action should be resolved by an explicit button press, not a
// stray click-off. Focus starts on the safe dismiss button so a reflexive
// Enter/Space never triggers the confirm.
function ConfirmModal({
  title,
  confirmLabel,
  dismissLabel,
  children,
  tone = 'danger',
  sending = false,
  error,
  onConfirm,
  onDismiss,
}: ConfirmModalProps) {
  const dialogRef = useRef<HTMLDivElement>(null)
  const dismissRef = useRef<HTMLButtonElement>(null)
  const titleId = useId()
  const bodyId = useId()

  // Move focus into the dialog on open, restoring it to the previously focused
  // element (the trigger button) on close.
  useEffect(() => {
    const previouslyFocused = document.activeElement as HTMLElement | null
    dismissRef.current?.focus()

    return () => previouslyFocused?.focus()
  }, [])

  // Recover focus into the dialog whenever it has escaped while the controls are
  // live. Clicking the confirm button disables both buttons during `sending`;
  // disabling the focused button drops focus to <body>, and after a *failed*
  // confirm the modal stays open with the buttons re-enabled but focus still on
  // <body>. Because the Escape/Tab handler is bound to the dialog (handleKeyDown
  // below), keystrokes from <body> never reach it, so Escape/Tab would be dead
  // until a mouse click. Pulling focus back to the dismiss button restores both.
  useEffect(() => {
    if (sending) {
      return
    }

    const dialog = dialogRef.current
    if (dialog && !dialog.contains(document.activeElement)) {
      dismissRef.current?.focus()
    }
  }, [sending])

  // Lock background scroll while the modal is open.
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
  const handleKeyDown = (event: KeyboardEvent) => {
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

  const confirmToneClass = tone === 'primary' ? 'bg-text' : 'bg-red'

  return createPortal(
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/55 p-4 backdrop-blur-sm"
      onKeyDown={handleKeyDown}
    >
      <div
        ref={dialogRef}
        role="alertdialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={bodyId}
        className="w-full max-w-md rounded-2xl border border-border bg-surface p-6 shadow-elevated"
      >
        <h2 id={titleId} className="text-[17px] font-semibold tracking-tight text-text">
          {title}
        </h2>

        <div id={bodyId} className="mt-2 text-[13px] leading-relaxed text-text-dim">
          {children}
        </div>

        {error ? (
          <div role="alert" className="mt-4 rounded-lg border border-red-line bg-red-bg p-2.5 text-[12px] text-red">
            {error}
          </div>
        ) : null}

        <div className="mt-6 flex justify-end gap-2.5">
          <button
            ref={dismissRef}
            type="button"
            onClick={onDismiss}
            disabled={sending}
            className="rounded-lg border border-border-strong bg-surface px-4 py-2 text-[12.5px] font-medium text-text transition-colors enabled:cursor-pointer hover:bg-surface-2 disabled:opacity-50"
          >
            {dismissLabel}
          </button>
          <button
            type="button"
            onClick={onConfirm}
            disabled={sending}
            className={`rounded-lg ${confirmToneClass} px-4 py-2 text-[12.5px] font-semibold text-bg transition-opacity enabled:cursor-pointer enabled:hover:opacity-90 disabled:opacity-50`}
          >
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>,
    document.body,
  )
}

export default ConfirmModal
