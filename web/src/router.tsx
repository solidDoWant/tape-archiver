import {
  useCallback,
  useEffect,
  useMemo,
  useState,
  type AnchorHTMLAttributes,
  type ReactNode,
} from 'react'
import { RouterContext, parseRoute, useNavigate } from './route'

// router.tsx is a small hand-rolled client-side router, not a dependency
// like react-router-dom. The app only ever has a handful of reachable views
// ("/" the dashboard, "/submit", "/runs/{runID}", "/tapes", "/login"), each
// with at most one path parameter, plus "/history" as a redirect-only alias
// of "/" (issue #276) — a general-purpose router is more machinery than that
// surface needs, and CLAUDE.md's Tech Stack guidance prefers minimal
// dependencies where reasonable. Together with route.ts (the Route type,
// parseRoute/runPath, and the useRoute/useNavigate hooks — split into its
// own file purely to satisfy eslint's react-refresh/only-export-components
// rule, which wants a component file to export components only) this is the
// whole router: a context provider that tracks the current path and reacts
// to the browser's back/forward buttons via the native "popstate" event,
// and a <Link> that navigates via history.pushState instead of a full page
// load. See App.tsx's doc comment for how routes map to views.

// RouterProvider owns "which route is current" as state, seeded from
// window.location on mount, and keeps it in sync in two ways: navigate()
// (called by Link and any component via useNavigate) pushes a new history
// entry and updates state directly, while a "popstate" listener updates
// state when the browser's own back/forward buttons move through history
// entries navigate() already pushed (pushState alone never fires popstate,
// which is exactly why the pre-sub-issue-7 App.tsx navigation did not
// support back/forward — this listener is the fix).
export function RouterProvider({ children }: { children: ReactNode }) {
  const [route, setRoute] = useState(() => parseRoute(window.location.pathname))

  useEffect(() => {
    const onPopState = () => setRoute(parseRoute(window.location.pathname))

    window.addEventListener('popstate', onPopState)

    return () => window.removeEventListener('popstate', onPopState)
  }, [])

  const navigate = useCallback((path: string) => {
    if (path === window.location.pathname + window.location.search) {
      // Already there: skip both the history push (would leave a no-op
      // entry, e.g. clicking a nav link for the page you're already on)
      // and the state update — setRoute(parseRoute(path)) would otherwise
      // always re-render every context consumer even though parseRoute
      // returns a brand-new object for an identical route, since React's
      // Object.is bail-out never gets a chance to apply.
      //
      // The comparison includes the query string, not just the pathname:
      // "/submit?from=B" and "/submit" are different destinations (the query
      // preloads a restart config — route.ts's submitPath), so a navigation
      // that only drops or changes the query — e.g. "Start new run" from a
      // pre-filled restart form — must not be swallowed as a no-op.
      return
    }

    window.history.pushState({}, '', path)
    setRoute(parseRoute(path))
  }, [])

  // Memoized so consumers that only need `navigate` (e.g. every <Link>) do
  // not re-render on every route change — without this, a fresh object
  // literal here on every RouterProvider render would change the context
  // value's identity whenever `route` changes, even though `navigate`
  // itself is referentially stable (useCallback, empty deps).
  const value = useMemo(() => ({ route, navigate }), [route, navigate])

  return <RouterContext.Provider value={value}>{children}</RouterContext.Provider>
}

export interface LinkProps extends Omit<AnchorHTMLAttributes<HTMLAnchorElement>, 'href'> {
  to: string
  children: ReactNode
}

// Link renders a real <a href> (so middle-click/ctrl-click "open in new
// tab", right-click "copy link", and screen-reader link semantics all keep
// working exactly like a normal link) but intercepts a plain left click to
// navigate client-side via the router instead of a full page load.
export function Link({ to, children, onClick, ...rest }: LinkProps) {
  const navigate = useNavigate()

  return (
    <a
      href={to}
      onClick={(event) => {
        onClick?.(event)

        if (
          event.defaultPrevented ||
          event.button !== 0 ||
          event.metaKey ||
          event.ctrlKey ||
          event.shiftKey ||
          event.altKey
        ) {
          return
        }

        event.preventDefault()
        navigate(to)
      }}
      {...rest}
    >
      {children}
    </a>
  )
}
