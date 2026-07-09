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
        pause={{ kind: 'eject', affectedTapes: ['TA0001L6'], awaitingExport: 1 }}
      />,
    )

    expect(screen.getByText(/Eject — import\/export station full/)).toBeInTheDocument()
    expect(screen.getByText(/awaiting export: 1/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^resume$/i })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^abort$/i })).not.toBeInTheDocument()
  })

  it('shows the burn pause detail with both actions available', () => {
    render(<PauseActions runId="run-1" pause={{ kind: 'burn', devices: ['/dev/sr0'] }} />)

    expect(screen.getByText(/Burn phase/)).toBeInTheDocument()
    expect(screen.getByText(/\/dev\/sr0/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^resume$/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^abort$/i })).toBeInTheDocument()
  })

  it('asks for confirmation before sending resume, and does not call the API when declined', () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(false))

    render(<PauseActions runId="run-1" pause={{ kind: 'burn' }} />)

    fireEvent.click(screen.getByRole('button', { name: /^resume$/i }))

    expect(window.confirm).toHaveBeenCalled()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('sends resume after confirmation', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 202,
      json: async () => ({ status: 'resume signal sent' }),
    })
    vi.stubGlobal('fetch', fetchMock)
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))

    render(<PauseActions runId="run-1" pause={{ kind: 'write-failure', phase: 'Write' }} />)

    fireEvent.click(screen.getByRole('button', { name: /^resume$/i }))

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/runs/run-1/resume',
        expect.objectContaining({ method: 'POST' }),
      )
    })
  })

  it('sends abort after confirmation', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 202,
      json: async () => ({ status: 'abort signal sent' }),
    })
    vi.stubGlobal('fetch', fetchMock)
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))

    render(<PauseActions runId="run-1" pause={{ kind: 'burn', devices: ['/dev/sr0'] }} />)

    fireEvent.click(screen.getByRole('button', { name: /^abort$/i }))

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/runs/run-1/abort',
        expect.objectContaining({ method: 'POST' }),
      )
    })
  })

  it('shows the API error message when an action is rejected (e.g. 409 not paused)', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({ error: 'run is not currently paused' }),
    })
    vi.stubGlobal('fetch', fetchMock)
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))

    render(<PauseActions runId="run-1" pause={{ kind: 'write-failure' }} />)

    fireEvent.click(screen.getByRole('button', { name: /^resume$/i }))

    await waitFor(() => {
      expect(screen.getByRole('alert')).toBeInTheDocument()
    })

    expect(screen.getByText('run is not currently paused')).toBeInTheDocument()
  })

  it('shows a network-failure message when fetch itself rejects', async () => {
    const fetchMock = vi.fn().mockRejectedValue(new TypeError('network down'))
    vi.stubGlobal('fetch', fetchMock)
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))

    render(<PauseActions runId="run-1" pause={{ kind: 'burn' }} />)

    fireEvent.click(screen.getByRole('button', { name: /^resume$/i }))

    await waitFor(() => {
      expect(screen.getByRole('alert')).toBeInTheDocument()
    })

    expect(screen.getByText(/could not reach the server/i)).toBeInTheDocument()
  })
})
