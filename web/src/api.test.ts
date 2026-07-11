import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, apiFetch, onSessionExpired } from './api'

// jsonResponse builds a minimal fetch Response stand-in, matching the shape
// apiFetch reads (ok/status/json()).
function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('apiFetch', () => {
  it('decodes a 2xx JSON body as T', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(200, { workflowId: 'run-1' })))

    await expect(apiFetch('/api/runs/run-1')).resolves.toEqual({ workflowId: 'run-1' })
  })

  it('throws ApiError with the response status and body message for a non-2xx response', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(500, { error: 'boom' })))

    await expect(apiFetch('/api/runs')).rejects.toMatchObject({
      status: 500,
      message: 'boom',
    })
  })

  // issue #285: apiFetch is the one place that sees every /api/* response,
  // so it is where mid-session 401s (session expired past
  // maxSessionDuration) get turned into a signal the rest of the app can
  // react to (identity.ts's useIdentity subscribes — see identity.test.ts).
  it('notifies onSessionExpired subscribers on a 401 response', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(401, { error: 'unauthorized' })))

    const listener = vi.fn()
    const unsubscribe = onSessionExpired(listener)

    await expect(apiFetch('/api/runs')).rejects.toBeInstanceOf(ApiError)

    expect(listener).toHaveBeenCalledTimes(1)
    unsubscribe()
  })

  // The backend's 401 is treated as authoritative (no debounce/confirmation
  // step — see api.ts's doc comment on apiFetch), but that only applies to
  // an actual 401 response. Anything else — a different error status, or a
  // network-level failure such as a flaky proxy hop dropping the
  // connection entirely — must not be mistaken for session loss.
  it('does not notify onSessionExpired subscribers for a non-401 error status', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(503, { error: 'unavailable' })))

    const listener = vi.fn()
    const unsubscribe = onSessionExpired(listener)

    await expect(apiFetch('/api/runs')).rejects.toBeInstanceOf(ApiError)

    expect(listener).not.toHaveBeenCalled()
    unsubscribe()
  })

  it('does not notify onSessionExpired subscribers when fetch itself rejects (network failure)', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))

    const listener = vi.fn()
    const unsubscribe = onSessionExpired(listener)

    await expect(apiFetch('/api/runs')).rejects.toBeInstanceOf(TypeError)

    expect(listener).not.toHaveBeenCalled()
    unsubscribe()
  })

  it('stops notifying a subscriber once it has unsubscribed', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(401, { error: 'unauthorized' })))

    const listener = vi.fn()
    const unsubscribe = onSessionExpired(listener)
    unsubscribe()

    await expect(apiFetch('/api/runs')).rejects.toBeInstanceOf(ApiError)

    expect(listener).not.toHaveBeenCalled()
  })
})
