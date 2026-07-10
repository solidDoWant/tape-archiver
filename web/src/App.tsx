import { useEffect, type ReactNode } from 'react'
import SubmitRunForm from './SubmitRunForm'
import RunDetail from './RunDetail'
import RunHistory from './RunHistory'
import TapesPage from './TapesPage'
import NotFoundPage from './NotFoundPage'
import LoginPage from './LoginPage'
import Sidebar from './Sidebar'
import { RouterProvider } from './router'
import { runPath, sanitizeRedirectPath, useNavigate, useRoute, type Route } from './route'
import { useTheme, type ThemePreference } from './theme'
import { useIdentity, type Identity } from './identity'

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
    case 'submit':
      return 'Start new run'
    case 'history':
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
    <div className="flex min-h-screen flex-col bg-bg text-text md:flex-row">
      <Sidebar identity={identity} preference={preference} onPreferenceChange={onPreferenceChange} />

      <main className="flex min-w-0 flex-1 flex-col">
        <header className="sticky top-0 z-5 border-b border-border bg-bg/80 px-5 py-4 backdrop-blur-sm sm:px-7">
          <div className="text-base font-semibold tracking-tight">{pageTitle(route)}</div>
        </header>

        <div className="flex flex-1 flex-col overflow-auto">{renderRoute(route, navigate)}</div>
      </main>
    </div>
  )
}

// CenteredView is the shared content wrapper for the pre-redesign pages
// (submit form, run history, run detail), which lay themselves out as
// centered max-width columns — the padding/centering they used to get from
// the old shell's `main`. The redesigned pages (Tapes, 404) own their whole
// content area instead.
function CenteredView({ children }: { children: ReactNode }) {
  return <div className="flex flex-col items-center gap-6 p-6 sm:p-7">{children}</div>
}

function renderRoute(route: Route, navigate: (path: string) => void) {
  switch (route.name) {
    case 'submit':
      return (
        <CenteredView>
          <SubmitRunForm onViewRun={(runId) => navigate(runPath(runId))} />
        </CenteredView>
      )
    case 'history':
      return (
        <CenteredView>
          <RunHistory />
        </CenteredView>
      )
    case 'run':
      // key={route.runId}: forces a fresh RunDetail mount (and thus a fresh
      // EventSource + reset display state) whenever the viewed run changes,
      // rather than RunDetail resetting its own state from inside an effect
      // on a prop change — see RunDetail's doc comment.
      return (
        <CenteredView>
          <RunDetail key={route.runId} runId={route.runId} />
        </CenteredView>
      )
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
