import { renderHook, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { apiFetch } from './api'
import { useIdentity } from './identity'

// jsonResponse builds a minimal fetch Response stand-in, matching the shape
// apiFetch reads (ok/status/json()).
function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

const testIdentity = { subject: 'user-1', email: 'operator@example.com', name: 'Test Operator' }

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('useIdentity', () => {
  it('resolves to authenticated from a 200 /api/me at mount', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(200, testIdentity)))

    const { result } = renderHook(() => useIdentity())

    await waitFor(() => {
      expect(result.current).toEqual({ status: 'authenticated', identity: testIdentity })
    })
  })

  // issue #285: a mid-session 401 — an in-app fetch made well after mount,
  // long after the initial /api/me check settled — must flip an already
  // "authenticated" hook straight to "unauthenticated" without waiting for
  // (or triggering) another /api/me round trip. App.tsx's AuthGate reacts
  // to that transition the same way it reacts to the initial mount finding
  // no session — see App.test.tsx for the end-to-end redirect behavior.
  it('flips from authenticated to unauthenticated when any apiFetch call observes a 401 later', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(200, testIdentity)))

    const { result } = renderHook(() => useIdentity())

    await waitFor(() => {
      expect(result.current).toEqual({ status: 'authenticated', identity: testIdentity })
    })

    // Simulate some unrelated component's data fetch discovering the
    // session is gone (e.g. RunDetail/TapesPage/PauseActions — any apiFetch
    // caller), long after useIdentity's own mount-time check already
    // resolved.
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(401, { error: 'unauthorized' })))
    await expect(apiFetch('/api/runs')).rejects.toMatchObject({ status: 401 })

    await waitFor(() => {
      expect(result.current).toEqual({ status: 'unauthenticated' })
    })
  })

  it('resolves to unauthenticated when the mount-time /api/me itself 401s', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(401, { error: 'unauthorized' })))

    const { result } = renderHook(() => useIdentity())

    await waitFor(() => {
      expect(result.current).toEqual({ status: 'unauthenticated' })
    })
  })

  it('unsubscribes from session-expiry notifications on unmount', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(200, testIdentity)))

    const { result, unmount } = renderHook(() => useIdentity())

    await waitFor(() => {
      expect(result.current).toEqual({ status: 'authenticated', identity: testIdentity })
    })

    unmount()

    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(401, { error: 'unauthorized' })))
    // Nothing to assert on result.current post-unmount (React Testing
    // Library detaches it); this only needs to prove the notify call does
    // not throw (e.g. from calling setState on an unmounted component).
    await expect(apiFetch('/api/runs')).rejects.toMatchObject({ status: 401 })
  })
})
