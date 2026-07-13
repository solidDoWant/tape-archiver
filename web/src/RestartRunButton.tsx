import { useActiveRun } from './activeRun'
import { submitPath, useNavigate } from './route'

export interface RestartRunButtonProps {
  runId: string
}

// RestartRunButton is the run page's "run this again" control for a closed run
// (RunOverview.tsx renders it in the same hero slot the in-progress CancelRunButton
// occupies — a run is either still cancellable or restartable, never both). It
// navigates to the config page with ?from=<runId> (submitPath), which preloads
// this run's submitted config there for a fresh submission (ConfigPage's
// restartFromRunId).
//
// Backup runs are a singleton (SPEC §4.2), so a new run cannot be submitted while
// one is in progress. This button reflects that up front: it is disabled whenever
// useActiveRun reports a run currently Running, so the operator is not walked to a
// config page that would only refuse the submission (the same block the sidebar's
// "Start new run" and ConfigPage's own active-run gate already apply). It stays
// disabled until the active-run check has resolved to a confirmed "nothing running"
// so a brief pre-load window can't offer a restart that isn't actually allowed.
function RestartRunButton({ runId }: RestartRunButtonProps) {
  const navigate = useNavigate()
  const activeRunState = useActiveRun()

  const canRestart = activeRunState.status === 'loaded' && !activeRunState.activeRun
  const blockedByActiveRun = activeRunState.status === 'loaded' && Boolean(activeRunState.activeRun)

  return (
    <div className="flex flex-col items-end gap-1.5">
      <button
        type="button"
        onClick={() => navigate(submitPath(runId))}
        disabled={!canRestart}
        title={blockedByActiveRun ? 'A run is already in progress — only one runs at a time.' : undefined}
        className="rounded-lg border border-border-strong bg-surface px-4 py-2 text-[12.5px] font-medium text-text transition-colors hover:bg-surface-2 disabled:opacity-50"
      >
        Restart run
      </button>

      {blockedByActiveRun ? (
        <p className="max-w-[220px] text-right text-[11px] text-text-faint">
          A run is already in progress — only one runs at a time.
        </p>
      ) : null}
    </div>
  )
}

export default RestartRunButton
