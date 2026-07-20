import type { PhaseInfo } from './RunDetail'
import { phaseLabel, formatDuration } from './phaseFormat'
import LogPanel from './LogPanel'
import DriveMetricsPanel from './DriveMetricsPanel'

function statusBadgeClass(status: PhaseInfo['status']): string {
  switch (status) {
    case 'completed':
      return 'bg-green-bg text-green border-green-line'
    case 'active':
      return 'bg-amber-bg text-amber border-amber-line'
    case 'failed':
      return 'bg-red-bg text-red border-red-line'
    default:
      return 'bg-inset text-text-faint border-border'
  }
}

export interface PhaseDetailProps {
  runId: string
  // index is this phase's 1-based position in the pipeline (for the "PHASE
  // N" caption, DESIGN_ANALYSIS.md §2.B's "other-phase view").
  index: number
  phase: PhaseInfo
  // terminal marks a closed run, so the embedded DriveMetricsPanel (Write
  // phase only) renders this run's final recorded write-health instead of
  // polling VictoriaMetrics — see DriveMetricsPanelProps.terminal.
  terminal: boolean
}

// PhaseDetail is the run detail page's per-phase view (issue #277 AC2):
// facts, a phase-scoped log (LogPanel with the phase prop), a pending
// placeholder, or a failed-phase error — matching whichever of
// DESIGN_ANALYSIS.md §2.B's "other-phase view" sub-states this phase is
// actually in. The Write phase is the one exception (issue #277's explicit
// instruction): it additionally embeds DriveMetricsPanel alongside its log,
// so an operator watching it live sees per-drive write-rate/reposition
// figures without leaving this view (AC3).
function PhaseDetail({ runId, index, phase, terminal }: PhaseDetailProps) {
  const isWrite = phase.name === 'Write'

  return (
    <div className="flex max-w-[880px] flex-col gap-5">
      <div>
        <div className="mb-1.5 flex flex-wrap items-center gap-2.5">
          <span className="font-mono text-[11px] tracking-[0.04em] text-text-faint">PHASE {index}</span>
          <span className={`rounded-full border px-2.5 py-0.5 font-mono text-[11px] font-semibold tracking-[0.03em] ${statusBadgeClass(phase.status)}`}>
            {phase.status.toUpperCase()}
          </span>
          <span className="font-mono text-[11px] text-text-faint">{formatDuration(phase.startTime, phase.endTime)}</span>
        </div>
        <h2 className="text-2xl font-semibold tracking-tight">{phaseLabel(phase.name)}</h2>
      </div>

      {phase.status === 'pending' ? (
        <div
          role="status"
          className="flex items-center gap-2.5 rounded-lg border border-dashed border-border-strong p-4 text-[12px] text-text-faint"
        >
          <span aria-hidden="true" className="h-2 w-2 rounded-full bg-border-strong" />
          Not started — waiting for earlier phases to complete.
        </div>
      ) : (
        <>
          {phase.facts.length > 0 ? (
            <dl className="rounded-xl border border-border bg-surface px-5 py-1 shadow-card">
              {phase.facts.map((fact) => (
                <div key={fact.key} className="flex items-center justify-between border-b border-border py-2.5 last:border-b-0">
                  <dt className="text-[12.5px] text-text-dim">{fact.label}</dt>
                  <dd className="font-mono text-[12px]" title={fact.title}>{fact.value}</dd>
                </div>
              ))}
            </dl>
          ) : null}

          {phase.status === 'failed' && phase.error ? (
            <div className="overflow-hidden rounded-xl border border-red-line bg-console-bg">
              <div className="flex items-center gap-2.5 border-b border-console-border px-4 py-2.5">
                <span aria-hidden="true" className="h-2 w-2 rounded-full bg-red" />
                <span className="font-mono text-[11px] font-semibold text-red">error</span>
                <span className="font-mono text-[11px] text-console-dim">
                  {phaseLabel(phase.name)} phase · workflow failed
                </span>
              </div>
              <pre className="max-w-full overflow-x-auto p-4 font-mono text-[11.5px] leading-[1.95] whitespace-pre-wrap text-console-text">
                {phase.error}
              </pre>
            </div>
          ) : null}

          {isWrite ? (
            <div className="flex flex-col gap-4">
              <LogPanel runId={runId} phase={phase.name} terminal={terminal} />
              <DriveMetricsPanel runId={runId} terminal={terminal} />
            </div>
          ) : (
            <LogPanel runId={runId} phase={phase.name} terminal={terminal} />
          )}
        </>
      )}
    </div>
  )
}

export default PhaseDetail
