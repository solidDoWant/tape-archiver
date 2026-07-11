import { useState } from 'react'
import { formatDuration, formatTimestamp, statusBadgeClass, type RunSummary } from './api'
import { Link } from './router'
import { runPath } from './route'

// runsPerPage matches DESIGN_ANALYSIS.md §2.A's dashboard runs table (8
// rows/page, Prev/Next pagination). GET /api/runs itself is not paginated
// (it returns everything Temporal visibility still retains, up to
// pkg/runsapi's own listPageSize — see docs/web-ui.md's "Browsing run
// history" section), so pagination here is purely client-side over the
// already-fetched list.
const runsPerPage = 8

export interface RunsTableProps {
  loadState: 'loading' | 'error' | 'loaded'
  error?: string
  runs: RunSummary[]
  // liveRunId/liveLastCompletedPhase let the row for the currently active
  // run (if any) show its live last-completed-phase from the same SSE
  // subscription Dashboard.tsx's CurrentRunCard already holds open, rather
  // than a second per-row fetch — RunHistory.tsx's old approach fetched GET
  // /api/runs/{runID} once per running row to backfill this same field; with
  // backup runs a serial singleton (SPEC §4.2) there is at most one such row,
  // and Dashboard already has its phase live, so reusing it here avoids that
  // extra round trip entirely. A closed run's last completed phase is not
  // shown (only a live workflow query can answer that, and a closed run has
  // no worker left polling it — docs/web-ui.md).
  liveRunId?: string | null
  liveLastCompletedPhase?: string
}

// RunsTable is the dashboard's embedded, paginated run-history list (issue
// #276) — this IS the history view now; there is no separate "/history"
// page (route.ts's "history" route redirects to "/" instead). Replaces
// RunHistory.tsx, which this component supersedes entirely.
function RunsTable({ loadState, error, runs, liveRunId, liveLastCompletedPhase }: RunsTableProps) {
  const [page, setPage] = useState(0)

  const pageCount = Math.max(1, Math.ceil(runs.length / runsPerPage))
  const clampedPage = Math.min(page, pageCount - 1)
  const start = clampedPage * runsPerPage
  const pageRuns = runs.slice(start, start + runsPerPage)

  return (
    <div className="overflow-hidden rounded-xl border border-border bg-surface shadow-card">
      <div className="flex items-center px-4 pt-3.5 pb-3">
        <span className="text-[12.5px] font-semibold">Runs</span>
      </div>

      {loadState === 'loading' ? (
        <p role="status" className="border-t border-border px-4 py-6 text-[12.5px] text-text-faint">
          Loading runs…
        </p>
      ) : null}

      {loadState === 'error' ? (
        <p role="alert" className="border-t border-border px-4 py-6 text-[12.5px] text-red">
          {error}
        </p>
      ) : null}

      {loadState === 'loaded' && runs.length === 0 ? (
        <p className="border-t border-border px-4 py-6 text-[12.5px] text-text-faint">
          No runs yet. Submit a config to start the first one.
        </p>
      ) : null}

      {loadState === 'loaded' && runs.length > 0 ? (
        <>
          <div className="overflow-x-auto">
            <div className="min-w-[600px]">
              <div className="grid grid-cols-[256px_1fr_1fr_1fr_1fr] gap-3 border-t border-b border-border bg-surface-2 px-4 py-2.5">
                <span className="font-mono text-[11px] tracking-[0.06em] text-text-faint">RUN ID</span>
                <span className="font-mono text-[11px] tracking-[0.06em] text-text-faint">STARTED</span>
                <span className="font-mono text-[11px] tracking-[0.06em] text-text-faint">DURATION</span>
                <span className="font-mono text-[11px] tracking-[0.06em] text-text-faint">RESULT</span>
                <span className="font-mono text-[11px] tracking-[0.06em] whitespace-nowrap text-text-faint">
                  LAST PHASE
                </span>
              </div>

              {pageRuns.map((run) => {
                const lastPhase = run.runId === liveRunId ? liveLastCompletedPhase : undefined

                return (
                  <Link
                    key={run.runId}
                    to={runPath(run.runId)}
                    // aria-label: the whole row is one clickable link (the
                    // design's "row click opens that run"), but its
                    // accessible name is just the run ID — without this it
                    // would be every cell's text concatenated, which is
                    // noisy for assistive tech and for tests alike.
                    aria-label={run.runId}
                    className="grid grid-cols-[256px_1fr_1fr_1fr_1fr] items-center gap-3 border-b border-border px-4 py-3 hover:bg-surface-2"
                  >
                    <span className="flex min-w-0 truncate font-mono text-[11.5px] font-medium text-text">
                      {run.runId}
                    </span>
                    <span className="font-mono text-[11px] whitespace-nowrap text-text-dim">
                      {formatTimestamp(run.startTime)}
                    </span>
                    <span className="font-mono text-[11px] whitespace-nowrap text-text-dim">
                      {run.status === 'Running' ? 'Running' : formatDuration(run.startTime, run.closeTime)}
                    </span>
                    <span>
                      <span
                        className={`rounded-full px-2 py-0.5 font-mono text-[11px] font-semibold tracking-[0.03em] ${statusBadgeClass(run.status)}`}
                      >
                        {run.status}
                      </span>
                    </span>
                    <span className="font-mono text-[11px] text-text-dim">{lastPhase || '—'}</span>
                  </Link>
                )
              })}
            </div>
          </div>

          <div className="flex items-center gap-3.5 border-t border-border px-4 py-3">
            <span className="font-mono text-[11px] text-text-faint">
              {start + 1}–{Math.min(start + runsPerPage, runs.length)} of {runs.length}
            </span>
            <span className="flex-1" />
            <span className="font-mono text-[11px] text-text-dim">
              Page {clampedPage + 1} of {pageCount}
            </span>
            <button
              type="button"
              disabled={clampedPage === 0}
              onClick={() => setPage(clampedPage - 1)}
              className="rounded-lg border border-border-strong px-3 py-1.5 text-[12px] font-medium whitespace-nowrap disabled:cursor-not-allowed disabled:opacity-40 enabled:hover:bg-surface-2"
            >
              ← Prev
            </button>
            <button
              type="button"
              disabled={clampedPage >= pageCount - 1}
              onClick={() => setPage(clampedPage + 1)}
              className="rounded-lg border border-border-strong px-3 py-1.5 text-[12px] font-medium whitespace-nowrap disabled:cursor-not-allowed disabled:opacity-40 enabled:hover:bg-surface-2"
            >
              Next →
            </button>
          </div>
        </>
      ) : null}
    </div>
  )
}

export default RunsTable
