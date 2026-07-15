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

// sessionExpiredListeners backs onSessionExpired/notifySessionExpired below
// (issue #285): a tiny module-level pub-sub so apiFetch — the one place that
// sees every /api/* response — can tell the rest of the app "the session is
// gone" without importing React or any component. identity.ts's
// useIdentity is the sole subscriber today (it turns a notification into
// IdentityState going to "unauthenticated", which App.tsx's AuthGate effect
// already redirects to /login?redirect=(current path) for); the pub-sub
// shape (rather than a direct identity.ts import here) keeps api.ts
// free of any dependency on identity.ts/React.
const sessionExpiredListeners = new Set<() => void>()

// onSessionExpired registers listener to be called every time apiFetch
// observes a 401 response, and returns an unsubscribe function. Intended for
// a component that lives for the app's lifetime (identity.ts's useIdentity,
// mounted by App.tsx's AuthGate) to subscribe once and react to session loss
// discovered by any fetch anywhere in the app, not just its own.
export function onSessionExpired(listener: () => void): () => void {
  sessionExpiredListeners.add(listener)

  return () => {
    sessionExpiredListeners.delete(listener)
  }
}

function notifySessionExpired(): void {
  for (const listener of sessionExpiredListeners) {
    listener()
  }
}

// apiFetch issues a fetch to the JSON API and decodes the response body as
// T, throwing ApiError for a non-2xx response (using the response's
// {"error": "..."} body when present) and letting a network-level failure
// (fetch itself rejecting) propagate as whatever error fetch threw. A
// response with no body (e.g. a 202 Accepted with only a status field, or
// truly empty) decodes as {} cast to T — callers that need specific fields
// should treat them as optional.
//
// A 401 status specifically also fires the onSessionExpired pub-sub above
// (issue #285's mid-session-expiry fix), on the principle that the backend's
// 401 is authoritative: pkg/webauth issues 401 only for a genuinely missing
// or invalid session (webauth.go), never for e.g. a proxy hiccup or a
// network blip — those surface as fetch() itself rejecting (a non-ApiError,
// handled distinctly by every existing caller already) or as a different
// status code, neither of which reaches this branch. So there is no
// debounce/retry/confirmation step here: a single 401 is treated as session
// loss immediately, on this response alone.
export async function apiFetch<T>(input: string, init?: RequestInit): Promise<T> {
  const response = await fetch(input, init)
  const body = await response.json().catch(() => null)

  if (!response.ok) {
    const message =
      body && typeof body.error === 'string'
        ? body.error
        : `Request failed with status ${response.status}.`

    if (response.status === 401) {
      notifySessionExpired()
    }

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
  // dryRun is true when the run was submitted as a dry-run (the mhvtl override).
  // Read back from the run's Temporal memo (pkg/runsapi RunSummary.DryRun); a run
  // predating the memo, or a production run, is false.
  dryRun: boolean
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
      return 'bg-blue-bg text-blue'
    case 'Completed':
      return 'bg-green-bg text-green'
    case 'Failed':
    case 'Terminated':
    case 'TimedOut':
      return 'bg-red-bg text-red'
    // Canceled shares the neutral tone runStatusView (runHeader.ts) gives it, so
    // a canceled run reads the same in the dashboard table/card as in its own
    // run-page header rather than one amber and one grey.
    case 'Canceled':
      return 'bg-inset text-text-dim'
    default:
      return 'bg-inset text-text-dim'
  }
}
