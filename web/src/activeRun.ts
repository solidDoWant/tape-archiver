import { useEffect, useState } from 'react'
import { apiFetch } from './api'
import type { RunSummary } from './RunHistory'

// ActiveRunState is whether the singleton backup workflow (SPEC §4.2 — at
// most one run at a time) currently has an execution in Temporal's
// "Running" status, for the sidebar's "Start new run" nav item.
export type ActiveRunState =
  | { status: 'loading' }
  | { status: 'error' }
  | { status: 'loaded'; activeRun: RunSummary | null }

// useActiveRun checks, once per Sidebar mount (effectively once per
// authenticated session — the shell never unmounts the sidebar while
// navigating between views), whether a run is currently active, so the
// sidebar can visibly disable "Start new run" with an explanation while one
// is in progress (issue #272's acceptance criterion). It reuses GET
// /api/runs rather than adding a new endpoint — the same data RunHistory.tsx
// already renders — and does not poll live; a run that starts or finishes
// while the operator is elsewhere in the app is picked up next time the
// sidebar (re)mounts, which is an accepted, minimal-scope gap for this
// foundational issue (later issues already give the dashboard/run-detail
// views their own live updates via SSE).
export function useActiveRun(): ActiveRunState {
  const [state, setState] = useState<ActiveRunState>({ status: 'loading' })

  useEffect(() => {
    let cancelled = false

    async function load() {
      try {
        const response = await apiFetch<{ runs: RunSummary[] }>('/api/runs')
        if (cancelled) {
          return
        }

        const activeRun = response.runs.find((run) => run.status === 'Running') ?? null
        setState({ status: 'loaded', activeRun })
      } catch {
        if (cancelled) {
          return
        }

        setState({ status: 'error' })
      }
    }

    void load()

    return () => {
      cancelled = true
    }
  }, [])

  return state
}
