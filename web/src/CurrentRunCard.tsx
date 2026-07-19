import { formatTimestamp, statusBadgeClass, type RunSummary } from './api'
import DryRunBadge from './DryRunBadge'
import { phaseLabel } from './phaseFormat'
import { Link } from './router'
import { runPath } from './route'
import PauseActions from './PauseActions'
import { livePauseState } from './runHeader'
import type { RunEventsState } from './runEvents'

// phaseOrder mirrors workflows/backup's Phase* constants in pipeline order
// (workflow.go) — the same 11 phases GET /api/runs/{runID}/phases derives a
// full timeline from (pkg/runsapi/phases.go). This card only needs "how far
// through the pipeline is the run" for its progress bar, which the run's own
// live lastCompletedPhase (already carried by the SSE stream every other
// live view on this page uses) answers directly — deriving that from an
// index into this fixed, known-in-advance list is simpler and cheaper than a
// second per-tick fetch of the fuller phase-timeline endpoint, and is not a
// fabricated value: it is this run's own reported last completed phase,
// just expressed as a fraction of the pipeline it belongs to.
const phaseOrder = [
  'Resolve',
  'Prepare',
  'Pack',
  'Generate PAR2',
  'Verify',
  'Load',
  'Write',
  'Eject',
  'Report',
  'Burn',
  'Deliver',
]

function phaseProgress(lastCompletedPhase: string): { completed: number; total: number; percent: number } {
  const index = phaseOrder.indexOf(lastCompletedPhase)
  const completed = index >= 0 ? index + 1 : 0
  const total = phaseOrder.length

  return { completed, total, percent: Math.round((completed / total) * 100) }
}

export interface CurrentRunCardProps {
  loadState: 'loading' | 'error' | 'loaded'
  error?: string
  // activeRun is the currently Running execution, if any (backup runs are a
  // serial singleton — SPEC §4.2 — so there is at most one).
  activeRun: RunSummary | null
  // mostRecentRun is the newest execution in GET /api/runs (regardless of
  // status), used for the idle state's "last run" summary. Null only when no
  // run has ever been submitted.
  mostRecentRun: RunSummary | null
  // live is the SSE subscription Dashboard.tsx holds open for activeRun (via
  // runEvents.ts's useRunEvents) — the single source of truth for status,
  // last completed phase, and any operator-in-the-loop pause once it starts
  // reporting, ahead of the one-shot GET /api/runs snapshot in activeRun.
  live: RunEventsState
  onStartRun: () => void
}

