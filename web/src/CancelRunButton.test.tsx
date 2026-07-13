import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import CancelRunButton from './CancelRunButton'

afterEach(() => {
  vi.unstubAllGlobals()
})

// openModal renders the button and clicks it, returning once the confirmation
// dialog is on screen.
function openModal(runId = 'run-1') {
  render(<CancelRunButton runId={runId} />)
  fireEvent.click(screen.getByRole('button', { name: /^cancel run$/i }))

  return screen.getByRole('alertdialog')
}

describe('CancelRunButton', () => {
  it('renders a Cancel run button and no dialog initially', () => {
    render(<CancelRunButton runId="run-1" />)

    expect(screen.getByRole('button', { name: /^cancel run$/i })).toBeInTheDocument()
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  })

  it('opens a modal confirmation dialog — not a native window.confirm — before doing anything', () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    const confirmMock = vi.fn()
    vi.stubGlobal('confirm', confirmMock)

    const dialog = openModal()

    expect(dialog).toBeInTheDocument()
    expect(dialog).toHaveAttribute('aria-modal', 'true')
    // Focus starts on the safe option, so a reflexive Enter can't cancel.
    expect(screen.getByRole('button', { name: /keep running/i })).toHaveFocus()
    expect(confirmMock).not.toHaveBeenCalled()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('does not call the API when the operator backs out with "Keep running"', () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)

    openModal()
    fireEvent.click(screen.getByRole('button', { name: /keep running/i }))

    expect(fetchMock).not.toHaveBeenCalled()
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^cancel run$/i })).toBeInTheDocument()
  })

  it('dismisses on Escape (equivalent to Keep running) without calling the API', () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)

    const dialog = openModal()
    fireEvent.keyDown(dialog, { key: 'Escape' })

    expect(fetchMock).not.toHaveBeenCalled()
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  })

  it('does not dismiss when the backdrop is clicked (an explicit choice is required)', () => {
    const dialog = openModal()
    // The scrim is the dialog's parent; clicking it must not close the modal.
    const backdrop = dialog.parentElement as HTMLElement
    fireEvent.click(backdrop)

    expect(screen.getByRole('alertdialog')).toBeInTheDocument()
  })

  it('POSTs to the cancel endpoint once the cancellation is confirmed', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 202,
      json: async () => ({ status: 'cancel requested' }),
    })
    vi.stubGlobal('fetch', fetchMock)

    openModal('run 1')
    fireEvent.click(screen.getByRole('button', { name: /yes, cancel run/i }))

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        // runId is percent-encoded into the path.
        '/api/runs/run%201/cancel',
        expect.objectContaining({ method: 'POST' }),
      )
    })
  })

  it('shows the API error message in the dialog when the server rejects the cancel (e.g. 409 already closed)', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({ error: 'run is not in progress; only a running run can be cancelled' }),
    })
    vi.stubGlobal('fetch', fetchMock)

    openModal()
    fireEvent.click(screen.getByRole('button', { name: /yes, cancel run/i }))

    await waitFor(() => {
      expect(screen.getByRole('alert')).toBeInTheDocument()
    })

    // The dialog stays open with the error, so the operator can retry or back out.
    expect(screen.getByRole('alertdialog')).toBeInTheDocument()
    expect(screen.getByText(/only a running run can be cancelled/i)).toBeInTheDocument()
  })

  it('shows a network-failure message when fetch itself rejects', async () => {
    const fetchMock = vi.fn().mockRejectedValue(new TypeError('network down'))
    vi.stubGlobal('fetch', fetchMock)

    openModal()
    fireEvent.click(screen.getByRole('button', { name: /yes, cancel run/i }))

    await waitFor(() => {
      expect(screen.getByRole('alert')).toBeInTheDocument()
    })

    expect(screen.getByText(/could not reach the server/i)).toBeInTheDocument()
  })
})
