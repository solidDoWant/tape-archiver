import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import App from './App'

// MinimalEventSource stands in for the browser's real EventSource (not
// implemented by jsdom): App renders RunDetail once navigated to a run,
// and RunDetail opens one on mount, so any test that reaches that view
// needs a stand-in that at least does not throw. RunDetail.test.tsx covers
// the SSE behavior itself in detail; these tests only care about
// navigation.
class MinimalEventSource {
  url: string

  constructor(url: string) {
    this.url = url
  }

  addEventListener() {
    // no-op: no test here drives an event through this connection.
  }

  close() {
    // no-op
  }
}

// jsonResponse builds a minimal fetch Response stand-in, matching the shape
// api.ts reads (ok/status/json()).
function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

// The identity /api/me returns for authenticated tests.
const testIdentity = { subject: 'user-1', email: 'operator@example.com', name: 'Test Operator' }

// stubApi installs a fetch mock routing the endpoints the shell itself hits
// on mount (AuthGate's /api/me, the Footer's /api/build-info, the Sidebar's
// active-run check via /api/runs), with per-test overrides. Unlisted URLs
// 404. Returns the mock for call assertions.
function stubApi(overrides: Record<string, { status: number; body: unknown }> = {}) {
  const routes: Record<string, { status: number; body: unknown }> = {
    '/api/me': { status: 200, body: testIdentity },
    '/api/build-info': { status: 200, body: { version: 'v-test' } },
    '/api/runs': { status: 200, body: { runs: [] } },
    // The dashboard (root path, issue #276) always fetches its library card
    // from GET /api/tapes regardless of which test/route is under test;
    // stubbed here so most tests don't need to care about it.
    '/api/tapes': { status: 200, body: { tapes: [] } },
    ...overrides,
  }

  const fetchMock = vi.fn((input: string) => {
    const url = typeof input === 'string' ? input : String(input)
    const route = routes[url.split('?')[0]]

    if (!route) {
      return Promise.resolve(jsonResponse(404, { error: `no stub for ${url}` }))
    }

    return Promise.resolve(jsonResponse(route.status, route.body))
  })

  vi.stubGlobal('fetch', fetchMock)

  return fetchMock
}

// stubControllableMatchMedia replaces window.matchMedia with one whose
// reported OS color-scheme preference a test can flip mid-session via the
// returned fireChange (matchMedia's real "change" event) — the same helper
// theme.test.ts uses, duplicated here because it is test scaffolding, not
// exportable product code.
function stubControllableMatchMedia(initialMatches: boolean) {
  let matches = initialMatches
  let changeListener: ((event: MediaQueryListEvent) => void) | null = null

  vi.stubGlobal('matchMedia', (query: string) => ({
    get matches() {
      return matches
    },
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: (_event: string, listener: (event: MediaQueryListEvent) => void) => {
      changeListener = listener
    },
    removeEventListener: () => {
      changeListener = null
    },
    dispatchEvent: () => false,
  }))

  return {
    fireChange(nextMatches: boolean) {
      matches = nextMatches
      changeListener?.({ matches: nextMatches } as MediaQueryListEvent)
    },
  }
}

beforeEach(() => {
  vi.stubGlobal('EventSource', MinimalEventSource)
  window.history.pushState({}, '', '/')
  document.documentElement.classList.remove('dark')
  window.localStorage.clear()
})

afterEach(() => {
  vi.unstubAllGlobals()
  window.history.pushState({}, '', '/')
  document.documentElement.classList.remove('dark')
  window.localStorage.clear()
})

// renderAuthenticated renders App with a session (stubbed /api/me) and
// waits for AuthGate's loading state to resolve into the shell.
async function renderAuthenticated(overrides: Record<string, { status: number; body: unknown }> = {}) {
  const fetchMock = stubApi(overrides)

  render(<App />)

  await waitFor(() => {
    expect(screen.getByRole('navigation', { name: 'Main' })).toBeInTheDocument()
  })

  return fetchMock
}

