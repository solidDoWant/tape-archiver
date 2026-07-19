import { useEffect, useState } from 'react'
import { apiFetch, onSessionExpired } from './api'

// Identity mirrors pkg/webauth.Identity's JSON shape, as returned by GET
// /api/me.
export interface Identity {
  subject: string
  email?: string
  name?: string
}

// IdentityState is the three states GET /api/me resolves to. "loading"
// covers the brief window before the first response; App.tsx's AuthGate
// treats that the same as "authenticated" is not yet known, and holds off
// rendering either the login page or the shell.
export type IdentityState =
  | { status: 'loading' }
  | { status: 'authenticated'; identity: Identity }
  | { status: 'unauthenticated' }

// useIdentity is how the SPA now learns whether the browser has a session,
// replacing pkg/webauth's old behavior of 302-ing an unauthenticated page
// request straight to the IdP before the SPA ever loaded. Every non-API
// page request is served the SPA unconditionally now (see pkg/webauth's
// package doc comment); this hook is what lets the client decide, from
// inside the already-loaded app, whether to show the styled login page.
//
// Besides the one-shot /api/me check at mount, this also subscribes to
// api.ts's onSessionExpired for the component's whole lifetime (issue #285):
// any apiFetch call anywhere in the app that gets back a 401 — a session
// that expired mid-use, e.g. past maxSessionDuration — flips this straight
// to "unauthenticated" without a redundant re-check against /api/me (the
// 401 that triggered it is already authoritative). App.tsx's AuthGate is
// the sole caller of this hook, and its existing effect already reacts to
// "unauthenticated" by redirecting to /login?redirect=(current path) — that
// effect needs no changes for mid-session expiry to reach the login page,
// since it does not distinguish "unauthenticated" learned at mount from
// "unauthenticated" learned later via this subscription.
export function useIdentity(): IdentityState {
  const [state, setState] = useState<IdentityState>({ status: 'loading' })

  useEffect(() => {
    let cancelled = false

    async function load() {
      try {
        const identity = await apiFetch<Identity>('/api/me')
        if (cancelled) {
          return
        }

        setState({ status: 'authenticated', identity })
      } catch {
        if (cancelled) {
          return
        }

        // Both a real 401 (ApiError) and a network-level failure (fetch
        // itself rejecting — the server is unreachable) are treated the
        // same, as "unauthenticated": rendering the shell would just fail
        // every subsequent API call anyway, and the login page's own
        // sign-in button surfaces a real error if the server truly is
        // down.
        setState({ status: 'unauthenticated' })
      }
    }

    void load()

    return () => {
      cancelled = true
    }
  }, [])

  useEffect(() => {
    return onSessionExpired(() => {
      setState({ status: 'unauthenticated' })
    })
  }, [])

  return state
}
