import { createContext, useContext } from 'react'

// route.ts holds the router's pure logic and hooks (no JSX/components — see
// router.tsx, which is the "components: RouterProvider, Link" half of the
// same small hand-rolled router, split out purely so eslint's
// react-refresh/only-export-components rule (which requires a component
// file to export components only) stays satisfied; the two files are one
// conceptual unit and are meant to be read together.

// Route is every reachable view. "not-found" covers any path that does not
// match a known route (e.g. a stale bookmark), so the app always has
// something reasonable to render rather than a blank page.
export type Route =
  | { name: 'submit' }
  | { name: 'history' }
  | { name: 'run'; runId: string }
  | { name: 'not-found'; path: string }

// parseRoute maps a URL pathname to a Route.
export function parseRoute(pathname: string): Route {
  if (pathname === '/') {
    return { name: 'submit' }
  }

  if (pathname === '/history' || pathname === '/history/') {
    return { name: 'history' }
  }

  const runMatch = /^\/runs\/([^/]+)\/?$/.exec(pathname)
  if (runMatch) {
    return { name: 'run', runId: decodeURIComponent(runMatch[1]) }
  }

  return { name: 'not-found', path: pathname }
}

// runPath builds the URL for a given run ID, the inverse of parseRoute's
// "run" case — the one place that knows the "/runs/{id}" URL shape, so
// callers (SubmitRunForm's onViewRun wiring, RunHistory's row links) never
// have to hand-build it themselves.
export function runPath(runId: string): string {
  return `/runs/${encodeURIComponent(runId)}`
}

export interface RouterContextValue {
  route: Route
  navigate: (path: string) => void
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
// new browser history entry.
export function useNavigate(): (path: string) => void {
  return useRouterContext().navigate
}
