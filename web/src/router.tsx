import {
  useCallback,
  useEffect,
  useState,
  type AnchorHTMLAttributes,
  type ReactNode,
} from 'react'
import { RouterContext, parseRoute, useNavigate } from './route'

// router.tsx is a small hand-rolled client-side router, not a dependency
// like react-router-dom. The app only ever has three reachable views ("/",
// "/runs/{runID}", "/history"), each with at most one path parameter — a
// general-purpose router is more machinery than that surface needs, and
// CLAUDE.md's Tech Stack guidance prefers minimal dependencies where
// reasonable. Together with route.ts (the Route type, parseRoute/runPath,
// and the useRoute/useNavigate hooks — split into its own file purely to
// satisfy eslint's react-refresh/only-export-components rule, which wants a
// component file to export components only) this is the whole router: a
// context provider that tracks the current path and reacts to the
// browser's back/forward buttons via the native "popstate" event, and a
// <Link> that navigates via history.pushState instead of a full page load.
// See App.tsx's doc comment for how routes map to views.

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
    if (path !== window.location.pathname) {
      window.history.pushState({}, '', path)
    }

    setRoute(parseRoute(path))
  }, [])

  return <RouterContext.Provider value={{ route, navigate }}>{children}</RouterContext.Provider>
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
