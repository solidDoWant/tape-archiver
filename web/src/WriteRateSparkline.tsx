// WriteRateSparkline is the reusable write-rate history chart
// (DESIGN_ANALYSIS.md §3's "WRITE RATE (8-bar sparkline + '140 MB/s · floor
// 50' caption)"), fed by GET
// /api/runs/{runID}/metrics/drives/{barcode}/history (pkg/runsapi/metrics.go
// — a fixed 8-point/90s-step VictoriaMetrics range query, never a
// client-controlled range). Per the dataviz skill: a single series needs no
// legend (the "MB/s" caption above the bars names it), thin marks with
// rounded data-ends, a status color (--green/--amber, never color alone —
// the numeric caption and the floor line both carry the same information in
// text/geometry), and the whole chart lives in a fixed-height flex row so it
// reflows on a narrow viewport without ever forcing horizontal scroll on the
// page body (issue #275 AC6).
export type SparklineStatus = 'loading' | 'unavailable' | 'no-data' | 'live'

export interface MetricPoint {
  time: string
  value: number
}

export interface WriteRateSparklineProps {
  status: SparklineStatus
  points: MetricPoint[]
  floorMBps?: number
  floorKnown?: boolean
}

function placeholder(text: string) {
  return <div className="rounded-xl border border-border bg-surface p-3 text-[12px] text-text-faint">{text}</div>
}

function WriteRateSparkline({ status, points, floorMBps, floorKnown }: WriteRateSparklineProps) {
  if (status === 'unavailable') {
    return placeholder('Write-rate history unavailable')
  }

  if (status === 'loading') {
    return placeholder('Loading write-rate history…')
  }

  if (status === 'no-data' || points.length === 0) {
    return placeholder('No write-rate samples yet')
  }

  const values = points.map((point) => point.value)
  const latest = values[values.length - 1]
  const maxValue = Math.max(...values, floorKnown && floorMBps ? floorMBps : 0, 1)
  const floorPercent = floorKnown && floorMBps ? Math.min(100, (floorMBps / maxValue) * 100) : null

  return (
    <div className="min-w-0 rounded-xl border border-border bg-surface p-3">
      <div className="mb-2 flex flex-wrap items-baseline gap-x-1.5 font-mono text-[12px]">
        <span className="text-[15px] font-semibold text-text">{latest.toFixed(0)} MB/s</span>
        {floorKnown ? <span className="text-text-faint">· floor {floorMBps} MB/s</span> : null}
      </div>

      <div
        role="img"
        aria-label={`Write rate over the last ${values.length} readings, in MB/s: ${values.map((value) => value.toFixed(0)).join(', ')}`}
        className="relative flex h-12 items-end gap-[3px]"
      >
        {floorPercent !== null ? (
          <div
            aria-hidden="true"
            className="absolute right-0 left-0 border-t border-dashed border-text-faint"
            style={{ bottom: `${floorPercent}%` }}
          />
        ) : null}

        {points.map((point, index) => {
          const heightPercent = Math.max(4, (point.value / maxValue) * 100)
          const belowFloor = floorKnown && floorMBps ? point.value < floorMBps : false

          return (
            <div
              key={`${point.time}-${index}`}
              className={`min-w-[3px] flex-1 rounded-t-[3px] ${belowFloor ? 'bg-amber' : 'bg-green'}`}
              style={{ height: `${heightPercent}%` }}
            />
          )
        })}
      </div>
    </div>
  )
}

export default WriteRateSparkline
