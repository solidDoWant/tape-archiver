import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { Link, RouterProvider } from './router'
import { parseRoute, runPath, sanitizeRedirectPath, useNavigate, useRoute } from './route'

beforeEach(() => {
  window.history.pushState({}, '', '/')
})

afterEach(() => {
  window.history.pushState({}, '', '/')
})

describe('parseRoute', () => {
  it('maps "/" to the submit route', () => {
    expect(parseRoute('/')).toEqual({ name: 'submit' })
  })

  it('maps "/history" (with or without a trailing slash) to the history route', () => {
    expect(parseRoute('/history')).toEqual({ name: 'history' })
    expect(parseRoute('/history/')).toEqual({ name: 'history' })
  })

  it('maps "/runs/{id}" to the run route, decoding the ID', () => {
    expect(parseRoute('/runs/run-abc')).toEqual({ name: 'run', runId: 'run-abc' })
    expect(parseRoute('/runs/run%20abc')).toEqual({ name: 'run', runId: 'run abc' })
  })

  it('maps "/tapes" (with or without a trailing slash) to the tapes route', () => {
    expect(parseRoute('/tapes')).toEqual({ name: 'tapes' })
    expect(parseRoute('/tapes/')).toEqual({ name: 'tapes' })
  })

  it('maps "/login" to the login route, ignoring any query string', () => {
    expect(parseRoute('/login')).toEqual({ name: 'login' })
    expect(parseRoute('/login?error=denied&redirect=%2Fruns%2Fabc')).toEqual({ name: 'login' })
  })

  it('ignores a query string when matching any route', () => {
    expect(parseRoute('/tapes?whatever=1')).toEqual({ name: 'tapes' })
    expect(parseRoute('/runs/run-abc?x=1')).toEqual({ name: 'run', runId: 'run-abc' })
  })

  it('maps anything else to not-found, carrying the original path', () => {
    expect(parseRoute('/nope')).toEqual({ name: 'not-found', path: '/nope' })
  })

  it('falls back to not-found instead of throwing on a malformed percent-encoded run ID', () => {
    // decodeURIComponent('%E0%A4%A') throws (an incomplete UTF-8 sequence);
    // this must not propagate out of parseRoute, since there is no
    // ErrorBoundary above the router to catch it.
    expect(parseRoute('/runs/%E0%A4%A')).toEqual({ name: 'not-found', path: '/runs/%E0%A4%A' })
  })
})

describe('runPath', () => {
  it('builds and encodes a run detail URL', () => {
    expect(runPath('run abc')).toBe('/runs/run%20abc')
  })
})

// The client-side mirror of pkg/webauth's identically-named server-side
// defense — same cases as its Go test (webauth_test.go's
// TestSanitizeRedirectPath), so the two implementations cannot silently
// diverge on what they reject.
describe('sanitizeRedirectPath', () => {
  it('defaults empty/missing values to root', () => {
    expect(sanitizeRedirectPath('')).toBe('/')
    expect(sanitizeRedirectPath(null)).toBe('/')
    expect(sanitizeRedirectPath(undefined)).toBe('/')
  })

  it('rejects a path without a leading slash', () => {
    expect(sanitizeRedirectPath('runs/abc')).toBe('/')
  })

  it('preserves a normal same-origin path', () => {
    expect(sanitizeRedirectPath('/runs/abc')).toBe('/runs/abc')
  })

  it('rejects a protocol-relative // URL', () => {
    expect(sanitizeRedirectPath('//evil.example')).toBe('/')
  })

  it('rejects a backslash anywhere in the path', () => {
    expect(sanitizeRedirectPath('/\\evil.example')).toBe('/')
    expect(sanitizeRedirectPath('/runs/\\evil.example')).toBe('/')
  })
})

// RouteProbe renders the current route name/runId as text and a couple of
// controls to drive navigation, so tests can assert on router behavior
// through the public hooks/components rather than reaching into internals.
function RouteProbe() {
  const route = useRoute()
  const navigate = useNavigate()

  return (
    <div>
      <p data-testid="route">{route.name === 'run' ? `run:${route.runId}` : route.name}</p>
      <Link to="/history">Go to history</Link>
      <button type="button" onClick={() => navigate('/runs/run-1')}>
        Go to run-1 via navigate()
      </button>
    </div>
  )
}

describe('RouterProvider', () => {
  it('seeds the initial route from window.location', () => {
    window.history.pushState({}, '', '/history')

    render(
      <RouterProvider>
        <RouteProbe />
      </RouterProvider>,
    )

    expect(screen.getByTestId('route')).toHaveTextContent('history')
  })

  it('Link navigates client-side (pushState, no full reload) on a plain click', () => {
    render(
      <RouterProvider>
        <RouteProbe />
      </RouterProvider>,
    )

    fireEvent.click(screen.getByRole('link', { name: 'Go to history' }))

    expect(window.location.pathname).toBe('/history')
    expect(screen.getByTestId('route')).toHaveTextContent('history')
  })

  it('does not intercept a modified click (e.g. ctrl-click to open in a new tab)', () => {
    render(
      <RouterProvider>
        <RouteProbe />
      </RouterProvider>,
    )

    fireEvent.click(screen.getByRole('link', { name: 'Go to history' }), { ctrlKey: true })

    // Navigation did not happen client-side; the browser's default action
    // (which jsdom does not actually follow) was left alone.
    expect(window.location.pathname).toBe('/')
    expect(screen.getByTestId('route')).toHaveTextContent('submit')
  })

  it('useNavigate() pushes a new history entry and updates the route', () => {
    render(
      <RouterProvider>
        <RouteProbe />
      </RouterProvider>,
    )

    fireEvent.click(screen.getByRole('button', { name: /go to run-1/i }))

    expect(window.location.pathname).toBe('/runs/run-1')
    expect(screen.getByTestId('route')).toHaveTextContent('run:run-1')
  })

  it('reacts to the browser back/forward buttons via popstate', async () => {
    render(
      <RouterProvider>
        <RouteProbe />
      </RouterProvider>,
    )

    fireEvent.click(screen.getByRole('link', { name: 'Go to history' }))
    expect(screen.getByTestId('route')).toHaveTextContent('history')

    window.history.back()

    await waitFor(() => {
      expect(screen.getByTestId('route')).toHaveTextContent('submit')
    })

    window.history.forward()

    await waitFor(() => {
      expect(screen.getByTestId('route')).toHaveTextContent('history')
    })
  })
})
