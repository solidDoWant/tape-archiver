import type { PhaseInfo, RunEventDetail } from './RunDetail'
import { formatDuration, phaseLabel } from './phaseFormat'
import PauseActions from './PauseActions'
import ConfigSummary from './ConfigSummary'
import TapesSection from './TapesSection'

function heroCopy(status: string, paused: boolean, pauseUnknown: boolean): { label: string; title: string; badgeClass: string } {
  // Order matters: a confirmed pause (kind !== '') wins, but a failed pause
  // query (unknown) must NOT assert "Backup paused" — the run may or may
  // not be waiting on an operator, and the hero saying PAUSED while
  // PauseActions right below says "pause status unavailable" would
  // contradict itself. The hero states the uncertainty instead;
  // PauseActions renders the full warning for the same state.
  if (paused) {
    return { label: 'PAUSED', title: 'Backup paused', badgeClass: 'text-amber' }
  }

  if (pauseUnknown) {
    return { label: 'PAUSE STATUS UNKNOWN', title: 'Backup in progress', badgeClass: 'text-amber' }
  }

  switch (status) {
    case 'Running':
      return { label: 'RUNNING', title: 'Backup in progress', badgeClass: 'text-blue' }
    case 'Completed':
      return { label: 'COMPLETE', title: 'Backup completed', badgeClass: 'text-green' }
    case 'Failed':
    case 'Terminated':
    case 'TimedOut':
      return { label: status.toUpperCase(), title: 'Backup failed', badgeClass: 'text-red' }
    case 'Canceled':
      return { label: 'CANCELED', title: 'Backup canceled', badgeClass: 'text-text-dim' }
    default:
      return { label: status.toUpperCase(), title: status, badgeClass: 'text-text-dim' }
  }
}

// factValue looks up one PhaseFact's numeric value from a phase's facts
// list, parsed from its pre-formatted display text (facts.go's intFact
// always renders a plain base-10 integer for the keys this reads). Returns
// undefined when the phase has not produced that fact yet.
function factValue(phase: PhaseInfo | undefined, key: string): number | undefined {
  const fact = phase?.facts.find((candidate) => candidate.key === key)

  if (!fact) {
    return undefined
  }

  const parsed = Number.parseInt(fact.value, 10)

  return Number.isNaN(parsed) ? undefined : parsed
}

export interface RunOverviewProps {
  runId: string
  detail: RunEventDetail
  phases: PhaseInfo[]
  terminal: boolean
}

// RunOverview is the run detail page's default "Run overview" view
// (DESIGN_ANALYSIS.md §2.B's "isSummary" sub-view): a status hero, the
// operator pause zone (or a "no action needed" placeholder while healthy),
// a phase-completion summary, a failed-run error console, the run-config
// viewer (ConfigSummary), and this run's own tape/slot outcomes
// (TapesSection). It owns none of the data fetching for the latter two —
// each is a self-contained panel, same pattern as LogPanel/DriveMetricsPanel
// — so a VictoriaLogs/VictoriaMetrics-style outage in one never blocks the
// rest of this view (issue #277 AC4).
function RunOverview({ runId, detail, phases, terminal }: RunOverviewProps) {
  const pause = detail.currentPause
  const isPaused = pause.kind !== ''
  const pauseUnknown = Boolean(pause.unknown)
  const hero = heroCopy(detail.status, isPaused, pauseUnknown)

  const completedCount = phases.filter((phase) => phase.status === 'completed').length
  const failedPhase = phases.find((phase) => phase.status === 'failed')

  const packPhase = phases.find((phase) => phase.name === 'Pack')
  const logicalTapes = factValue(packPhase, 'logicalTapes')
  const copies = factValue(packPhase, 'copies')

  return (
    <div className="flex max-w-[840px] flex-col gap-5">
      <div>
        <div className="mb-1 flex items-baseline gap-2.5">
          <span className={`font-mono text-[11px] tracking-[0.04em] ${hero.badgeClass}`}>{hero.label}</span>
          <span className="font-mono text-[11px] text-text-faint">{formatDuration(detail.startTime, detail.closeTime)}</span>
        </div>
        <h2 className="text-[27px] font-semibold tracking-tight">{hero.title}</h2>
        <p className="mt-1.5 max-w-[560px] text-[13.5px] text-text-dim">
          Last completed phase: {detail.lastCompletedPhase || '—'}
        </p>
      </div>

      {isPaused || pauseUnknown ? (
        <PauseActions runId={runId} pause={pause} />
      ) : detail.status === 'Running' ? (
        <div className="flex items-center gap-2.5 rounded-lg border border-dashed border-border-strong px-4 py-3 text-[12px] text-text-dim">
          No operator action needed right now. Pauses show up here when you need to restock blanks, clear the I/O
          station, or swap an optical disc.
        </div>
      ) : null}

      <div className="rounded-xl border border-border bg-surface p-4 shadow-card">
        <div className="mb-2.5 flex items-baseline justify-between">
          <span className="text-[12.5px] font-medium">Pipeline progress</span>
          <span className="font-mono text-[12px] text-text-dim">
            {completedCount} of {phases.length} phases complete
          </span>
        </div>
        <div className="h-[9px] overflow-hidden rounded-md bg-inset">
          <div
            className={`h-full rounded-md ${detail.status === 'Failed' ? 'bg-red' : 'bg-green'}`}
            style={{ width: `${phases.length > 0 ? (completedCount / phases.length) * 100 : 0}%` }}
          />
        </div>
      </div>

      {failedPhase?.error ? (
        <div className="overflow-hidden rounded-xl border border-red-line bg-console-bg">
          <div className="flex items-center gap-2.5 border-b border-console-border px-4 py-2.5">
            <span aria-hidden="true" className="h-2 w-2 rounded-full bg-red" />
            <span className="font-mono text-[11px] font-semibold text-red">error</span>
            <span className="font-mono text-[11px] text-console-dim">{phaseLabel(failedPhase.name)} phase · workflow failed</span>
          </div>
          <pre className="max-w-full overflow-x-auto p-4 font-mono text-[11.5px] leading-[1.95] whitespace-pre-wrap text-console-text">
            {failedPhase.error}
          </pre>
        </div>
      ) : null}

      <ConfigSummary runId={runId} logicalTapes={logicalTapes} copies={copies} />

      <div>
        <h3 className="mb-2 text-[12.5px] font-medium text-text-dim">Tapes loaded by this run</h3>
        <TapesSection runId={runId} terminal={terminal} />
      </div>
    </div>
  )
}

export default RunOverview
