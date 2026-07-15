import { useEffect, useState } from 'react'
import { apiFetch, ApiError } from './api'
import DriveGauge from './DriveGauge'
import WriteRateSparkline, { type MetricPoint, type SparklineStatus } from './WriteRateSparkline'
import { IconWarning } from './icons'

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

// WriteHealthInfo/TapeOutcome mirror pkg/runsapi's GET /api/runs/{runID}/tapes
// JSON shapes (tapes.go), which back the terminal-run view below.
interface WriteHealthInfo {
  measured: boolean
  throughputMBps: number
  floorMBps?: number
  floorKnown: boolean
  belowFloor: boolean
  repositions?: number
  repositionsMeasured: boolean
  tapeAlertFlags?: string[]
  healthy: boolean
}

interface TapeOutcome {
  barcode: string
  tapeIndex: number
  copyIndex: number
  driveIndex: number
  result: string
  writeHealth?: WriteHealthInfo
}

interface RunTapesResponse {
  runId: string
  tapes: TapeOutcome[]
}

type PanelState = { status: 'loading' } | { status: 'unavailable' } | { status: 'no-data' } | { status: 'live'; drives: DriveMetric[] }

export interface DriveMetricsPanelProps {
  runId: string
  // terminal marks a run that has reached a terminal status (RunDetail's SSE
  // "done" event). A terminal run never touches VictoriaMetrics at all: its
  // final per-tape write-health is rendered once from the run's own Temporal
  // history (GET /api/runs/{runID}/tapes) instead — no poll, no live
  // styling. Two reasons: polling a closed run's metrics forever is pure
  // waste, and — worse — VictoriaMetrics samples are only correlated to a
  // run by barcode-right-now, so a barcode reused by a LATER run would have
  // its readings wrongly attributed to this old run's page. The history-
  // derived data has no such hazard: it is this run's own recorded
  // measurement, immutable.
  terminal?: boolean
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

// DriveMetricsPanel is the run page's drive write-health view (issue #275).
// For a run still in progress it polls GET /api/runs/{runId}/metrics/drives
// every pollIntervalMs and renders one DriveGauge + WriteRateSparkline per
// tape the run has loaded, live from VictoriaMetrics; for a terminal run it
// renders the final measurements from the run's own history instead (see
// DriveMetricsPanelProps.terminal). It owns exactly the four states
// DriveGauge/WriteRateSparkline themselves render from props — "loading"
// until the first response, "unavailable" for a 503 (VictoriaMetrics
// unconfigured or unreachable, pkg/runsapi/metrics.go), "no-data" once
// reachable but before any tape has been loaded, and "live" with an actual
// drive list — so a transient poll failure degrades to the same styled
// "unavailable" state rather than a raw error or a stuck spinner (AC1/AC2).
//
// This is intentionally reusable and minimally wired: RunDetail.tsx mounts
// it once the run's state is known (the underlying data is simply
// empty/"no-data" outside the Write phase), and issue #277's redesigned run
// page re-homes it into the fuller Write-phase layout described in
// DESIGN_ANALYSIS.md §3.
//
// The labeled <section> wrapper exists so the panel is addressable as one
// landmark regardless of which of its states is rendering (live gauges, the
// terminal write-health summary, "unavailable", "no data") — its inner
// states share no other stable root element. Screen-reader users get a
// named region for the same reason tests get a stable locator (issue #281's
// e2e pass asserts this panel on the Write phase by this accessible name).
function DriveMetricsPanel({ runId, terminal = false, pollIntervalMs = defaultPollIntervalMs }: DriveMetricsPanelProps) {
  return (
    <section aria-label="Drive write health">
      {terminal ? <FinalDriveMetrics runId={runId} /> : <LiveDriveMetrics runId={runId} pollIntervalMs={pollIntervalMs} />}
    </section>
  )
}

// LiveDriveMetrics is the non-terminal (VictoriaMetrics-polling) half of
// DriveMetricsPanel — see its doc comment.
function LiveDriveMetrics({ runId, pollIntervalMs }: { runId: string; pollIntervalMs: number }) {
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

type FinalState =
  | { status: 'loading' }
  | { status: 'unavailable' }
  | { status: 'no-data' }
  | { status: 'final'; tapes: TapeOutcome[] }

// FinalDriveMetrics renders a terminal run's per-tape write-health verdicts
// from the run's own Temporal history (GET /api/runs/{runID}/tapes — the
// same writeHealth data #273's tape-outcome endpoints already expose): one
// fetch, no polling, no live styling. See DriveMetricsPanelProps.terminal
// for why a closed run must never be rendered from VictoriaMetrics.
function FinalDriveMetrics({ runId }: { runId: string }) {
  const [state, setState] = useState<FinalState>({ status: 'loading' })

  useEffect(() => {
    let cancelled = false

    const fetchTapes = async () => {
      try {
        const response = await apiFetch<RunTapesResponse>(`/api/runs/${encodeURIComponent(runId)}/tapes`)

        if (!cancelled) {
          setState(response.tapes.length > 0 ? { status: 'final', tapes: response.tapes } : { status: 'no-data' })
        }
      } catch {
        if (!cancelled) {
          setState({ status: 'unavailable' })
        }
      }
    }

    void fetchTapes()

    return () => {
      cancelled = true
    }
  }, [runId])

  if (state.status === 'loading') {
    return <p className="rounded-xl border border-border bg-surface p-3 text-[12px] text-text-faint">Loading final write-health…</p>
  }

  if (state.status === 'unavailable') {
    return <p className="rounded-xl border border-border bg-surface p-3 text-[12px] text-text-faint">Write-health unavailable</p>
  }

  if (state.status === 'no-data') {
    return <p className="rounded-xl border border-border bg-surface p-3 text-[12px] text-text-faint">No tapes were written by this run</p>
  }

  return (
    <div className="flex flex-col gap-2">
      <p className="text-[11.5px] text-text-faint">
        Final measurements from this run's own record — write health is measured once, after each tape completes.
      </p>

      {state.tapes.map((tape) => (
        <FinalTapeCard key={tape.barcode} tape={tape} />
      ))}
    </div>
  )
}

// FinalTapeCard is one tape's static, final write-health line.
function FinalTapeCard({ tape }: { tape: TapeOutcome }) {
  const health = tape.writeHealth

  return (
    <div className="min-w-0 rounded-xl border border-border bg-surface p-3 text-[12px] shadow-card">
      <div className="flex flex-wrap items-baseline gap-x-1.5 font-mono text-text-faint">
        <span>DRIVE {tape.driveIndex}</span>
        <span className="truncate text-text-dim">· {tape.barcode}</span>
        <span>· {tape.result}</span>
      </div>

      {health?.measured ? (
        <div className="mt-0.5 flex flex-wrap items-baseline gap-x-2 gap-y-0.5 font-mono">
          <span className="text-[13px] font-semibold text-text">{health.throughputMBps.toFixed(0)} MB/s</span>
          {health.floorKnown ? <span className="text-text-faint">floor {health.floorMBps}</span> : null}
          {health.repositionsMeasured ? (
            <span className="text-text-faint">
              {health.repositions ?? 0} reposition{(health.repositions ?? 0) === 1 ? '' : 's'}
            </span>
          ) : null}
        </div>
      ) : (
        <p className="mt-0.5 text-text-faint">No measurement was taken for this tape</p>
      )}

      {health?.measured && health.belowFloor ? (
        <span className="mt-1 mr-1 inline-flex items-center gap-1 rounded-full border border-amber-line bg-amber-bg px-2 py-0.5 text-[11px] font-medium text-amber">
          <IconWarning className="h-3 w-3" />
          Below speed-matching floor
        </span>
      ) : null}

      {health?.measured && (health.tapeAlertFlags?.length ?? 0) > 0 ? (
        <span className="mt-1 inline-flex items-center gap-1 rounded-full border border-red-line bg-red-bg px-2 py-0.5 text-[11px] font-medium text-red">
          <IconWarning className="h-3 w-3" />
          TapeAlert: {health.tapeAlertFlags!.join('; ')}
        </span>
      ) : null}
    </div>
  )
}

export default DriveMetricsPanel
