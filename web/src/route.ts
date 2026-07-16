import { createContext, useContext } from 'react'

// route.ts holds the router's pure logic and hooks (no JSX/components — see
// router.tsx, which is the "components: RouterProvider, Link" half of the
// same small hand-rolled router, split out purely so eslint's
// react-refresh/only-export-components rule (which requires a component
// file to export components only) stays satisfied; the two files are one
// conceptual unit and are meant to be read together.

// Route is every reachable view. "not-found" covers any path that does not
// match a known route (e.g. a stale bookmark), so the app always has
// something reasonable to render rather than a blank page. "login" is the
// styled pre-redirect login screen (issue #272) — reachable directly (a
// deep link, or a hard reload while unauthenticated, both now served the
// SPA rather than an unstyled server redirect — see pkg/webauth's package
// doc comment) and via AuthGate's own client-side redirect when an API call
// reports no session. "history" is likewise never rendered directly (issue
// #276): App.tsx redirects it to "dashboard" the moment it is seen, so a
// bookmarked/linked "/history" URL still lands the operator on "/" per that
// issue's acceptance criterion, rather than requiring every inbound link to
// be updated at once.
export type Route =
  | { name: 'login' }
  | { name: 'dashboard' }
  | { name: 'submit' }
  | { name: 'history' }
  | { name: 'run'; runId: string }
  | { name: 'tapes' }
  | { name: 'not-found'; path: string }

// parseRoute maps a URL path to a Route. The input may carry a query string
// (e.g. "/login?error=denied&redirect=/runs/abc" — AuthGate/LoginPage build
// these) or just a pathname (window.location.pathname); only the path
// portion decides which Route this resolves to; a view that needs the query
// string itself (LoginPage's error/redirect params) reads
// window.location.search directly rather than the Route.
export function parseRoute(rawPath: string): Route {
  const pathname = rawPath.split(/[?#]/)[0] || '/'

  if (pathname === '/') {
    return { name: 'dashboard' }
  }

  // The submit form used to live at "/" (issue #272); it moved to "/submit"
  // so "/" could become the dashboard (issue #276) without losing "Start new
  // run" as a directly linkable/bookmarkable URL.
  if (pathname === '/submit' || pathname === '/submit/') {
    return { name: 'submit' }
  }

  if (pathname === '/history' || pathname === '/history/') {
    return { name: 'history' }
  }

  if (pathname === '/tapes' || pathname === '/tapes/') {
    return { name: 'tapes' }
  }

  if (pathname === '/login' || pathname === '/login/') {
    return { name: 'login' }
  }

  const runMatch = /^\/runs\/([^/]+)\/?$/.exec(pathname)
  if (runMatch) {
    // decodeURIComponent throws on a malformed percent-encoded segment
    // (e.g. a stray "%" from a hand-typed or stale URL) — this file has no
    // ErrorBoundary above it, so an uncaught throw here would crash the
    // whole app to a blank white screen instead of falling through to the
    // "not-found" route this function's doc comment promises.
    try {
      return { name: 'run', runId: decodeURIComponent(runMatch[1]) }
    } catch {
      return { name: 'not-found', path: pathname }
    }
  }

  return { name: 'not-found', path: pathname }
}

// sanitizeRedirectPath restricts a caller-supplied post-login redirect to a
// same-origin absolute path, defaulting to "/" for anything else — the
// client-side mirror of pkg/webauth's identically-named server-side
// function (which re-validates it again before ever using it to build an
// IdP redirect; this copy exists so the SPA never even hands a malformed or
// cross-origin value to history.pushState / window.location.assign in the
// first place). See that function's doc comment for why "//" and "\" are
// rejected specifically.
export function sanitizeRedirectPath(path: string | null | undefined): string {
  if (!path || !path.startsWith('/') || path.startsWith('//') || path.includes('\\')) {
    return '/'
  }

  return path
}

// runPath builds the URL for a given run ID, the inverse of parseRoute's
// "run" case — the one place that knows the "/runs/{id}" URL shape, so
// callers (SubmitRunForm's onViewRun wiring, RunHistory's row links) never
// have to hand-build it themselves.
export function runPath(runId: string): string {
  return `/runs/${encodeURIComponent(runId)}`
}

// submitPath builds the URL for the "Start new run" config page (parseRoute's
// "submit" route). With a fromRunId it adds the ?from=<runId> query the config
// page reads (App.tsx → ConfigPage's restartFromRunId) to preload that run's
// config for a restart — RestartRunButton's target. The query is preserved in
// the URL but ignored by parseRoute (which resolves on pathname alone), the
// same read-the-query-directly pattern LoginPage uses for its params.
export function submitPath(fromRunId?: string): string {
  return fromRunId ? `/submit?from=${encodeURIComponent(fromRunId)}` : '/submit'
}

// NavigateOptions tunes a navigation. replace swaps the current history entry
// (history.replaceState) instead of pushing a new one — used for redirect-only
// hops (App.tsx bouncing "/history" → "/", or the login gate) so the browser's
// Back button does not land on the redirecting URL and immediately bounce
// forward again, trapping the user.
export interface NavigateOptions {
  replace?: boolean
}

export interface RouterContextValue {
  route: Route
  navigate: (path: string, options?: NavigateOptions) => void
}

// RouterContext is populated by router.tsx's RouterProvider; exported so
// that file can supply it without this one importing anything JSX-shaped.
export const RouterContext = createContext<RouterContextValue | null>(null)

function useRouterContext(): RouterContextValue {
  const context = useContext(RouterContext)

  if (!context) {
    throw new Error('useRoute/useNavigate/Link must be used within a RouterProvider')
  }

  return context
}

// useRoute returns the currently active Route, re-rendering the caller on
// every navigation (via Link, useNavigate, or the browser's back/forward).
export function useRoute(): Route {
  return useRouterContext().route
}

// useNavigate returns a stable function that navigates to path, pushing a
// new browser history entry (or replacing the current one with
// { replace: true } — see NavigateOptions).
export function useNavigate(): (path: string, options?: NavigateOptions) => void {
  return useRouterContext().navigate
}
