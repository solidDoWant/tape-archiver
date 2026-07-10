import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import DriveMetricsPanel from './DriveMetricsPanel'

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
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

  it('polls the drives endpoint again after the interval elapses', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })

    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { runId: 'run-1', drives: [] }))
    vi.stubGlobal('fetch', fetchMock)

    render(<DriveMetricsPanel runId="run-1" pollIntervalMs={1000} />)

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))

    await vi.advanceTimersByTimeAsync(1000)
    await vi.waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThanOrEqual(2))

    vi.useRealTimers()
  })

  it('stops polling once unmounted', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })

    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { runId: 'run-1', drives: [] }))
    vi.stubGlobal('fetch', fetchMock)

    const { unmount } = render(<DriveMetricsPanel runId="run-1" pollIntervalMs={1000} />)

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))

    unmount()
    const callsAtUnmount = fetchMock.mock.calls.length

    await vi.advanceTimersByTimeAsync(5000)

    expect(fetchMock.mock.calls.length).toBe(callsAtUnmount)

    vi.useRealTimers()
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

      await vi.waitFor(() => expect(screen.getByText('140 MB/s')).toBeInTheDocument())
      expect(screen.getByText(/measured once, after each tape completes/i)).toBeInTheDocument()
      expect(screen.getByText(/TA0001L6/)).toBeInTheDocument()
      expect(screen.getByText(/no measurement was taken/i)).toBeInTheDocument()

      // No VictoriaMetrics-backed endpoint may ever be hit for a terminal
      // run — a reused barcode would attribute a later run's samples here.
      const calls = fetchMock.mock.calls.map((call) => String(call[0]))
      expect(calls.every((url) => url.endsWith('/api/runs/run-1/tapes'))).toBe(true)

      // And it is a single fetch, not a poll.
      await vi.advanceTimersByTimeAsync(5000)
      expect(fetchMock).toHaveBeenCalledTimes(1)

      vi.useRealTimers()
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
