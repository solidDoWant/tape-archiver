import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen } from '@testing-library/react'
import LoginPage from './LoginPage'

// settle flushes state updates still pending from a component's fetch-on-mount
// (e.g. the footer version fetch) inside act(), so they do not land after the
// test body returns as a "not wrapped in act(...)" warning. Awaiting one async
// act() tick drains the resolved-promise microtask chain those fetches sit on.
async function settle() {
  await act(async () => {})
}

// jsonResponse builds a minimal fetch Response stand-in, matching the shape
// api.ts reads (ok/status/json()) — here only the Footer's
// GET /api/build-info is ever fetched.
function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

// stubLocationAssign replaces window.location with a spy-able stand-in:
// jsdom's real location.assign throws "not implemented" on navigation, and
// the sign-in flow is a real browser navigation to /auth/login this suite
// needs to observe. pathname/search delegate live to the original location
// (which history.pushState keeps updating), so tests can pushState a
// specific query string after this stub is installed.
function stubLocationAssign() {
  const assign = vi.fn()
  const original = window.location

  Object.defineProperty(window, 'location', {
    configurable: true,
    value: {
      get pathname() {
        return original.pathname
      },
      get search() {
        return original.search
      },
      assign,
    },
  })

  return {
    assign,
    restore() {
      Object.defineProperty(window, 'location', { configurable: true, value: original })
    },
  }
}

let location: ReturnType<typeof stubLocationAssign>

beforeEach(() => {
  window.history.pushState({}, '', '/login')
  vi.stubGlobal(
    'fetch',
    vi.fn().mockResolvedValue(jsonResponse(200, { version: 'v-test', footerHost: 'test-host' })),
  )
  location = stubLocationAssign()
})

afterEach(() => {
  location.restore()
  vi.unstubAllGlobals()
  window.history.pushState({}, '', '/')
})

describe('LoginPage', () => {
  it('default state: brand mark, an enabled sign-in control, no heading, no error banner', async () => {
    render(<LoginPage />)

    expect(screen.getByText('tape-archiver')).toBeInTheDocument()

    const button = screen.getByRole('button', { name: /continue with sso/i })
    expect(button).toBeEnabled()
    // Focused on load so Enter/Space signs in without reaching for the mouse.
    expect(button).toHaveFocus()

    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByRole('heading')).not.toBeInTheDocument()
    await settle()
  })

  it('starts the OIDC flow (a real navigation to /auth/login) when the control is activated', async () => {
    window.history.pushState({}, '', '/login?redirect=%2Fruns%2Fabc')

    render(<LoginPage />)

    fireEvent.click(screen.getByRole('button', { name: /continue with sso/i }))

    expect(location.assign).toHaveBeenCalledWith('/auth/login?redirect=%2Fruns%2Fabc')
    await settle()
  })

  it('redirecting state: shows a redirecting heading and disables the control from re-triggering', async () => {
    render(<LoginPage />)

    fireEvent.click(screen.getByRole('button', { name: /continue with sso/i }))

    expect(screen.getByRole('heading', { name: /redirecting/i })).toBeInTheDocument()

    const button = screen.getByRole('button', { name: /redirecting/i })
    expect(button).toBeDisabled()

    fireEvent.click(button)
    expect(location.assign).toHaveBeenCalledTimes(1)
    await settle()
  })

  it('error-denied state: shows the access-denied banner and a way to retry sign-in', async () => {
    window.history.pushState({}, '', '/login?error=denied')

    render(<LoginPage />)

    expect(screen.getByRole('heading', { name: /sign-in required/i })).toBeInTheDocument()

    const banner = screen.getByRole('alert')
    expect(banner).toHaveTextContent(/access denied/i)
    expect(banner).toHaveTextContent(/isn't authorized/i)

    const retry = screen.getByRole('button', { name: /^sign in$/i })
    fireEvent.click(retry)
    expect(location.assign).toHaveBeenCalledWith('/auth/login?redirect=%2F')
    await settle()
  })

  it('error-expired state: shows the session-expired banner and a way to retry sign-in', async () => {
    window.history.pushState({}, '', '/login?error=expired&redirect=%2Ftapes')

    render(<LoginPage />)

    const banner = screen.getByRole('alert')
    expect(banner).toHaveTextContent(/session expired/i)

    fireEvent.click(screen.getByRole('button', { name: /^sign in$/i }))
    expect(location.assign).toHaveBeenCalledWith('/auth/login?redirect=%2Ftapes')
    await settle()
  })

  it('offers "Try a different account" on error states, triggering the same sign-in flow', async () => {
    window.history.pushState({}, '', '/login?error=denied')

    render(<LoginPage />)

    fireEvent.click(screen.getByRole('button', { name: /try a different account/i }))
    expect(location.assign).toHaveBeenCalledWith('/auth/login?redirect=%2F')
    await settle()
  })

  it('sanitizes a malicious redirect parameter instead of passing it to the auth flow', async () => {
    window.history.pushState({}, '', '/login?redirect=' + encodeURIComponent('//evil.example'))

    render(<LoginPage />)

    fireEvent.click(screen.getByRole('button', { name: /continue with sso/i }))

    expect(location.assign).toHaveBeenCalledWith('/auth/login?redirect=%2F')
    await settle()
  })

  it('renders the footer version line, including the configured host label', async () => {
    render(<LoginPage />)

    expect(await screen.findByText(/tape-archiver v-test · test-host/)).toBeInTheDocument()
  })

  it('hides the footer host segment entirely when it is not configured', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(200, { version: 'v-test' })))

    render(<LoginPage />)

    const footer = await screen.findByText(/tape-archiver v-test/)
    expect(footer.textContent).toBe('tape-archiver v-test')
  })
})
