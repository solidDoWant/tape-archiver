import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen, waitFor } from '@testing-library/react'
import DriveMetricsPanel from './DriveMetricsPanel'

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

// advance drives the fake clock forward inside act() so the poll fetches each
// tick issues — and the state updates their responses trigger — are flushed
// the way React expects. vi.advanceTimersByTimeAsync / vi.waitFor do not wrap
// updates in act() (unlike RTL's waitFor), so a bare advance leaves the
// panel's setState landing outside act, which React reports as "not wrapped in
// act(...)". advance(0) after render flushes the mount fetch's resolved-promise
// chain the same way, without moving the clock.
async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('DriveMetricsPanel', () => {
  it('shows a loading state before the first response', () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockReturnValue(new Promise(() => {})), // never resolves
    )

    render(<DriveMetricsPanel runId="run-1" />)

    expect(screen.getByText(/loading drive metrics/i)).toBeInTheDocument()
  })

  it('shows an unavailable state on a 503 from the drives endpoint', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(503, { error: 'VictoriaMetrics is not configured' })))

    render(<DriveMetricsPanel runId="run-1" />)

    await waitFor(() => expect(screen.getByText(/metrics unavailable/i)).toBeInTheDocument())
  })

  it('shows a no-data state when the run has not loaded any tape yet', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(200, { runId: 'run-1', drives: [] })))

    render(<DriveMetricsPanel runId="run-1" />)

    await waitFor(() => expect(screen.getByText(/no measurement yet/i)).toBeInTheDocument())
  })

  it('renders a DriveGauge and sparkline per drive once live, including a visible below-floor indicator', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)

      if (url.includes('/metrics/drives/TA0001L6/history')) {
        return jsonResponse(200, {
          runId: 'run-1',
          barcode: 'TA0001L6',
          metric: 'throughput',
          points: [
            { time: '2026-07-10T00:00:00Z', value: 40 },
            { time: '2026-07-10T00:01:30Z', value: 38 },
          ],
        })
      }

      return jsonResponse(200, {
        runId: 'run-1',
        drives: [
          {
            barcode: 'TA0001L6',
            tapeIndex: 0,
            copyIndex: 0,
            driveIndex: 0,
            result: 'loaded',
            hasData: true,
            throughputMBps: 38,
            repositions: 1,
            tapeAlertFlagCount: 0,
            belowFloor: true,
            floorMBps: 50,
            floorKnown: true,
          },
        ],
      })
    })

    vi.stubGlobal('fetch', fetchMock)

    render(<DriveMetricsPanel runId="run-1" />)

    await waitFor(() => expect(screen.getByText('38 MB/s')).toBeInTheDocument())
    expect(screen.getByText(/TA0001L6/)).toBeInTheDocument()
    expect(screen.getByText(/below speed-matching floor/i)).toBeInTheDocument()

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining('/metrics/drives/TA0001L6/history'), undefined))
  })

  it('keeps the live gauges when a later drives poll fails transiently', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })

    let drivesCalls = 0
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)

      if (url.includes('/metrics/drives/TA0001L6/history')) {
        return jsonResponse(200, {
          runId: 'run-1',
          barcode: 'TA0001L6',
          metric: 'throughput',
          points: [{ time: '2026-07-10T00:00:00Z', value: 40 }],
        })
      }

      drivesCalls += 1
      if (drivesCalls === 1) {
        return jsonResponse(200, {
          runId: 'run-1',
          drives: [
            {
              barcode: 'TA0001L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              result: 'loaded',
              hasData: true,
              throughputMBps: 38,
              repositions: 1,
              tapeAlertFlagCount: 0,
              belowFloor: false,
              floorMBps: 50,
              floorKnown: true,
            },
          ],
        })
      }

      // Every later poll fails (transient blip / VictoriaMetrics hiccup).
      return jsonResponse(503, { error: 'VictoriaMetrics blip' })
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<DriveMetricsPanel runId="run-1" pollIntervalMs={1000} />)

    await advance(0)
    await vi.waitFor(() => expect(screen.getByText('38 MB/s')).toBeInTheDocument())

    // A later poll fails; the gauges must stay (not collapse to "unavailable"),
    // preserving each card's sparkline history.
    await advance(1000)
    await vi.waitFor(() => expect(drivesCalls).toBeGreaterThanOrEqual(2))

    expect(screen.getByText('38 MB/s')).toBeInTheDocument()
    expect(screen.queryByText(/metrics unavailable/i)).not.toBeInTheDocument()

    vi.useRealTimers()
  })

  it('keeps the last-good sparkline when a later history poll fails transiently', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })

    let historyCalls = 0
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)

      if (url.includes('/metrics/drives/TA0001L6/history')) {
        historyCalls += 1
        if (historyCalls === 1) {
          return jsonResponse(200, {
            runId: 'run-1',
            barcode: 'TA0001L6',
            metric: 'throughput',
            points: [
              { time: '2026-07-10T00:00:00Z', value: 40 },
              { time: '2026-07-10T00:01:00Z', value: 42 },
            ],
          })
        }

        // Every later history poll fails transiently.
        return jsonResponse(503, { error: 'VictoriaMetrics blip' })
      }

      // The drives endpoint stays live so the card is never unmounted.
      return jsonResponse(200, {
        runId: 'run-1',
        drives: [
          {
            barcode: 'TA0001L6',
            tapeIndex: 0,
            copyIndex: 0,
            driveIndex: 0,
            result: 'loaded',
            hasData: true,
            throughputMBps: 38,
            repositions: 0,
            tapeAlertFlagCount: 0,
            belowFloor: false,
            floorMBps: 50,
            floorKnown: true,
          },
        ],
      })
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<DriveMetricsPanel runId="run-1" pollIntervalMs={1000} />)

    // The sparkline populates from the first successful history poll.
    await advance(0)
    await vi.waitFor(() => expect(screen.getByRole('img', { name: /write rate over the last/i })).toBeInTheDocument())

    // A later history poll 503s; the sparkline must keep its last-good data
    // rather than flipping to the "unavailable" placeholder (mirroring how the
    // parent keeps the gauge live through the same blip).
    await advance(1000)
    await vi.waitFor(() => expect(historyCalls).toBeGreaterThanOrEqual(2))

    expect(screen.getByRole('img', { name: /write rate over the last/i })).toBeInTheDocument()
    expect(screen.queryByText(/write-rate history unavailable/i)).not.toBeInTheDocument()

    vi.useRealTimers()
  })

  it('polls the drives endpoint again after the interval elapses', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })

    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { runId: 'run-1', drives: [] }))
    vi.stubGlobal('fetch', fetchMock)

    render(<DriveMetricsPanel runId="run-1" pollIntervalMs={1000} />)

    await advance(0)
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))

    await advance(1000)
    await vi.waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThanOrEqual(2))

    vi.useRealTimers()
  })

  it('stops polling once unmounted', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })

    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { runId: 'run-1', drives: [] }))
    vi.stubGlobal('fetch', fetchMock)

    const { unmount } = render(<DriveMetricsPanel runId="run-1" pollIntervalMs={1000} />)

    await advance(0)
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))

    unmount()
    const callsAtUnmount = fetchMock.mock.calls.length

    await advance(5000)

    expect(fetchMock.mock.calls.length).toBe(callsAtUnmount)

    vi.useRealTimers()
  })

  it('does not start an overlapping poll while one is still in flight', async () => {
    // Regression: the panel self-schedules the next poll only after the current
    // one settles. A fixed-rate setInterval would keep firing fetches while a
    // slow poll is still pending, and their responses could resolve out of order
    // and flicker the gauges backward to stale readings. With a poll that never
    // resolves, no further fetch may be issued no matter how much time passes.
    vi.useFakeTimers()

    try {
      const fetchMock = vi.fn().mockReturnValue(new Promise(() => {})) // never resolves
      vi.stubGlobal('fetch', fetchMock)

      render(<DriveMetricsPanel runId="run-1" pollIntervalMs={1000} />)

      const callsAfterMount = fetchMock.mock.calls.length
      expect(callsAfterMount).toBeGreaterThan(0)

      await advance(10_000)

      // No new poll while the first is still pending — the interval-driven
      // version would have fired ~10 more by now.
      expect(fetchMock.mock.calls.length).toBe(callsAfterMount)
    } finally {
      vi.useRealTimers()
    }
  })

  describe('terminal runs', () => {
    it('renders final write-health from the run history and never calls the VictoriaMetrics endpoints', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })

      const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)

        if (url.endsWith('/api/runs/run-1/tapes')) {
          return jsonResponse(200, {
            runId: 'run-1',
            tapes: [
              {
                barcode: 'TA0001L6',
                tapeIndex: 0,
                copyIndex: 0,
                driveIndex: 0,
                result: 'written',
                writeHealth: {
                  measured: true,
                  throughputMBps: 140,
                  floorMBps: 50,
                  floorKnown: true,
                  belowFloor: false,
                  repositions: 0,
                  repositionsMeasured: true,
                  tapeAlertFlags: [],
                  healthy: true,
                },
              },
              {
                barcode: 'TA0002L6',
                tapeIndex: 1,
                copyIndex: 0,
                driveIndex: 1,
                result: 'written',
                // No measurement was taken (writehealth.go's Measured false).
                writeHealth: {
                  measured: false,
                  throughputMBps: 0,
                  floorKnown: false,
                  belowFloor: false,
                  repositionsMeasured: false,
                  healthy: false,
                },
              },
            ],
          })
        }

        throw new Error(`unexpected fetch to ${url}`)
      })

      vi.stubGlobal('fetch', fetchMock)

      render(<DriveMetricsPanel runId="run-1" terminal pollIntervalMs={1000} />)

      await advance(0)
      await vi.waitFor(() => expect(screen.getByText('140 MB/s')).toBeInTheDocument())
      expect(screen.getByText(/measured once, after each tape completes/i)).toBeInTheDocument()
      expect(screen.getByText(/TA0001L6/)).toBeInTheDocument()
      expect(screen.getByText(/no measurement was taken/i)).toBeInTheDocument()

      // No VictoriaMetrics-backed endpoint may ever be hit for a terminal
      // run — a reused barcode would attribute a later run's samples here.
      const calls = fetchMock.mock.calls.map((call) => String(call[0]))
      expect(calls.every((url) => url.endsWith('/api/runs/run-1/tapes'))).toBe(true)

      // And it is a single fetch, not a poll.
      await advance(5000)
      expect(fetchMock).toHaveBeenCalledTimes(1)

      vi.useRealTimers()
    })

    it('renders a measured tape missing throughputMBps without crashing', async () => {
      vi.stubGlobal(
        'fetch',
        vi.fn(async (input: RequestInfo | URL) => {
          if (String(input).endsWith('/api/runs/run-1/tapes')) {
            return jsonResponse(200, {
              runId: 'run-1',
              tapes: [
                {
                  barcode: 'TA0003L6',
                  tapeIndex: 0,
                  copyIndex: 0,
                  driveIndex: 0,
                  result: 'written',
                  // measured, but throughputMBps is absent — a server-side
                  // invariant violation that must degrade, not crash the render.
                  writeHealth: {
                    measured: true,
                    floorKnown: false,
                    belowFloor: false,
                    repositionsMeasured: false,
                    healthy: true,
                  },
                },
              ],
            })
          }

          throw new Error(`unexpected fetch to ${String(input)}`)
        }),
      )

      render(<DriveMetricsPanel runId="run-1" terminal />)

      await waitFor(() => expect(screen.getByText(/TA0003L6/)).toBeInTheDocument())
      expect(screen.queryByText(/MB\/s/)).not.toBeInTheDocument()
      expect(screen.queryByText(/undefined/)).not.toBeInTheDocument()
    })

    it('shows a no-tapes placeholder for a terminal run that wrote nothing', async () => {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(200, { runId: 'run-1', tapes: [] })))

      render(<DriveMetricsPanel runId="run-1" terminal />)

      await waitFor(() => expect(screen.getByText(/no tapes were written/i)).toBeInTheDocument())
    })

    it('degrades to a styled unavailable state when the tapes endpoint fails', async () => {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(502, { error: 'upstream' })))

      render(<DriveMetricsPanel runId="run-1" terminal />)

      await waitFor(() => expect(screen.getByText(/write-health unavailable/i)).toBeInTheDocument())
    })
  })
})
