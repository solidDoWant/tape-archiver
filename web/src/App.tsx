import { useEffect, useState, type ReactNode } from 'react'
import ConfigPage from './ConfigPage'
import RunDetail from './RunDetail'
import Dashboard from './Dashboard'
import TapesPage from './TapesPage'
import NotFoundPage from './NotFoundPage'
import LoginPage from './LoginPage'
import Sidebar from './Sidebar'
import { RouterProvider } from './router'
import { runPath, sanitizeRedirectPath, useNavigate, useRoute, type Route } from './route'
import { useTheme, type ThemePreference } from './theme'
import { useIdentity, type Identity } from './identity'
import {
  pillDotClass,
  pillToneClass,
  RunHeaderSetContext,
  RunHeaderStateContext,
  useRunHeaderInfo,
  type RunHeaderInfo,
  type RunHeaderTone,
} from './runHeader'

// App is the persistent shell for the tape-archiver web UI (issue #272's
// design-tokens/shell/login/404 foundation): a themed sidebar (Sidebar.tsx)
// with app-wide navigation wrapping a router-driven content area, an
// AuthGate deciding between the styled login page and that shell, and a
// hand-rolled router (router.tsx/route.ts — unchanged rationale, see those
// files' doc comments).
function App() {
  return (
    <RouterProvider>
      <AuthGate />
    </RouterProvider>
  )
}

// AuthGate is the top of the authenticated app: on every route change it
// decides whether to show the styled login page, a brief loading state, or
// the real app shell. This replaces pkg/webauth's previous behavior of
// 302-ing every unauthenticated page request straight to the IdP before the
// SPA ever loaded — every page request (not just "/") is now served the SPA
// unconditionally, and this is the client-side logic that reacts to GET
// /api/me reporting no session (see pkg/webauth's package doc comment and
// identity.ts's useIdentity).
function AuthGate() {
  const route = useRoute()
  const navigate = useNavigate()
  const identityState = useIdentity()

  // Theme state lives here — above the login-page/shell split — not in
  // Shell: useTheme() owns both applying the theme class and tracking live
  // OS preference changes under "Auto", and the login page (rendered
  // WITHOUT Shell) must keep tracking those too (issue #272's theme
  // acceptance criterion covers the login page explicitly). Shell just
  // receives the preference + setter for the sidebar's control.
  const [preference, , setPreference] = useTheme()

  useEffect(() => {
    if (route.name === 'history') {
      // "/history" is folded into the dashboard (issue #276's acceptance
      // criterion: navigating here must land the operator on "/", not just
      // render the same content under a second URL) — redirect immediately,
      // before even considering auth state, so an unauthenticated deep link
      // to "/history" still ends up bounced to "/login?redirect=%2F" rather
      // than a redirect that references a URL the app no longer serves.
      navigate('/')

      return
    }

    if (route.name === 'login') {
      if (identityState.status === 'authenticated') {
        // Already signed in (e.g. a bookmarked /login, or landing back here
        // after a successful callback that AuthGate itself triggered) —
        // move on to wherever the login attempt was headed.
        const redirect = sanitizeRedirectPath(new URLSearchParams(window.location.search).get('redirect'))
        navigate(redirect)
      }

      return
    }

    if (identityState.status === 'unauthenticated') {
      const redirect = window.location.pathname + window.location.search
      navigate(`/login?redirect=${encodeURIComponent(redirect)}`)
    }
  }, [route.name, identityState.status, navigate])

  if (route.name === 'login') {
    return <LoginPage />
  }

  if (identityState.status !== 'authenticated') {
    // "loading", or "unauthenticated" for the one render before the effect
    // above's navigate() to /login commits — either way, nothing gated
    // should render yet.
    return (
      <div className="flex min-h-screen items-center justify-center bg-bg text-[12.5px] text-text-dim">
        Loading…
      </div>
    )
  }

  return (
    <Shell
      identity={identityState.identity}
      preference={preference}
      onPreferenceChange={setPreference}
    />
  )
}

function pageTitle(route: Route): string {
  switch (route.name) {
    case 'dashboard':
      return 'Dashboard'
    case 'submit':
      return 'Start new run'
    case 'history':
      // Transient: AuthGate's effect redirects this route to "dashboard"
      // before the next render, so this title is never actually shown.
      return 'Dashboard'
    case 'run':
      return `Run ${route.runId}`
    case 'tapes':
      return 'Tapes'
    case 'not-found':
      return 'Not found'
    case 'login':
      // Unreachable: AuthGate renders LoginPage directly for this route
      // without ever mounting Shell.
      return ''
  }
}

interface ShellProps {
  identity: Identity
  preference: ThemePreference
  onPreferenceChange: (preference: ThemePreference) => void
}

