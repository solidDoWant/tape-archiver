import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import TapesSection from './TapesSection'

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('TapesSection', () => {
  it('lists each loaded tape with its slot, copy/tape index, and result', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          runId: 'run-1',
          tapes: [
            { barcode: 'TA0041L6', tapeIndex: 1, copyIndex: 0, driveIndex: 0, slot: 5, result: 'written' },
            {
              barcode: 'TA0042L6',
              tapeIndex: 1,
              copyIndex: 1,
              driveIndex: 1,
              slot: 6,
              result: 'failed',
              error: 'medium error',
              writeHealth: { measured: true, belowFloor: true, tapeAlertFlags: ['0x09'] },
            },
          ],
        }),
      ),
    )

    render(<TapesSection runId="run-1" terminal={false} />)

    await waitFor(() => {
      expect(screen.getByText('TA0041L6')).toBeInTheDocument()
    })
    expect(screen.getByText('slot 5')).toBeInTheDocument()
    expect(screen.getByText('written')).toBeInTheDocument()

    expect(screen.getByText('TA0042L6')).toBeInTheDocument()
    expect(screen.getByText('failed')).toBeInTheDocument()
    expect(screen.getByText('medium error')).toBeInTheDocument()
    expect(screen.getByText(/below floor/i)).toBeInTheDocument()
    expect(screen.getByText(/tapealert/i)).toBeInTheDocument()
  })

  it('shows a no-tapes-yet message when the run has loaded nothing', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(200, { runId: 'run-1', tapes: [] })))

    render(<TapesSection runId="run-1" terminal={false} />)

    await waitFor(() => {
      expect(screen.getByText(/no tapes have been loaded/i)).toBeInTheDocument()
    })
  })

  it('shows an unavailable notice once history has aged out (410)', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(410, { error: 'gone' })))

    render(<TapesSection runId="run-1" terminal />)

    await waitFor(() => {
      expect(screen.getByText(/no longer available/i)).toBeInTheDocument()
    })
  })

  it('refetches once the run transitions to terminal', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { runId: 'run-1', tapes: [] }))
    vi.stubGlobal('fetch', fetchMock)

    const { rerender } = render(<TapesSection runId="run-1" terminal={false} />)

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))

    rerender(<TapesSection runId="run-1" terminal />)

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
  })
})