describe('App shell (authenticated)', () => {
  it('renders the sidebar nav items, the operator identity, and the dashboard at the root path', async () => {
    await renderAuthenticated()

    expect(screen.getByRole('link', { name: /dashboard/i })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /start new run/i })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /tapes/i })).toBeInTheDocument()

    // Signed-in operator's name and email (from /api/me) in the sidebar.
    expect(screen.getByText('Test Operator')).toBeInTheDocument()
    expect(screen.getByText('operator@example.com')).toBeInTheDocument()

    // Root path renders the new dashboard (issue #276) inside the shell —
    // Dashboard.test.tsx covers its states in detail; this just proves App
    // wires the route to it.
    await waitFor(() => {
      expect(screen.getByText('No runs yet')).toBeInTheDocument()
    })
  })

  it('renders the config page at "/submit"', async () => {
    await renderAuthenticated()

    fireEvent.click(screen.getByRole('link', { name: /start new run/i }))

    expect(window.location.pathname).toBe('/submit')
    // The config page (issue #279) — ConfigPage.test.tsx covers its
    // behavior in detail; this just proves App wires the route to it.
    await waitFor(() => {
      expect(screen.getByRole('group', { name: /config input mode/i })).toBeInTheDocument()
    })
  })

  it('shows the Light/Dark/Auto theme control and applies an explicit Dark choice', async () => {
    await renderAuthenticated()

    expect(document.documentElement.classList.contains('dark')).toBe(false)

    fireEvent.click(screen.getByRole('button', { name: /dark/i }))

    expect(document.documentElement.classList.contains('dark')).toBe(true)
    expect(window.localStorage.getItem('tape-archiver:theme')).toBe('dark')

    fireEvent.click(screen.getByRole('button', { name: /light/i }))

    expect(document.documentElement.classList.contains('dark')).toBe(false)
    expect(window.localStorage.getItem('tape-archiver:theme')).toBe('light')

    fireEvent.click(screen.getByRole('button', { name: /auto/i }))

    expect(window.localStorage.getItem('tape-archiver:theme')).toBe('auto')
    // setupTests' matchMedia stub reports light, so Auto resolves to light.
    expect(document.documentElement.classList.contains('dark')).toBe(false)
  })

  it('disables "Start new run" with an explanation while a run is active', async () => {
    await renderAuthenticated({
      '/api/runs': {
        status: 200,
        body: {
          runs: [
            {
              workflowId: 'backup',
              runId: 'run-live',
              status: 'Running',
              startTime: '2026-07-01T00:00:00Z',
            },
          ],
        },
      },
    })

    await waitFor(() => {
      expect(screen.queryByRole('link', { name: /start new run/i })).not.toBeInTheDocument()
    })

    // Scoped to the nav: the "/" page header also reads "Start new run".
    const nav = screen.getByRole('navigation', { name: 'Main' })
    const disabledItem = within(nav).getByText('Start new run').closest('[aria-disabled="true"]')
    expect(disabledItem).not.toBeNull()

    // The explanation is announced to assistive tech, not just shown on
    // hover: the focusable disabled item must reference the tooltip text
    // via aria-describedby.
    const tooltip = screen.getByRole('tooltip')
    expect(tooltip).toHaveTextContent(/already in progress/i)
    expect(tooltip.id).not.toBe('')
    expect(disabledItem).toHaveAttribute('aria-describedby', tooltip.id)
  })

  it('navigates back to the dashboard (with the embedded runs table) via the sidebar', async () => {
    window.history.pushState({}, '', '/submit')

    await renderAuthenticated({
      '/api/runs': {
        status: 200,
        body: {
          runs: [
            {
              workflowId: 'backup',
              runId: 'run-1',
              status: 'Completed',
              startTime: '2026-07-01T00:00:00Z',
              closeTime: '2026-07-01T02:00:00Z',
            },
          ],
        },
      },
    })

    fireEvent.click(screen.getByRole('link', { name: /dashboard/i }))

    await waitFor(() => {
      expect(screen.getByRole('link', { name: 'run-1' })).toBeInTheDocument()
    })
    expect(window.location.pathname).toBe('/')
  })

  it('renders the run detail view directly when the URL already points at a run', async () => {
    window.history.pushState({}, '', '/runs/run-xyz')

    await renderAuthenticated()

    expect(screen.getByRole('heading', { name: /run run-xyz/i })).toBeInTheDocument()
    expect(screen.queryByRole('form', { name: /submit backup run/i })).not.toBeInTheDocument()
  })

  it('navigates from a history row straight to that run detail view', async () => {
    window.history.pushState({}, '', '/history')

    await renderAuthenticated({
      '/api/runs': {
        status: 200,
        body: {
          runs: [
            {
              workflowId: 'backup',
              runId: 'run-1',
              status: 'Completed',
              startTime: '2026-07-01T00:00:00Z',
            },
          ],
        },
      },
    })

    await waitFor(() => {
      expect(screen.getByRole('link', { name: 'run-1' })).toBeInTheDocument()
    })

    fireEvent.click(screen.getByRole('link', { name: 'run-1' }))

    expect(screen.getByRole('heading', { name: /run run-1/i })).toBeInTheDocument()
    expect(window.location.pathname).toBe('/runs/run-1')
  })

  it('returns to the previous view via the browser back button', async () => {
    await renderAuthenticated()

    fireEvent.click(screen.getByRole('link', { name: /tapes/i }))
    expect(window.location.pathname).toBe('/tapes')

    window.history.back()

    await waitFor(() => {
      expect(window.location.pathname).toBe('/')
    })
    await waitFor(() => {
      expect(screen.getByText('No runs yet')).toBeInTheDocument()
    })
  })

  it('shows the Tapes placeholder page via the sidebar', async () => {
    await renderAuthenticated()

    fireEvent.click(screen.getByRole('link', { name: /tapes/i }))

    expect(screen.getByRole('heading', { name: /tapes/i })).toBeInTheDocument()
    expect(window.location.pathname).toBe('/tapes')
  })

  it('shows the 404 page, with the sidebar still present, for an unknown path', async () => {
    window.history.pushState({}, '', '/no-such-page')

    await renderAuthenticated()

    expect(screen.getByRole('heading', { name: /page not found/i })).toBeInTheDocument()
    expect(screen.getByText('/no-such-page')).toBeInTheDocument()

    // The shell (sidebar) is still there — the 404 view renders inside it.
    expect(screen.getByRole('navigation', { name: 'Main' })).toBeInTheDocument()

    fireEvent.click(screen.getByRole('link', { name: /back to dashboard/i }))
    expect(window.location.pathname).toBe('/')
  })

  it('redirects "/history" to "/", the dashboard now embedding what used to be the standalone history page', async () => {
    window.history.pushState({}, '', '/history')

    await renderAuthenticated()

    await waitFor(() => {
      expect(window.location.pathname).toBe('/')
    })
    await waitFor(() => {
      expect(screen.getByText('No runs yet')).toBeInTheDocument()
    })
  })

  it('redirects a bookmarked /login back into the app when already signed in', async () => {
    window.history.pushState({}, '', '/login?redirect=%2Ftapes')

    stubApi()
    render(<App />)

    await waitFor(() => {
      expect(window.location.pathname).toBe('/tapes')
    })
  })
})

