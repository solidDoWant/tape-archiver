import { useEffect, useState } from 'react'
import { apiFetch, ApiError, describeNetworkError, type RunsResponse, type RunSummary } from './api'
import CurrentRunCard from './CurrentRunCard'
import RunsTable from './RunsTable'
import LibraryCard from './LibraryCard'
import HardwareEnvCard from './HardwareEnvCard'
import { useRunEvents } from './runEvents'
import { findActiveRun } from './activeRun'

type LoadState = { status: 'loading' } | { status: 'error'; error: string } | { status: 'loaded' }

export interface DashboardProps {
  onStartRun: () => void
}

// Dashboard is the app's new landing page at "/" (issue #276): a
// current/most-recent run status card (live via SSE while a run is
// active), the paginated runs table that used to be the standalone
// "/history" page (route.ts's "history" route now redirects here), a
// history-derived library summary, and a hardware/environment card sourced
// from the deploy-config endpoint (GET /api/config/ui). Every card degrades
// independently (loading/error/empty states of its own) rather than the
// whole page failing over one data source.
//
// GET /api/runs is fetched once here, on mount — the same one-shot
// (not-live) pattern Sidebar's useActiveRun already uses (issue #272's
// accepted minimal-scope gap): a run that starts while the dashboard is
// already open is not picked up without a reload. The one exception is the
// currently active run itself, if any at mount time — once known, its
// status/phase/pause stay live via the shared SSE subscription
// (runEvents.ts), the same one RunDetail.tsx's own page uses.
function Dashboard({ onStartRun }: DashboardProps) {
  const [state, setState] = useState<LoadState>({ status: 'loading' })
  const [runs, setRuns] = useState<RunSummary[]>([])

  useEffect(() => {
    let cancelled = false

    async function load() {
      setState({ status: 'loading' })

      try {
        const response = await apiFetch<RunsResponse>('/api/runs')
        if (cancelled) {
          return
        }

        setRuns(response.runs)
        setState({ status: 'loaded' })
      } catch (error) {
        if (cancelled) {
          return
        }

        const message = error instanceof ApiError ? error.message : describeNetworkError(error)
        setState({ status: 'error', error: message })
      }
    }

    void load()

    return () => {
      cancelled = true
    }
  }, [])

  // GET /api/runs returns newest first (runsapi.go), so runs[0] is the most
  // recently started execution — the right "last run" summary for the idle
  // state and the right config source once no run is active.
  const activeRun = findActiveRun(runs)
  const mostRecentRun = runs[0] ?? null

  const live = useRunEvents(activeRun?.runId ?? null)

  return (
    <div className="flex max-w-[980px] flex-col gap-4 p-6 sm:p-7">
      <CurrentRunCard
        loadState={state.status}
        error={state.status === 'error' ? state.error : undefined}
        activeRun={activeRun}
        mostRecentRun={mostRecentRun}
        live={live}
        onStartRun={onStartRun}
      />

      <RunsTable
        loadState={state.status}
        error={state.status === 'error' ? state.error : undefined}
        runs={runs}
        liveRunId={activeRun?.runId ?? null}
        liveLastCompletedPhase={live.detail?.lastCompletedPhase}
      />

      <LibraryCard />

      <HardwareEnvCard />
    </div>
  )
}

export default Dashboard
