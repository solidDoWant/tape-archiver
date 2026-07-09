import { useEffect, useState } from 'react'
import { apiFetch, ApiError } from './api'
import { Link } from './router'
import { runPath } from './route'

// RunSummary mirrors pkg/runsapi.RunSummary's JSON shape, as returned in the
// GET /api/runs list (pkg/runsapi.RunsResponse).
export interface RunSummary {
  workflowId: string
  runId: string
  status: string
  startTime: string
  closeTime?: string
}

interface RunsResponse {
  runs: RunSummary[]
}

// RunRow augments RunSummary with a best-effort last-completed-phase, which
// GET /api/runs itself does not return — only GET /api/runs/{runID}
// (pkg/runsapi.RunDetail) has that field, since it comes from a live
// workflow query (backup.LastCompletedPhaseQuery) rather than Temporal
// visibility. Fetching every history row's detail to backfill this would be
// an N+1 fan-out against Temporal for data that is essentially never
// available anyway once a run has closed (the query needs a live worker
// polling that execution) — so this view only enriches rows that are
// currently "Running" (at most one, since backup runs are a serial
// singleton — SPEC §4.2) with a live per-row fetch, and leaves closed rows'
// phase unavailable ("—"), matching GET /api/runs's own documented
// unpagination limitation as an accepted, disclosed gap rather than an
// unbounded background fetch storm.
interface RunRow extends RunSummary {
  lastCompletedPhase?: string
}

interface RunDetailResponse extends RunSummary {
  lastCompletedPhase: string
}

type LoadState = { status: 'loading' } | { status: 'error'; error: string } | { status: 'loaded' }

function describeNetworkError(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error)

  return `Could not reach the server: ${message}`
}

function statusBadgeClass(status: string): string {
  switch (status) {
    case 'Running':
      return 'bg-blue-100 text-blue-900 dark:bg-blue-950 dark:text-blue-100'
    case 'Completed':
      return 'bg-green-100 text-green-900 dark:bg-green-950 dark:text-green-100'
    case 'Failed':
    case 'Terminated':
    case 'TimedOut':
      return 'bg-red-100 text-red-900 dark:bg-red-950 dark:text-red-100'
    case 'Canceled':
      return 'bg-amber-100 text-amber-900 dark:bg-amber-950 dark:text-amber-100'
    default:
      return 'bg-slate-100 text-slate-900 dark:bg-slate-800 dark:text-slate-100'
  }
}

function formatTimestamp(value?: string): string {
  return value ? new Date(value).toLocaleString() : '—'
}

// RunHistory lists past (and any currently running) executions of the
// singleton backup workflow, on top of the existing GET /api/runs endpoint
// — no new backend endpoint is added here (docs/web-ui-design.md §2, this
// issue's non-goals). Depth is bounded by Temporal visibility retention and
// GET /api/runs's own documented listPageSize limitation
// (pkg/runsapi/runsapi.go), same as it always was.
function RunHistory() {
  const [state, setState] = useState<LoadState>({ status: 'loading' })
  const [runs, setRuns] = useState<RunRow[]>([])

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

        const running = response.runs.filter((run) => run.status === 'Running')

        await Promise.all(
          running.map(async (run) => {
            try {
              const detail = await apiFetch<RunDetailResponse>(
                `/api/runs/${encodeURIComponent(run.runId)}`,
              )
              if (cancelled) {
                return
              }

              setRuns((current) =>
                current.map((existing) =>
                  existing.runId === run.runId
                    ? { ...existing, lastCompletedPhase: detail.lastCompletedPhase }
                    : existing,
                ),
              )
            } catch {
              // Best-effort enrichment only — leave this row's phase
              // unavailable rather than failing the whole history view over
              // one run's detail fetch.
            }
          }),
        )
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

  if (state.status === 'loading') {
    return (
      <p role="status" className="text-slate-600 dark:text-slate-400">
        Loading run history…
      </p>
    )
  }

  if (state.status === 'error') {
    return (
      <div
        role="alert"
        className="w-full max-w-3xl rounded border border-red-600 bg-red-50 p-3 text-red-900 dark:border-red-500 dark:bg-red-950 dark:text-red-100"
      >
        {state.error}
      </div>
    )
  }

  return (
    <div className="flex w-full max-w-3xl flex-col gap-4 text-left">
      <h2 className="text-xl font-semibold">Run history</h2>

      {runs.length === 0 ? (
        <p className="text-slate-600 dark:text-slate-400">No runs yet.</p>
      ) : (
        <ul className="flex flex-col gap-3">
          {runs.map((run) => (
            <li
              key={run.runId}
              className="rounded border border-slate-300 p-3 dark:border-slate-700"
            >
              <div className="flex flex-wrap items-center justify-between gap-2">
                <Link to={runPath(run.runId)} className="font-medium underline">
                  {run.runId}
                </Link>
                <span
                  className={`rounded px-2 py-0.5 text-xs font-semibold ${statusBadgeClass(run.status)}`}
                >
                  {run.status}
                </span>
              </div>

              <dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
                <dt className="font-medium">Started</dt>
                <dd>{formatTimestamp(run.startTime)}</dd>
                <dt className="font-medium">Closed</dt>
                <dd>{formatTimestamp(run.closeTime)}</dd>
                <dt className="font-medium">Last completed phase</dt>
                <dd>{run.lastCompletedPhase || '—'}</dd>
              </dl>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

export default RunHistory
