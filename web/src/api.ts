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
