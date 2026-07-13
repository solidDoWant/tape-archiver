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

  it('shows a line’s error detail under its message', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          runId: 'run-1',
          live: false,
          lines: [
            {
              time: '2026-07-10T12:00:00Z',
              level: 'ERROR',
              message: 'Activity error.',
              error: 'resolve sources[0] k8s snapshot: get VolumeSnapshot asdf/asdf: boom',
            },
          ],
        }),
      ),
    )

    render(<LogPanel runId="run-1" />)

    // Both the terse message and the actual cause are shown.
    await waitFor(() => {
      expect(screen.getByText('Activity error.')).toBeInTheDocument()
    })
    expect(screen.getByText(/resolve sources\[0\] k8s snapshot.*boom/)).toBeInTheDocument()
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
      .mockResolvedValue(jsonResponse(200, { runId: 'run-1', live: false, lines: [] }))
    vi.stubGlobal('fetch', fetchMock)

    render(<LogPanel runId="run-1" />)

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    await vi.waitFor(() => expect(screen.getByText('first line')).toBeInTheDocument())

    await vi.advanceTimersByTimeAsync(3000)

    expect(fetchMock).toHaveBeenNthCalledWith(2, '/api/runs/run-1/logs?since=2026-07-10T12%3A00%3A00Z', undefined)
    await vi.waitFor(() => expect(screen.getByText('second line')).toBeInTheDocument())

    // Both lines are still present — a poll appends rather than replaces.
    expect(screen.getByText('first line')).toBeInTheDocument()

    // live:false does not stop polling on the spot: log shipping is
    // asynchronous, so exactly one delayed catch-up poll runs first...
    await vi.advanceTimersByTimeAsync(5000)
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3))

    // ...and once that catch-up also reports live:false, polling stops for
    // good.
    await vi.advanceTimersByTimeAsync(120000)
    expect(fetchMock).toHaveBeenCalledTimes(3)
  })

  it('appends late lines found by the single post-live catch-up poll', async () => {
    vi.useFakeTimers()

    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        jsonResponse(200, {
          runId: 'run-1',
          live: false,
          lines: [line('2026-07-10T12:00:00Z', 'INFO', 'body line')],
        }),
      )
      .mockResolvedValueOnce(
        jsonResponse(200, {
          runId: 'run-1',
          live: false,
          lines: [line('2026-07-10T12:00:05Z', 'ERROR', 'final error summary')],
        }),
      )
    vi.stubGlobal('fetch', fetchMock)

    render(<LogPanel runId="run-1" />)

    await vi.waitFor(() => expect(screen.getByText('body line')).toBeInTheDocument())

    // The trailing lines a batched shipper had not delivered yet when the
    // live:false response was served — often the run's final summary or
    // error, the most operator-relevant lines — arrive via the catch-up.
    await vi.advanceTimersByTimeAsync(5000)
    await vi.waitFor(() => expect(screen.getByText('final error summary')).toBeInTheDocument())
    expect(screen.getByText('body line')).toBeInTheDocument()

    // Exactly one catch-up; the panel then stops for good.
    await vi.advanceTimersByTimeAsync(120000)
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('keeps same-timestamp lines split across a poll boundary, without duplicating the ones already shown', async () => {
    vi.useFakeTimers()

    // Two lines share one timestamp; only the first had been ingested when
    // the first poll ran. The server's since bound is inclusive, so the
    // second poll re-sends the first line alongside the newly ingested
    // second one — the panel must keep both, each exactly once.
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        jsonResponse(200, {
          runId: 'run-1',
          live: true,
          lines: [line('2026-07-10T12:00:00Z', 'INFO', 'twin one')],
        }),
      )
      .mockResolvedValueOnce(
        jsonResponse(200, {
          runId: 'run-1',
          live: false,
          lines: [line('2026-07-10T12:00:00Z', 'INFO', 'twin one'), line('2026-07-10T12:00:00Z', 'INFO', 'twin two')],
        }),
      )
      .mockResolvedValue(jsonResponse(200, { runId: 'run-1', live: false, lines: [] }))
    vi.stubGlobal('fetch', fetchMock)

    render(<LogPanel runId="run-1" />)

    await vi.waitFor(() => expect(screen.getByText('twin one')).toBeInTheDocument())

    await vi.advanceTimersByTimeAsync(3000)
    await vi.waitFor(() => expect(screen.getByText('twin two')).toBeInTheDocument())

    // getAllByText, not getByText: the whole point is asserting the re-sent
    // boundary line was deduplicated, not rendered twice.
    expect(screen.getAllByText('twin one')).toHaveLength(1)
    expect(screen.getAllByText('twin two')).toHaveLength(1)
  })

  it('backs off exponentially on repeated mid-stream poll failures', async () => {
    vi.useFakeTimers()

    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        jsonResponse(200, {
          runId: 'run-1',
          live: true,
          lines: [line('2026-07-10T12:00:00Z', 'INFO', 'still here')],
        }),
      )
      .mockRejectedValue(new TypeError('network down'))
    vi.stubGlobal('fetch', fetchMock)

    render(<LogPanel runId="run-1" />)

    await vi.waitFor(() => expect(screen.getByText('still here')).toBeInTheDocument())

    // The regular poll at +3s is the first failure; its retry keeps the
    // base 3s delay (backoff starts doubling from the second consecutive
    // failure).
    await vi.advanceTimersByTimeAsync(3000)
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))

    await vi.advanceTimersByTimeAsync(3000)
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3))

    // After the second failure the retry is backed off to 6s: nothing at
    // +3s...
    await vi.advanceTimersByTimeAsync(3000)
    expect(fetchMock).toHaveBeenCalledTimes(3)

    // ...but it fires by +6s, and the lines already shown were never lost.
    await vi.advanceTimersByTimeAsync(3000)
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(4))
    expect(screen.getByText('still here')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
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
      // The live:false response above still triggers the one catch-up poll;
      // give it (and nothing else) an empty answer so it ends the stream.
      .mockResolvedValue(jsonResponse(200, { runId: 'run-1', live: false, lines: [] }))
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