describe('App auth gating (unauthenticated)', () => {
  it('shows the styled login page instead of any app content when /api/me reports no session', async () => {
    window.history.pushState({}, '', '/runs/run-abc')

    stubApi({ '/api/me': { status: 401, body: { error: 'unauthorized' } } })
    render(<App />)

    await waitFor(() => {
      expect(screen.getByRole('button', { name: /continue with sso/i })).toBeInTheDocument()
    })

    // Redirected to /login, preserving where the operator was headed.
    expect(window.location.pathname).toBe('/login')
    expect(window.location.search).toContain('redirect=%2Fruns%2Frun-abc')

    // Nothing gated leaked out.
    expect(screen.queryByRole('navigation', { name: 'Main' })).not.toBeInTheDocument()
  })

  it('keeps tracking live OS theme changes on the login page under Auto', async () => {
    const { fireChange } = stubControllableMatchMedia(false)

    stubApi({ '/api/me': { status: 401, body: { error: 'unauthorized' } } })
    render(<App />)

    await waitFor(() => {
      expect(screen.getByRole('button', { name: /continue with sso/i })).toBeInTheDocument()
    })

    // No stored preference — Auto by default, currently light.
    expect(document.documentElement.classList.contains('dark')).toBe(false)

    // The OS flips to dark mid-session; the login page (rendered WITHOUT
    // the authenticated shell, whose sidebar used to be the only thing
    // mounting the theme hook) must follow it live.
    fireChange(true)
    await waitFor(() => {
      expect(document.documentElement.classList.contains('dark')).toBe(true)
    })

    // ... and back.
    fireChange(false)
    await waitFor(() => {
      expect(document.documentElement.classList.contains('dark')).toBe(false)
    })
  })
})