// CurrentRunCard is the dashboard's top card (issue #276 AC1-3,
// DESIGN_ANALYSIS.md §2.A #1): the operator's first-glance answer to "is
// anything happening right now, and does it need me". Three mutually
// exclusive states — paused (needs an operator action), idle (nothing
// active, summarizing the last run), and active (a run in progress, live).
function CurrentRunCard({ loadState, error, activeRun, mostRecentRun, live, onStartRun }: CurrentRunCardProps) {
  if (loadState === 'loading') {
    return (
      <div className="rounded-xl border border-border bg-surface p-5 shadow-card">
        <p role="status" className="text-[12.5px] text-text-faint">
          Loading current run…
        </p>
      </div>
    )
  }

  if (loadState === 'error') {
    return (
      <div className="rounded-xl border border-border bg-surface p-5 shadow-card">
        <p role="alert" className="text-[12.5px] text-red">
          {error}
        </p>
      </div>
    )
  }

  if (!activeRun && !mostRecentRun) {
    return (
      <div className="rounded-xl border border-border bg-surface p-5 shadow-card">
        <div className="mb-2 flex items-center gap-2.5">
          <span className="text-[12px] font-semibold text-text-dim">CURRENT RUN</span>
          <span className="rounded-full border border-border bg-inset px-2.5 py-0.5 font-mono text-[11px] font-semibold text-text-dim">
            IDLE
          </span>
        </div>
        <div className="text-[19px] font-semibold tracking-tight">No runs yet</div>
        <p className="mt-1.5 text-[12.5px] text-text-dim">
          The backup workflow is idle. Submit a config to start the first run.
        </p>
        <button
          type="button"
          onClick={onStartRun}
          className="mt-4 rounded-[9px] bg-text px-4 py-2 text-[12.5px] font-semibold text-bg shadow-card transition-opacity hover:opacity-90"
        >
          Start a run →
        </button>
      </div>
    )
  }

  if (activeRun) {
    const status = live.detail?.status ?? activeRun.status
    const lastCompletedPhase = live.detail?.lastCompletedPhase ?? ''
    const pause = live.detail?.currentPause

    // A run that closed (terminated/completed/canceled) while waiting at a
    // pause still reports its last currentPause.kind, but Resume/Abort no
    // longer apply. livePauseState collapses that to not-paused for a terminal
    // run, so this card never shows the operator-action banner or the live
    // Resume/Abort buttons for a finished run — the same guard RunOverview and
    // the shell header already apply (runHeader.ts). Without it, aborting a run
    // paused on write-failure leaves the dashboard showing "Run paused — needs
    // you" with destructive actions for an already-closed run.
    const terminal = Boolean(live.detail?.closeTime) || live.state === 'terminal'
    const { isPaused, pauseUnknown } = pause
      ? livePauseState(pause, terminal)
      : { isPaused: false, pauseUnknown: false }

    if (pause && isPaused) {
      return (
        <div className="rounded-xl border border-border bg-surface p-5 shadow-card">
          <div className="border-l-[3px] border-amber pl-4">
            <div className="mb-1.5 flex flex-wrap items-center gap-2.5">
              <span className="text-[15px]">⏸</span>
              <span className="text-[15px] font-semibold">Run paused — needs you</span>
              <span className="rounded-full border border-amber-line bg-amber-bg px-2.5 py-0.5 font-mono text-[11px] font-semibold whitespace-nowrap text-amber">
                PAUSED
              </span>
              <span className="flex-1" />
              <Link to={runPath(activeRun.runId)} className="text-[12px] font-medium text-blue hover:opacity-60">
                Open run →
              </Link>
            </div>

            <PauseActions runId={activeRun.runId} pause={pause} />
          </div>
        </div>
      )
    }

    if (pauseUnknown) {
      // The pause query failed this tick, so we cannot tell whether the run is
      // waiting on an operator. It must NOT fall through to the healthy
      // progress bar below: a run genuinely stuck at an eject/write-failure
      // pause would then look identical to one running cleanly. State the
      // uncertainty, matching RunOverview/runStatusView's "PAUSE STATUS
      // UNKNOWN".
      return (
        <div className="rounded-xl border border-border bg-surface p-5 shadow-card">
          <div className="border-l-[3px] border-amber pl-4">
            <div className="mb-1.5 flex flex-wrap items-center gap-2.5">
              <span className="text-[15px]">⏸</span>
              <span className="text-[15px] font-semibold">Pause status unknown</span>
              <span className="rounded-full border border-amber-line bg-amber-bg px-2.5 py-0.5 font-mono text-[11px] font-semibold whitespace-nowrap text-amber">
                PAUSE STATUS UNKNOWN
              </span>
              <span className="flex-1" />
              <Link to={runPath(activeRun.runId)} className="text-[12px] font-medium text-blue hover:opacity-60">
                Open run →
              </Link>
            </div>
            <p className="text-[12.5px] text-text-dim">
              The pause state could not be read — the run may be waiting on an operator. Open the run to check.
            </p>
          </div>
        </div>
      )
    }

    const progress = phaseProgress(lastCompletedPhase)

    return (
      <div className="rounded-xl border border-border bg-surface p-5 shadow-card">
        <div className="mb-3.5 flex flex-wrap items-center gap-2.5">
          <span className="text-[12px] font-semibold text-text-dim">CURRENT RUN</span>
          <span
            className={`rounded-full px-2.5 py-0.5 font-mono text-[11px] font-semibold ${statusBadgeClass(status)}`}
          >
            {status}
          </span>
          <DryRunBadge dryRun={activeRun.dryRun} />
          <span className="flex-1" />
          <Link to={runPath(activeRun.runId)} className="text-[12px] font-medium text-blue hover:opacity-60">
            Open run →
          </Link>
        </div>

        <div className="flex flex-wrap items-baseline gap-3">
          <div className="text-[20px] font-semibold tracking-tight">{activeRun.runId}</div>
          <span className="font-mono text-[11px] text-text-dim">{lastCompletedPhase ? phaseLabel(lastCompletedPhase) : 'Starting…'}</span>
        </div>

        <div className="mt-3.5 h-2 overflow-hidden rounded-full bg-inset">
          <div className="h-full bg-blue" style={{ width: `${progress.percent}%` }} />
        </div>
        <div className="mt-2.5 flex gap-4">
          <span className="font-mono text-[11px] text-text-dim">
            Phase {progress.completed} of {progress.total}
          </span>
        </div>

        {live.state === 'error' ? (
          <p role="alert" className="mt-3 text-[11.5px] text-amber">
            Live updates disconnected — retrying automatically.
          </p>
        ) : null}
      </div>
    )
  }

  // Idle: no run is currently active, but at least one has run before.
  const summary = mostRecentRun!

  return (
    <div className="rounded-xl border border-border bg-surface p-5 shadow-card">
      <div className="mb-2 flex items-center gap-2.5">
        <span className="text-[12px] font-semibold text-text-dim">CURRENT RUN</span>
        <span className="rounded-full border border-border bg-inset px-2.5 py-0.5 font-mono text-[11px] font-semibold text-text-dim">
          IDLE
        </span>
        <span className="flex-1" />
        <button type="button" onClick={onStartRun} className="text-[12px] font-medium text-blue hover:opacity-60">
          Start a run →
        </button>
      </div>
      <div className="text-[19px] font-semibold tracking-tight">No active run</div>
      <p className="mt-1.5 text-[12.5px] text-text-dim">
        The backup workflow is idle. Submit a config to start the next run.
      </p>
      <div className="mt-3 font-mono text-[11px] text-text-faint">
        Last run{' '}
        <Link to={runPath(summary.runId)} aria-label={`Open last run ${summary.runId}`} className="text-blue hover:opacity-60">
          {summary.runId}
        </Link>
        : <span className={`rounded px-1.5 py-0.5 ${statusBadgeClass(summary.status)}`}>{summary.status}</span>{' '}
        {summary.dryRun ? <><DryRunBadge dryRun={summary.dryRun} /> </> : null}· started {formatTimestamp(summary.startTime)}
        {summary.closeTime ? <> · closed {formatTimestamp(summary.closeTime)}</> : null}
      </div>
    </div>
  )
}

export default CurrentRunCard
