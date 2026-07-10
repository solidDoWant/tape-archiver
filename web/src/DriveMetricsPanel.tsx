import { useEffect, useState } from 'react'
import { apiFetch, ApiError } from './api'
import DriveGauge from './DriveGauge'
import WriteRateSparkline, { type MetricPoint, type SparklineStatus } from './WriteRateSparkline'

// DriveMetric mirrors pkg/runsapi.DriveMetric's JSON shape (GET
// /api/runs/{runID}/metrics/drives).
interface DriveMetric {
  barcode: string
  tapeIndex: number
  copyIndex: number
  driveIndex: number
  result: string
  hasData: boolean
  throughputMBps?: number
  repositions?: number
  tapeAlertFlagCount?: number
  belowFloor?: boolean
  floorMBps?: number
  floorKnown: boolean
}

interface DriveMetricsResponse {
  runId: string
  drives: DriveMetric[]
}

interface DriveMetricHistoryResponse {
  runId: string
  barcode: string
  metric: string
  points: MetricPoint[]
}

type PanelState = { status: 'loading' } | { status: 'unavailable' } | { status: 'no-data' } | { status: 'live'; drives: DriveMetric[] }

export interface DriveMetricsPanelProps {
  runId: string
  // pollIntervalMs lets tests drive polling deterministically without real
  // timers; production leaves this at defaultPollIntervalMs.
  pollIntervalMs?: number
}

// defaultPollIntervalMs: a plain client-side poll, not SSE, is the
// deliberate choice here (issue #275's technical context) — unlike run
// status/phase (GET /api/events/runs/{runID}, a core, always-configured part
// of the app), this data is optional best-effort observability that may be
// entirely unconfigured, and a short poll is simplest to reason about, test,
// and degrade from.
const defaultPollIntervalMs = 5000

// DriveMetricsPanel is the Write-phase live drive metrics view (issue #275):
// polls GET /api/runs/{runId}/metrics/drives every pollIntervalMs and
// renders one DriveGauge + WriteRateSparkline per tape the run has loaded,
// live from VictoriaMetrics. It owns exactly the four states DriveGauge/
// WriteRateSparkline themselves render from props — "loading" until the
// first response, "unavailable" for a 503 (VictoriaMetrics unconfigured or
// unreachable, pkg/runsapi/metrics.go), "no-data" once reachable but before
// any tape has been loaded, and "live" with an actual drive list — so a
// transient poll failure degrades to the same styled "unavailable" state
// rather than a raw error or a stuck spinner (AC1/AC2).
//
// This is intentionally reusable and minimally wired: RunDetail.tsx mounts
// it unconditionally (the underlying data is simply empty/"no-data" outside
// the Write phase), and issue #277's redesigned run page re-homes it into
// the fuller Write-phase layout described in DESIGN_ANALYSIS.md §3.
function DriveMetricsPanel({ runId, pollIntervalMs = defaultPollIntervalMs }: DriveMetricsPanelProps) {
  const [state, setState] = useState<PanelState>({ status: 'loading' })

  useEffect(() => {
    let cancelled = false

    const poll = async () => {
      try {
        const response = await apiFetch<DriveMetricsResponse>(`/api/runs/${encodeURIComponent(runId)}/metrics/drives`)

        if (cancelled) {
          return
        }

        setState(response.drives.length > 0 ? { status: 'live', drives: response.drives } : { status: 'no-data' })
      } catch {
        // Both a stable 503 (unconfigured/unreachable VictoriaMetrics) and
        // any other transient failure (network blip) degrade to the same
        // styled "unavailable" state — this panel is best-effort
        // observability layered on top of the run detail page, and must
        // never make the page itself look broken.
        if (!cancelled) {
          setState({ status: 'unavailable' })
        }
      }
    }

    void poll()
    const interval = setInterval(() => void poll(), pollIntervalMs)

    return () => {
      cancelled = true
      clearInterval(interval)
    }
  }, [runId, pollIntervalMs])

  if (state.status !== 'live') {
    return <DriveGauge driveIndex={0} status={state.status} />
  }

  return (
    <div className="flex flex-col gap-3">
      {state.drives.map((drive) => (
        <DriveMetricCard key={drive.barcode} runId={runId} drive={drive} pollIntervalMs={pollIntervalMs} />
      ))}
    </div>
  )
}

// DriveMetricCard pairs one drive's gauge with its own write-rate sparkline,
// polling GET /api/runs/{runId}/metrics/drives/{barcode}/history
// independently (a barcode's history is meaningful even before/after it
// carries an instant reading, so this is not derived from the parent
// panel's drives list).
function DriveMetricCard({ runId, drive, pollIntervalMs }: { runId: string; drive: DriveMetric; pollIntervalMs: number }) {
  const [points, setPoints] = useState<MetricPoint[] | null>(null)
  const [unavailable, setUnavailable] = useState(false)

  useEffect(() => {
    let cancelled = false

    const poll = async () => {
      try {
        const response = await apiFetch<DriveMetricHistoryResponse>(
          `/api/runs/${encodeURIComponent(runId)}/metrics/drives/${encodeURIComponent(drive.barcode)}/history`,
        )

        if (!cancelled) {
          setPoints(response.points)
          setUnavailable(false)
        }
      } catch (error) {
        if (cancelled) {
          return
        }

        // A 404 (barcode not yet known to a stale-cached run view) is
        // treated the same as "no data yet", not an error banner — only a
        // real fetch/5xx failure is surfaced as unavailable.
        if (error instanceof ApiError && error.status === 404) {
          setPoints([])
        } else {
          setUnavailable(true)
        }
      }
    }

    void poll()
    const interval = setInterval(() => void poll(), pollIntervalMs)

    return () => {
      cancelled = true
      clearInterval(interval)
    }
  }, [runId, drive.barcode, pollIntervalMs])

  const sparklineStatus: SparklineStatus = unavailable ? 'unavailable' : points === null ? 'loading' : points.length === 0 ? 'no-data' : 'live'

  return (
    <div className="flex flex-col gap-2 sm:flex-row sm:items-stretch">
      <DriveGauge
        driveIndex={drive.driveIndex}
        barcode={drive.barcode}
        status={drive.hasData ? 'live' : 'no-data'}
        throughputMBps={drive.throughputMBps}
        floorMBps={drive.floorMBps}
        floorKnown={drive.floorKnown}
        repositions={drive.repositions}
        tapeAlertFlagCount={drive.tapeAlertFlagCount}
        belowFloor={drive.belowFloor}
      />

      <div className="min-w-0 flex-1">
        <WriteRateSparkline status={sparklineStatus} points={points ?? []} floorMBps={drive.floorMBps} floorKnown={drive.floorKnown} />
      </div>
    </div>
  )
}

export default DriveMetricsPanel