// RunHeaderProvider holds the current run's header info (status + runtime),
// published by RunDetail and read by ShellHeader below — see runHeader.ts. The
// setter context wraps the state context so a publisher subscribes only to the
// (stable) setter and never re-renders when the info flows back to the header.
function RunHeaderProvider({ children }: { children: ReactNode }) {
  const [info, setInfo] = useState<RunHeaderInfo | null>(null)

  return (
    <RunHeaderSetContext.Provider value={setInfo}>
      <RunHeaderStateContext.Provider value={info}>{children}</RunHeaderStateContext.Provider>
    </RunHeaderSetContext.Provider>
  )
}

// RunStatusPill is the header's at-a-glance run status: a tone-coloured dot and
// label (RUNNING/PAUSED/COMPLETE/FAILED), matching the design mockup's header.
function RunStatusPill({ label, tone }: { label: string; tone: RunHeaderTone }) {
  return (
    <div className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 ${pillToneClass[tone]}`}>
      <span className={`h-2 w-2 rounded-full ${pillDotClass[tone]}`} aria-hidden="true" />
      <span className="font-mono text-[11px] font-semibold tracking-[0.03em]">{label}</span>
    </div>
  )
}

// ShellHeader is the single page-title bar every route shares. For a run it also
// shows the runtime line and status pill RunDetail published (runHeader.ts) — the
// header info the design mockup carries and the reason RunDetail renders no title
// bar of its own; other pages publish nothing, so it stays just the title.
function ShellHeader({ route }: { route: Route }) {
  const runInfo = useRunHeaderInfo()

  return (
    <header className="sticky top-0 z-5 flex items-center gap-3.5 border-b border-border bg-bg/80 px-5 py-4 backdrop-blur-sm sm:px-7">
      <div className="min-w-0">
        <div className="text-base font-semibold tracking-tight">{pageTitle(route)}</div>
        {runInfo?.runtime ? (
          <div className="mt-0.5 font-mono text-[11px] text-text-faint">{runInfo.runtime}</div>
        ) : null}
      </div>
      <div className="flex-1" />
      {runInfo ? <RunStatusPill label={runInfo.statusLabel} tone={runInfo.tone} /> : null}
    </header>
  )
}

// Shell renders the sidebar and the current route's view, once AuthGate has
// confirmed a session exists. Theme state comes from AuthGate (see its
// comment), not a useTheme() call of its own.
function Shell({ identity, preference, onPreferenceChange }: ShellProps) {
  const route = useRoute()
  const navigate = useNavigate()

  // flex-col below md: the sidebar stacks as a full-width block above the
  // content on narrow viewports (see Sidebar.tsx's own comment) so nothing
  // ever needs horizontal page scrolling.
  return (
    <RunHeaderProvider>
      <div className="flex min-h-screen flex-col bg-bg text-text md:flex-row">
        <Sidebar identity={identity} preference={preference} onPreferenceChange={onPreferenceChange} />

        <main className="flex min-w-0 flex-1 flex-col">
          <ShellHeader route={route} />

          <div className="flex flex-1 flex-col overflow-auto">{renderRoute(route, navigate)}</div>
        </main>
      </div>
    </RunHeaderProvider>
  )
}

function renderRoute(route: Route, navigate: (path: string) => void) {
  switch (route.name) {
    case 'dashboard':
      return <Dashboard onStartRun={() => navigate('/submit')} />
    case 'submit':
      // ConfigPage lays out its own full-width max-w-3xl content area
      // (issue #279 — richer than the other pre-redesign pages' centered
      // narrow column), so it is not wrapped in CenteredView, matching
      // TapesPage/NotFoundPage's already-redesigned pattern below.
      return <ConfigPage onViewRun={(runId) => navigate(runPath(runId))} />
    case 'history':
      // Transient: AuthGate's effect redirects this route to "/" before the
      // next render (see route.ts's doc comment on the "history" variant).
      return null
    case 'run':
      // key={route.runId}: forces a fresh RunDetail mount (and thus a fresh
      // EventSource + reset display state) whenever the viewed run changes,
      // rather than RunDetail resetting its own state from inside an effect
      // on a prop change — see RunDetail's doc comment. Full-width, like
      // TapesPage below — the redesigned phase-rail + detail-pane layout
      // (issue #277) owns its own padding/centering rather than getting it
      // from CenteredView, which the old single-pane view relied on.
      return <RunDetail key={route.runId} runId={route.runId} />
    case 'tapes':
      return <TapesPage />
    case 'not-found':
      return <NotFoundPage path={route.path} />
    case 'login':
      // Unreachable — see pageTitle's comment.
      return null
  }
}

export default App