// issue #285: a session that expires mid-use (past maxSessionDuration) must
// route the operator back to the styled login page instead of leaving a
// dead component-level "unauthorized" error in place — and, on sign-in,
// land them back where they were. The unauthenticated-first-load flow above
// (App auth gating) is /api/me itself 401ing at mount; this covers a
// session that was good at mount and is discovered lost later, by some
// other in-app fetch (api.ts's apiFetch -> onSessionExpired ->
// identity.ts's useIdentity -> this same AuthGate redirect effect).
describe('App auth gating (mid-session 401)', () => {
  it('routes to the login page, preserving the current path, when an in-app fetch discovers the session is gone', async () => {
    const fetchMock = await renderAuthenticated()

    await waitFor(() => {
      expect(screen.getByText('No runs yet')).toBeInTheDocument()
    })

    // From here on, any /api/tapes call (TapesPage's own fetch, reached via
    // the sidebar) reports the session is gone — as it would once the
    // browser's session cookie is past maxSessionDuration — while every
    // other endpoint keeps behaving as before.
    fetchMock.mockImplementation((input: string) => {
      const url = typeof input === 'string' ? input : String(input)

      if (url.split('?')[0] === '/api/tapes') {
        return Promise.resolve(jsonResponse(401, { error: 'unauthorized' }))
      }

      return Promise.resolve(jsonResponse(200, { runs: [] }))
    })

    fireEvent.click(screen.getByRole('link', { name: /tapes/i }))

    await waitFor(() => {
      expect(screen.getByRole('button', { name: /continue with sso/i })).toBeInTheDocument()
    })

    // Redirected to /login, preserving the page the operator was actually
    // on (not wherever /api/me was last checked from).
    expect(window.location.pathname).toBe('/login')
    expect(window.location.search).toContain('redirect=%2Ftapes')

    // The shell — and any dead component-level "unauthorized" error
    // TapesPage might otherwise have rendered in place — is gone.
    expect(screen.queryByRole('navigation', { name: 'Main' })).not.toBeInTheDocument()
    expect(screen.queryByText(/unauthorized/i)).not.toBeInTheDocument()
  })

  it('does not treat a non-401 failure (e.g. a flaky proxy hop) as session loss', async () => {
    const fetchMock = await renderAuthenticated()

    await waitFor(() => {
      expect(screen.getByText('No runs yet')).toBeInTheDocument()
    })

    fetchMock.mockImplementation((input: string) => {
      const url = typeof input === 'string' ? input : String(input)

      if (url.split('?')[0] === '/api/tapes') {
        return Promise.resolve(jsonResponse(503, { error: 'upstream unavailable' }))
      }

      return Promise.resolve(jsonResponse(200, { runs: [] }))
    })

    fireEvent.click(screen.getByRole('link', { name: /tapes/i }))

    // TapesPage.test.tsx covers the component-level error state itself in
    // detail; this only needs to prove the working session was not evicted
    // over it — a real 401 is the only thing this app treats as
    // authoritative session loss (api.ts's apiFetch doc comment).
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /tapes/i })).toBeInTheDocument()
    })
    expect(window.location.pathname).toBe('/tapes')
    expect(screen.queryByRole('button', { name: /continue with sso/i })).not.toBeInTheDocument()
    expect(screen.getByRole('navigation', { name: 'Main' })).toBeInTheDocument()
  })

  it('lands the operator back on the page they were on once they sign back in', async () => {
    // The mid-session redirect above leaves the browser at
    // "/login?redirect=%2Ftapes" — pkg/webauth's OIDC callback (webauth.go)
    // sets a fresh session cookie and 302s the real browser straight back
    // to that redirect path server-side (not through the SPA's /login route
    // at all), so simulate that here as a fresh App mount landing directly
    // on "/tapes" with a valid session again.
    window.history.pushState({}, '', '/tapes')
    stubApi()

    render(<App />)

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /tapes/i })).toBeInTheDocument()
    })
    expect(window.location.pathname).toBe('/tapes')
    expect(screen.queryByRole('button', { name: /continue with sso/i })).not.toBeInTheDocument()
  })
})
