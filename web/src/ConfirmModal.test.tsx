import { type ComponentProps } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import ConfirmModal from './ConfirmModal'

function renderModal(props: Partial<ComponentProps<typeof ConfirmModal>> = {}) {
  return render(
    <ConfirmModal
      title="Cancel this run?"
      confirmLabel="Yes, cancel run"
      dismissLabel="Keep running"
      onConfirm={props.onConfirm ?? vi.fn()}
      onDismiss={props.onDismiss ?? vi.fn()}
      {...props}
    >
      body
    </ConfirmModal>,
  )
}

describe('ConfirmModal', () => {
  it('focuses the safe dismiss button on open', () => {
    renderModal()

    expect(screen.getByRole('button', { name: 'Keep running' })).toHaveFocus()
  })

  it('recovers focus into the dialog after a failed confirm re-enables the buttons', () => {
    // While sending, both buttons are disabled; disabling the focused confirm
    // button drops focus to <body>. When the confirm then fails, the modal stays
    // open with the buttons re-enabled — focus must return to the dialog so the
    // Escape/Tab handler (bound to the dialog) keeps working.
    const { rerender } = renderModal({ sending: true })

    // Focus is not inside the dialog while sending (disabled buttons can't hold
    // it) — simulate the browser having dropped it to <body>.
    ;(document.activeElement as HTMLElement | null)?.blur()
    expect(screen.getByRole('button', { name: 'Keep running' })).not.toHaveFocus()

    rerender(
      <ConfirmModal
        title="Cancel this run?"
        confirmLabel="Yes, cancel run"
        dismissLabel="Keep running"
        sending={false}
        error="Temporal is unreachable."
        onConfirm={vi.fn()}
        onDismiss={vi.fn()}
      >
        body
      </ConfirmModal>,
    )

    expect(screen.getByRole('button', { name: 'Keep running' })).toHaveFocus()
  })
})
