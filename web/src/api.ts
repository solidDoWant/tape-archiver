// api.ts is a small shared fetch helper for web/src components that talk to
// the JSON API under /api/* (pkg/runsapi). SubmitRunForm.tsx was the first
// fetch call in this package and inlined its own request/error handling;
// PauseActions.tsx (sub-issue 5) is the second, so per the epic tracking
// notes this is the point to factor a shared helper rather than copy-pasting
// a third time. SubmitRunForm.tsx itself is left as-is here (already shipped
// and tested by sub-issue 3) — retrofitting it is left to sub-issue 7's
// app-shell pass, which touches routing/shared UI broadly anyway.

// ApiError is thrown by apiFetch for a non-2xx response, carrying the HTTP
// status and the API's own JSON error message (pkg/runsapi's
// errorResponse{"error": "..."} shape) when the body could be parsed, or a
// generic fallback otherwise.
export class ApiError extends Error {
  readonly status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

// apiFetch issues a fetch to the JSON API and decodes the response body as
// T, throwing ApiError for a non-2xx response (using the response's
// {"error": "..."} body when present) and letting a network-level failure
// (fetch itself rejecting) propagate as whatever error fetch threw. A
// response with no body (e.g. a 202 Accepted with only a status field, or
// truly empty) decodes as {} cast to T — callers that need specific fields
// should treat them as optional.
export async function apiFetch<T>(input: string, init?: RequestInit): Promise<T> {
  const response = await fetch(input, init)
  const body = await response.json().catch(() => null)

  if (!response.ok) {
    const message =
      body && typeof body.error === 'string'
        ? body.error
        : `Request failed with status ${response.status}.`

    throw new ApiError(response.status, message)
  }

  return (body ?? {}) as T
}

// describeNetworkError renders a non-ApiError failure from apiFetch (fetch
// itself rejecting — the server is unreachable, DNS failure, offline, etc.)
// as operator-facing text. The usual call-site pattern is
// `error instanceof ApiError ? error.message : describeNetworkError(error)`.
export function describeNetworkError(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error)

  return `Could not reach the server: ${message}`
}

// formatTimestamp renders an optional ISO timestamp (as pkg/runsapi's JSON
// responses carry start/close times) in the operator's local timezone, or an
// em dash when absent (e.g. a run that has not closed yet).
export function formatTimestamp(value?: string): string {
  return value ? new Date(value).toLocaleString() : '—'
}

// formatDuration renders the elapsed time between two ISO timestamps as a
// short human string ("2h 14m", "48s"), or an em dash when end is absent
// (the run this pair describes has not closed yet — see RunsTable.tsx,
// which shows "Running" instead of calling this for that case).
export function formatDuration(start: string, end?: string): string {
  if (!end) {
    return '—'
  }

  const millis = new Date(end).getTime() - new Date(start).getTime()
  if (!Number.isFinite(millis) || millis < 0) {
    return '—'
  }

  const totalSeconds = Math.round(millis / 1000)
  const hours = Math.floor(totalSeconds / 3600)
  const minutes = Math.floor((totalSeconds % 3600) / 60)
  const seconds = totalSeconds % 60

  if (hours > 0) {
    return `${hours}h ${minutes}m`
  }

  if (minutes > 0) {
    return `${minutes}m ${seconds}s`
  }

  return `${seconds}s`
}

// RunSummary mirrors pkg/runsapi.RunSummary's JSON shape, as returned in the
// GET /api/runs list (pkg/runsapi.RunsResponse) — the shared shape behind
// the sidebar's active-run check (activeRun.ts), the dashboard's current-run
// card and runs table (Dashboard.tsx, RunsTable.tsx, CurrentRunCard.tsx).
export interface RunSummary {
  workflowId: string
  runId: string
  status: string
  startTime: string
  closeTime?: string
}

export interface RunsResponse {
  runs: RunSummary[]
}

// statusBadgeClass renders one of Temporal's workflow execution statuses
// (pkg/runsapi.RunSummary.Status, e.g. "Running"/"Completed"/"Failed") as a
// themed badge class, shared by every view that shows a run's status
// (RunsTable, CurrentRunCard).
export function statusBadgeClass(status: string): string {
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
