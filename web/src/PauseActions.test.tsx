import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import PauseActions from './PauseActions'

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('PauseActions', () => {
  it('renders nothing when the run is not paused', () => {
    const { container } = render(<PauseActions runId="run-1" pause={{ kind: '' }} />)

    expect(container).toBeEmptyDOMElement()
  })

  it('shows the write-failure pause detail with both Resume and Abort available', () => {
    render(
      <PauseActions
        runId="run-1"
        pause={{
          kind: 'write-failure',
          phase: 'Write',
          affectedTapes: ['TA0001L6'],
          reloadSlots: [101],
          errorSummary: 'mkltfs: drive reported a hard write error',
          canAbort: true,
        }}
      />,
    )

    expect(screen.getByText(/Load\/Write failure/)).toBeInTheDocument()
    expect(screen.getByText(/TA0001L6/)).toBeInTheDocument()
    expect(screen.getByText(/101/)).toBeInTheDocument()
    expect(screen.getByText(/hard write error/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^resume$/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^abort$/i })).toBeInTheDocument()
  })

  it('shows the eject pause detail with only Resume available', () => {
    render(
      <PauseActions
        runId="run-1"
        // canAbort omitted: eject is the one pause kind the server rejects
        // abort for (pkg/runsapi's pauseAcceptsAbort) — this component reads
        // that decision from the field rather than re-deriving it from kind.
        pause={{ kind: 'eject', affectedTapes: ['TA0001L6'], awaitingExport: 1 }}
      />,
    )

    expect(screen.getByText(/Eject — import\/export station full/)).toBeInTheDocument()
    // The label and count live in separate nodes (a bold label span + the
    // value), so match the whole line's text on its <p>.
    expect(
      screen.getByText(
        (_, element) => element?.tagName === 'P' && element.textContent === 'Tapes still awaiting export: 1',
      ),
    ).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^resume$/i })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^abort$/i })).not.toBeInTheDocument()
  })

  it('shows the burn pause detail with both actions available', () => {
    render(
      <PauseActions runId="run-1" pause={{ kind: 'burn', devices: ['/dev/sr0'], canAbort: true }} />,
    )

    expect(screen.getByText(/Burn phase/)).toBeInTheDocument()
    expect(screen.getByText(/\/dev\/sr0/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^resume$/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^abort$/i })).toBeInTheDocument()
  })

  it('renders a warning, not nothing, when the server could not determine pause status', () => {
    render(<PauseActions runId="run-1" pause={{ kind: '', unknown: true }} />)

    expect(screen.getByRole('alert')).toHaveTextContent(/pause status unavailable/i)
    expect(screen.queryByRole('button', { name: /^resume$/i })).not.toBeInTheDocument()
  })

  it('sends resume immediately on click, with no confirmation dialog', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 202,
      json: async () => ({ status: 'resume signal sent' }),
    })
    vi.stubGlobal('fetch', fetchMock)
    const confirmMock = vi.fn()
    vi.stubGlobal('confirm', confirmMock)

    render(<PauseActions runId="run-1" pause={{ kind: 'write-failure', phase: 'Write' }} />)

    fireEvent.click(screen.getByRole('button', { name: /^resume$/i }))

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith('/api/runs/run-1/resume', expect.objectContaining({ method: 'POST' }))
    })
    // Resume has no confirmation step — no native dialog, no modal.
    expect(confirmMock).not.toHaveBeenCalled()
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  })

  it('opens a modal to confirm abort — not a native dialog — and does not call the API until confirmed', () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    const confirmMock = vi.fn()
    vi.stubGlobal('confirm', confirmMock)

    render(<PauseActions runId="run-1" pause={{ kind: 'burn', devices: ['/dev/sr0'], canAbort: true }} />)

    fireEvent.click(screen.getByRole('button', { name: /^abort$/i }))

    expect(screen.getByRole('alertdialog')).toBeInTheDocument()
    expect(confirmMock).not.toHaveBeenCalled()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('does not call the API when the abort modal is dismissed with "Keep paused"', () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)

    render(<PauseActions runId="run-1" pause={{ kind: 'burn', canAbort: true }} />)

    fireEvent.click(screen.getByRole('button', { name: /^abort$/i }))
    fireEvent.click(screen.getByRole('button', { name: /keep paused/i }))

    expect(fetchMock).not.toHaveBeenCalled()
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  })

  it('sends abort once the modal is confirmed', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 202,
      json: async () => ({ status: 'abort signal sent' }),
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<PauseActions runId="run-1" pause={{ kind: 'burn', devices: ['/dev/sr0'], canAbort: true }} />)

    fireEvent.click(screen.getByRole('button', { name: /^abort$/i }))
    fireEvent.click(screen.getByRole('button', { name: /abort run/i }))

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith('/api/runs/run-1/abort', expect.objectContaining({ method: 'POST' }))
    })
  })

  it('shows a resume failure inline in the pause box', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({ error: 'run is not currently paused' }),
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<PauseActions runId="run-1" pause={{ kind: 'write-failure' }} />)

    fireEvent.click(screen.getByRole('button', { name: /^resume$/i }))

    await waitFor(() => {
      expect(screen.getByRole('alert')).toBeInTheDocument()
    })

    expect(screen.getByText('run is not currently paused')).toBeInTheDocument()
    // Resume shows its error inline, not in a modal.
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  })

  it('shows an abort failure inside the modal, which stays open to retry', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 502,
      json: async () => ({ error: 'signal RPC failed' }),
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<PauseActions runId="run-1" pause={{ kind: 'burn', canAbort: true }} />)

    fireEvent.click(screen.getByRole('button', { name: /^abort$/i }))
    fireEvent.click(screen.getByRole('button', { name: /abort run/i }))

    await waitFor(() => {
      expect(screen.getByText('signal RPC failed')).toBeInTheDocument()
    })

    // The dialog stays open with the error so the operator can retry or back out.
    expect(screen.getByRole('alertdialog')).toBeInTheDocument()
  })

  it('shows a network-failure message when resume’s fetch itself rejects', async () => {
    const fetchMock = vi.fn().mockRejectedValue(new TypeError('network down'))
    vi.stubGlobal('fetch', fetchMock)

    render(<PauseActions runId="run-1" pause={{ kind: 'burn' }} />)

    fireEvent.click(screen.getByRole('button', { name: /^resume$/i }))

    await waitFor(() => {
      expect(screen.getByRole('alert')).toBeInTheDocument()
    })

    expect(screen.getByText(/could not reach the server/i)).toBeInTheDocument()
  })
})
