import { useId } from 'react'
import { IconSpinner, IconWarning } from './icons'

// DriveGaugeStatus is the four states issue #275 requires: "loading" while
// the first fetch is in flight, "unavailable" when VictoriaMetrics is
// unconfigured/unreachable (pkg/runsapi/metrics.go's stable 503), "no-data"
// when this drive/tape has no VictoriaMetrics reading yet (DriveMetric.hasData
// false — not yet measured, or the run has not loaded a tape at all), and
// "live" — an actual reading to render.
export type DriveGaugeStatus = 'loading' | 'unavailable' | 'no-data' | 'live'

export interface DriveGaugeProps {
  // driveIndex labels the gauge ("DRIVE 0"/"DRIVE 1", DESIGN_ANALYSIS.md §3).
  // A container without a specific drive to show yet (the panel-level
  // loading/unavailable/no-data placeholder) passes 0.
  driveIndex: number
  barcode?: string
  status: DriveGaugeStatus
  throughputMBps?: number
  floorMBps?: number
  floorKnown?: boolean
  repositions?: number
  tapeAlertFlagCount?: number
  belowFloor?: boolean
}

// ringDiameterPx matches the design's 48-52px per-drive circular gauge
// (DESIGN_ANALYSIS.md §3's component inventory).
const ringDiameterPx = 52

// ringColor picks the conic-gradient ring color for a live reading. Healthy
// (at/above a known floor, no active TapeAlert flags) is --green; a
// below-floor or TapeAlert reading is --amber — the ring color alone is
// never the only signal, though: the text badges below always spell out
// "Below speed-matching floor" / the flag count too (dataviz "never
// color-alone" rule), and the ring is skipped entirely (a static --inset
// disc) for every non-live status.
function ringColor(belowFloor?: boolean, tapeAlertFlagCount?: number): string {
  if (belowFloor || (tapeAlertFlagCount ?? 0) > 0) {
    return 'var(--amber)'
  }

  return 'var(--green)'
}

// DriveGauge is the reusable per-drive circular write-rate gauge
// (DESIGN_ANALYSIS.md §3's "per-drive circular gauge with health label +
// barcode + live rate '142 MB/s · floor 50 · 0 reposition'"), issue #275. It is
// purely presentational — a container (DriveMetricsPanel) owns fetching and
// polling and passes down whichever status/reading applies; DriveGauge
// itself renders any of the four states from props alone, which is what
// makes each state independently testable without a network fake.
function DriveGauge({
  driveIndex,
  barcode,
  status,
  throughputMBps,
  floorMBps,
  floorKnown,
  repositions,
  tapeAlertFlagCount,
  belowFloor,
}: DriveGaugeProps) {
  const labelId = useId()

  const hasReading = status === 'live' && typeof throughputMBps === 'number'

  // percent fills the ring relative to the speed-matching floor when known
  // (capped at 100% — the ring communicates "at/above floor", not an
  // unbounded scale); with no known floor to compare against, a live
  // reading still fills the ring fully so the color alone is not the only
  // cue that a reading exists (the MB/s text is always shown too).
  const percent = hasReading && floorKnown && floorMBps && floorMBps > 0 ? Math.min(1, throughputMBps! / floorMBps) : hasReading ? 1 : 0
  const degrees = Math.round(percent * 360)
  const color = ringColor(belowFloor, tapeAlertFlagCount)

  return (
    <div
      role="group"
      aria-labelledby={labelId}
      className="flex min-w-0 items-center gap-3 rounded-xl border border-border bg-surface p-3 shadow-card"
    >
      <div
        className="relative flex-none rounded-full"
        style={{
          width: ringDiameterPx,
          height: ringDiameterPx,
          border: '3px solid var(--border)',
          background: hasReading ? `conic-gradient(${color} ${degrees}deg, var(--inset) ${degrees}deg)` : 'var(--inset)',
        }}
      >
        <div
          aria-hidden="true"
          className="absolute inset-[5px] flex items-center justify-center rounded-full bg-surface font-mono text-[11px] text-text-dim"
        >
          {status === 'loading' ? <IconSpinner className="h-3.5 w-3.5 animate-spin text-text-faint" /> : driveIndex}
        </div>
      </div>

      <div className="min-w-0 flex-1 text-[12px]">
        <div id={labelId} className="flex flex-wrap items-baseline gap-x-1.5 font-mono text-text-faint">
          <span>DRIVE {driveIndex}</span>
          {barcode ? <span className="truncate text-text-dim">· {barcode}</span> : null}
        </div>

        {status === 'unavailable' ? (
          <p className="mt-0.5 text-text-faint">Metrics unavailable</p>
        ) : status === 'loading' ? (
          <p className="mt-0.5 text-text-faint">Loading drive metrics…</p>
        ) : !hasReading ? (
          // "No measurement yet" — deliberately NOT "not writing":
          // hasData:false also covers a tape currently mid-write, since
          // write health is measured once, after the tape's write window
          // closes (workflows/backup/writehealth.go), not continuously.
          <p className="mt-0.5 text-text-faint">No measurement yet — write health is measured after each tape completes</p>
        ) : (
          <div className="mt-0.5 flex flex-wrap items-baseline gap-x-2 gap-y-0.5 font-mono">
            <span className="text-[13px] font-semibold text-text">{throughputMBps!.toFixed(0)} MB/s</span>
            {floorKnown ? <span className="text-text-faint">floor {floorMBps}</span> : null}
            {typeof repositions === 'number' ? (
              <span className="text-text-faint">
                {repositions} reposition{repositions === 1 ? '' : 's'}
              </span>
            ) : null}
          </div>
        )}

        {hasReading && belowFloor ? (
          <span className="mt-1 mr-1 inline-flex items-center gap-1 rounded-full border border-amber-line bg-amber-bg px-2 py-0.5 text-[11px] font-medium text-amber">
            <IconWarning className="h-3 w-3" />
            Below speed-matching floor
          </span>
        ) : null}

        {hasReading && (tapeAlertFlagCount ?? 0) > 0 ? (
          <span className="mt-1 inline-flex items-center gap-1 rounded-full border border-red-line bg-red-bg px-2 py-0.5 text-[11px] font-medium text-red">
            <IconWarning className="h-3 w-3" />
            {tapeAlertFlagCount} TapeAlert flag{tapeAlertFlagCount === 1 ? '' : 's'}
          </span>
        ) : null}
      </div>
    </div>
  )
}

export default DriveGauge
