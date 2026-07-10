import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import TapesPage from './TapesPage'
import { RouterProvider } from './router'

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

function renderTapesPage() {
  return render(
    <RouterProvider>
      <TapesPage />
    </RouterProvider>,
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('TapesPage', () => {
  it('always explains that the listing is derived from run history, not a live catalog', () => {
    vi.stubGlobal('fetch', vi.fn().mockReturnValue(new Promise(() => {})))

    renderTapesPage()

    expect(screen.getByText(/no persistent tape catalog/i)).toBeInTheDocument()
    expect(screen.getByText(/does not read live status from the tape changer/i)).toBeInTheDocument()
  })

  it('shows a loading state before the response arrives', () => {
    vi.stubGlobal('fetch', vi.fn().mockReturnValue(new Promise(() => {})))

    renderTapesPage()

    expect(screen.getByRole('status')).toHaveTextContent(/loading tapes/i)
  })

  it('shows an error state when the fetch fails', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(jsonResponse(500, { error: 'Temporal is unreachable.' })),
    )

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent('Temporal is unreachable.')
    })
  })

  it('shows an empty state instead of an empty table when no tapes are found', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(200, { tapes: [] })))

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByText(/no tapes to show yet/i)).toBeInTheDocument()
    })

    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })

  it('lists each tape with its barcode, a link to the run that wrote it, its tape/copy index, and its outcome', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          tapes: [
            {
              barcode: 'TA0001L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              slot: 1,
              result: 'written',
              runId: 'run-1',
              runStartTime: '2026-07-01T00:00:00Z',
              runStatus: 'Completed',
              writeHealth: {
                measured: true,
                throughputMBps: 142,
                floorMBps: 50,
                floorKnown: true,
                belowFloor: false,
                repositions: 0,
                repositionsMeasured: true,
                healthy: true,
              },
            },
          ],
        }),
      ),
    )

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByRole('table')).toBeInTheDocument()
    })

    expect(screen.getByText('TA0001L6')).toBeInTheDocument()
    expect(screen.getByText('written')).toBeInTheDocument()
    expect(screen.getByText(/tape 0 · copy 0/i)).toBeInTheDocument()
    expect(screen.getByText('142 MB/s')).toBeInTheDocument()

    const runLink = screen.getByRole('link', { name: 'run-1' })
    expect(runLink).toHaveAttribute('href', '/runs/run-1')
  })

  it('shows a below-floor warning and a TapeAlert warning on their respective tapes', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          tapes: [
            {
              barcode: 'TA0002L6',
              tapeIndex: 0,
              copyIndex: 1,
              driveIndex: 1,
              slot: 2,
              result: 'written',
              runId: 'run-2',
              runStartTime: '2026-07-02T00:00:00Z',
              runStatus: 'Completed',
              writeHealth: {
                measured: true,
                throughputMBps: 38,
                floorMBps: 50,
                floorKnown: true,
                belowFloor: true,
                repositionsMeasured: false,
                healthy: false,
              },
            },
            {
              barcode: 'TA0003L6',
              tapeIndex: 1,
              copyIndex: 0,
              driveIndex: 0,
              slot: 3,
              result: 'failed',
              error: 'drive reported a hard write error',
              runId: 'run-2',
              runStartTime: '2026-07-02T00:00:00Z',
              runStatus: 'Failed',
              writeHealth: {
                measured: true,
                throughputMBps: 60,
                floorKnown: false,
                belowFloor: false,
                repositionsMeasured: false,
                tapeAlertFlags: ['CLEANING_NEEDED'],
                healthy: false,
              },
            },
          ],
        }),
      ),
    )

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByText('failed')).toBeInTheDocument()
    })

    expect(screen.getByText(/below floor/i)).toBeInTheDocument()
    expect(screen.getByText(/tapealert/i)).toBeInTheDocument()
  })

  it('shows an unmeasured note when a tape has no write-health measurement', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          tapes: [
            {
              barcode: 'TA0004L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              slot: 1,
              result: 'loaded',
              runId: 'run-3',
              runStartTime: '2026-07-03T00:00:00Z',
              runStatus: 'Running',
            },
          ],
        }),
      ),
    )

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByText('loaded')).toBeInTheDocument()
    })

    expect(screen.getByText(/not measured/i)).toBeInTheDocument()
  })

  it('renders a non-fatal degradation notice listing runs that could not be reconstructed, alongside the tapes that could', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          tapes: [
            {
              barcode: 'TA0005L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              slot: 1,
              result: 'written',
              runId: 'run-4',
              runStartTime: '2026-07-04T00:00:00Z',
              runStatus: 'Completed',
            },
          ],
          runErrors: [{ runId: 'run-old', error: 'workflow history not found' }],
        }),
      ),
    )

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByText(/could not be reconstructed/i)).toBeInTheDocument()
    })

    expect(screen.getByText(/run-old/)).toBeInTheDocument()
    expect(screen.getByText(/workflow history not found/)).toBeInTheDocument()
    // The degraded run must not hide the tapes that DID reconstruct fine.
    expect(screen.getByText('TA0005L6')).toBeInTheDocument()
  })
})
