import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import LogPanel from './LogPanel'

// jsonResponse builds a fetch Response stand-in, mirroring RunHistory.test.tsx's
// helper of the same shape.
function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

function line(time: string, level: string, message: string) {
  return { time, level, message }
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('LogPanel', () => {
  it('shows a loading state before the first fetch resolves', () => {
    vi.stubGlobal('fetch', vi.fn().mockReturnValue(new Promise(() => {})))

    render(<LogPanel runId="run-1" />)

    expect(screen.getByRole('status')).toHaveTextContent(/loading logs/i)
  })

  it('fetches the whole-run window when no phase is given', () => {
    const fetchMock = vi.fn().mockReturnValue(new Promise(() => {}))
    vi.stubGlobal('fetch', fetchMock)

    render(<LogPanel runId="run-1" />)

    expect(fetchMock).toHaveBeenCalledWith('/api/runs/run-1/logs', undefined)
  })

  it('scopes the request to the given phase', () => {
    const fetchMock = vi.fn().mockReturnValue(new Promise(() => {}))
    vi.stubGlobal('fetch', fetchMock)

    render(<LogPanel runId="run-1" phase="Write" />)

    expect(fetchMock).toHaveBeenCalledWith('/api/runs/run-1/logs?phase=Write', undefined)
  })

  it('shows the explicit unavailable state on a 503, not an error dump', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(jsonResponse(503, { error: 'victorialogs is not configured or is unreachable' })),
    )

    render(<LogPanel runId="run-1" />)

    await waitFor(() => {
      expect(screen.getByText(/logs unavailable/i)).toBeInTheDocument()
    })
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('shows a distinct error state for a non-503 failure', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(400, { error: 'invalid run ID' })))

    render(<LogPanel runId="run-1" />)

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/invalid run id/i)
    })
  })

  it('shows an empty message when the window has no lines yet', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(200, { runId: 'run-1', lines: [], live: false })))

    render(<LogPanel runId="run-1" phase="Burn" />)

    await waitFor(() => {
      expect(screen.getByRole('log')).toHaveTextContent(/no log lines/i)
    })
  })

  it('renders matched lines in order', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          runId: 'run-1',
          live: false,
          lines: [
            line('2026-07-10T12:00:00Z', 'INFO', 'resolving snapshots'),
            line('2026-07-10T12:01:00Z', 'WARN', 'pack slow'),
          ],
        }),
      ),
    )

    render(<LogPanel runId="run-1" />)

    await waitFor(() => {
      expect(screen.getByText('resolving snapshots')).toBeInTheDocument()
    })

    const log = screen.getByRole('log')
    const text = log.textContent ?? ''
    expect(text.indexOf('resolving snapshots')).toBeLessThan(text.indexOf('pack slow'))
  })

  it('polls for new lines with ?since= while live, and appends them', async () => {
    vi.useFakeTimers()

    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        jsonResponse(200, {
          runId: 'run-1',
          live: true,
          lines: [line('2026-07-10T12:00:00Z', 'INFO', 'first line')],
        }),
      )
      .mockResolvedValueOnce(
        jsonResponse(200, {
          runId: 'run-1',
          live: false,
          lines: [line('2026-07-10T12:01:00Z', 'INFO', 'second line')],
        }),
      )
    vi.stubGlobal('fetch', fetchMock)

    render(<LogPanel runId="run-1" />)

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    await vi.waitFor(() => expect(screen.getByText('first line')).toBeInTheDocument())

    await vi.advanceTimersByTimeAsync(3000)

    expect(fetchMock).toHaveBeenNthCalledWith(2, '/api/runs/run-1/logs?since=2026-07-10T12%3A00%3A00Z', undefined)
    await vi.waitFor(() => expect(screen.getByText('second line')).toBeInTheDocument())

    // Both lines are still present — a poll appends rather than replaces.
    expect(screen.getByText('first line')).toBeInTheDocument()

    // live: false on the second response means no third poll is scheduled.
    await vi.advanceTimersByTimeAsync(10000)
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('keeps showing existing lines and quietly retries after a mid-stream poll failure', async () => {
    vi.useFakeTimers()

    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        jsonResponse(200, { runId: 'run-1', live: true, lines: [line('2026-07-10T12:00:00Z', 'INFO', 'ok so far')] }),
      )
      .mockRejectedValueOnce(new TypeError('network down'))
      .mockResolvedValueOnce(
        jsonResponse(200, { runId: 'run-1', live: false, lines: [line('2026-07-10T12:01:00Z', 'INFO', 'recovered')] }),
      )
    vi.stubGlobal('fetch', fetchMock)

    render(<LogPanel runId="run-1" />)

    await vi.waitFor(() => expect(screen.getByText('ok so far')).toBeInTheDocument())

    await vi.advanceTimersByTimeAsync(3000)
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))

    // The failed poll must not blow away what's already shown, nor show an
    // error/unavailable state over a transient hiccup.
    expect(screen.getByText('ok so far')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByText(/logs unavailable/i)).not.toBeInTheDocument()

    await vi.advanceTimersByTimeAsync(3000)
    await vi.waitFor(() => expect(screen.getByText('recovered')).toBeInTheDocument())
  })

  it('stops polling and re-fetches from scratch when the run ID changes', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { runId: 'run-1', lines: [], live: false }))
    vi.stubGlobal('fetch', fetchMock)

    const { rerender } = render(<LogPanel runId="run-1" />)
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))

    rerender(<LogPanel runId="run-2" />)

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
    expect(fetchMock).toHaveBeenNthCalledWith(2, '/api/runs/run-2/logs', undefined)
  })
})
