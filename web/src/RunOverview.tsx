import CancelRunButton from './CancelRunButton'
import ConfigSummary from './ConfigSummary'
import DriveMetricsPanel from './DriveMetricsPanel'
import DryRunBadge from './DryRunBadge'
import RestartRunButton from './RestartRunButton'
import PauseActions from './PauseActions'
import type { PhaseInfo, RunEventDetail } from './RunDetail'
import TapesSection from './TapesSection'
import { formatDuration, phaseLabel } from './phaseFormat'
import { useReportMessageUrl } from './runDelivery'
import { livePauseState, runStatusView } from './runHeader'
import { temporalWorkflowUrl, useUiConfig } from './uiConfig'

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
// a phase-completion summary, a live per-drive write-health glance while the
// run is writing (DriveMetricsPanel, issue #307), a failed-run error console,
// the run-config viewer (ConfigSummary), and this run's own tape/slot outcomes
// (TapesSection). It owns none of the data fetching for the panels —
// each is a self-contained panel, same pattern as LogPanel/DriveMetricsPanel
// — so a VictoriaLogs/VictoriaMetrics-style outage in one never blocks the
// rest of this view (issue #277 AC4).
function RunOverview({ runId, detail, phases, terminal }: RunOverviewProps) {
  const pause = detail.currentPause
  const { isPaused, pauseUnknown } = livePauseState(pause, terminal)
  const hero = runStatusView(detail.status, isPaused, pauseUnknown)

  const completedCount = phases.filter((phase) => phase.status === 'completed').length
  const failedPhase = phases.find((phase) => phase.status === 'failed')

  const packPhase = phases.find((phase) => phase.name === 'Pack')
  const logicalTapes = factValue(packPhase, 'logicalTapes')
  const copies = factValue(packPhase, 'copies')

  // Live drive write-health glance (issue #307): the same per-drive
  // rate/floor/reposition gauges DriveMetricsPanel renders on the Write phase,
  // surfaced on the overview once there are actually drive writes to watch.
  // Shown only for a still-running run whose Write phase has begun (active or
  // completed) — the by-barcode VictoriaMetrics readings stay valid through the
  // running tail (Report/Burn/Deliver), but there is nothing to glance at
  // before Write starts, so the section is omitted then rather than showing an
  // empty "no measurement yet" gauge. A terminal run is excluded on purpose:
  // TapesSection below already reports each tape's final recorded write-health,
  // so the live panel would only duplicate it.
  const writePhase = phases.find((phase) => phase.name === 'Write')
  const showLiveDriveHealth = !terminal && (writePhase?.status === 'active' || writePhase?.status === 'completed')

  // The Temporal Web UI deep-link (design's "Temporal workflow ↗"): shown only
  // when the deployment configured a UI base URL (cmd/web's TEMPORAL_UI_URL);
  // otherwise there is no browsable UI to link to, so the link is omitted.
  const uiConfig = useUiConfig()
  const temporalUrl =
    uiConfig.status === 'loaded' ? temporalWorkflowUrl(uiConfig.config, detail.workflowId, runId) : null

  // The Discord report deep-link (design's "Discord report ↗", issue #306):
  // shown only for a run that delivered its PDF report to Discord and whose
  // posted-message identity was reconstructable from history; otherwise (no
  // delivery configured, delivery failed, or a still-running run) it is null and
  // the link is omitted.
  const reportUrl = useReportMessageUrl(runId, terminal)

  return (
    <div className="flex max-w-[880px] flex-col gap-5">
      <div className="flex items-start justify-between gap-4">
        <div>
          <div className="mb-1 flex flex-wrap items-center gap-2.5">
            <span className={`font-mono text-[11px] tracking-[0.04em] ${hero.badgeClass}`}>{hero.label}</span>
            <span className="font-mono text-[11px] text-text-faint">{formatDuration(detail.startTime, detail.closeTime)}</span>
            <DryRunBadge dryRun={detail.dryRun} />
          </div>
          <h2 className="text-[27px] font-semibold tracking-tight">{hero.title}</h2>
          {temporalUrl || reportUrl ? (
            <div className="mt-2.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-[12px]">
              {temporalUrl ? (
                <a
                  href={temporalUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="font-mono text-blue transition-opacity hover:opacity-70"
                >
                  Temporal workflow ↗
                </a>
              ) : null}
              {reportUrl ? (
                <a
                  href={reportUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="font-mono text-blue transition-opacity hover:opacity-70"
                >
                  Discord report ↗
                </a>
              ) : null}
            </div>
          ) : null}
        </div>

        {/* One control occupies the hero's action slot depending on the run's
            state: while it is still in progress, Cancel (stop it now — distinct
            from PauseActions' pause-specific Resume/Abort below, which apply only
            when paused); once it has closed, Restart (re-run the same config).
            Cancel applies whether or not the run is currently paused. */}
        {!terminal ? <CancelRunButton runId={runId} /> : <RestartRunButton runId={runId} />}
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
            className={`h-full rounded-md ${hero.tone === 'failed' ? 'bg-red' : 'bg-green'}`}
            style={{ width: `${phases.length > 0 ? (completedCount / phases.length) * 100 : 0}%` }}
          />
        </div>
      </div>

      {showLiveDriveHealth ? (
        <div>
          <h3 className="mb-1 text-[12.5px] font-medium text-text-dim">Drive write health</h3>
          <p className="mb-2 max-w-[560px] text-[11.5px] text-text-faint">
            Rate is measured once per tape when its write window closes — not a continuous trace, so a drive can read
            idle mid-write.
          </p>
          <DriveMetricsPanel runId={runId} terminal={false} />
        </div>
      ) : null}

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
