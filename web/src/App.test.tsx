import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
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
// SubmitRunForm/api.ts/RunHistory all read (ok/status/json()).
function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
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

describe('App', () => {
  it('renders the shell heading, nav, and the submit-run form at the root path', () => {
    render(<App />)

    expect(screen.getByRole('link', { name: 'tape-archiver' })).toBeInTheDocument()
    expect(screen.getByRole('navigation', { name: 'Main' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Submit' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'History' })).toBeInTheDocument()
    expect(screen.getByRole('form', { name: /submit backup run/i })).toBeInTheDocument()
  })

  it('navigates to the run detail view via the submit form\'s "View run" link', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(jsonResponse(201, { workflowId: 'backup', runId: 'run-abc-123' }))
    vi.stubGlobal('fetch', fetchMock)

    render(<App />)

    fireEvent.change(screen.getByLabelText('Run config (JSON)'), {
      target: { value: '{"copies":1}' },
    })
    fireEvent.click(screen.getByRole('button', { name: /submit run/i }))

    await waitFor(() => {
      expect(screen.getByRole('button', { name: /view run/i })).toBeInTheDocument()
    })

    fireEvent.click(screen.getByRole('button', { name: /view run/i }))

    expect(screen.getByRole('heading', { name: /run run-abc-123/i })).toBeInTheDocument()
    expect(window.location.pathname).toBe('/runs/run-abc-123')
  })

  it('renders the run detail view directly when the URL already points at a run', () => {
    window.history.pushState({}, '', '/runs/run-xyz')

    render(<App />)

    expect(screen.getByRole('heading', { name: /run run-xyz/i })).toBeInTheDocument()
    expect(screen.queryByRole('form', { name: /submit backup run/i })).not.toBeInTheDocument()
  })

  it('returns to the previous view via the browser back button', async () => {
    render(<App />)

    fireEvent.click(screen.getByRole('link', { name: 'History' }))
    expect(window.location.pathname).toBe('/history')

    window.history.back()

    await waitFor(() => {
      expect(window.location.pathname).toBe('/')
    })
    expect(screen.getByRole('form', { name: /submit backup run/i })).toBeInTheDocument()
  })

  it('shows the run history view, fetched from GET /api/runs, via the nav link', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse(200, {
        runs: [
          {
            workflowId: 'backup',
            runId: 'run-1',
            status: 'Completed',
            startTime: '2026-07-01T00:00:00Z',
            closeTime: '2026-07-01T02:00:00Z',
          },
        ],
      }),
    )
    vi.stubGlobal('fetch', fetchMock)

    render(<App />)

    fireEvent.click(screen.getByRole('link', { name: 'History' }))

    await waitFor(() => {
      expect(screen.getByRole('link', { name: 'run-1' })).toBeInTheDocument()
    })
    expect(fetchMock).toHaveBeenCalledWith('/api/runs', undefined)
    expect(window.location.pathname).toBe('/history')
  })

  it('navigates from a history row straight to that run\'s detail view', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse(200, {
        runs: [
          {
            workflowId: 'backup',
            runId: 'run-1',
            status: 'Completed',
            startTime: '2026-07-01T00:00:00Z',
          },
        ],
      }),
    )
    vi.stubGlobal('fetch', fetchMock)

    render(<App />)

    fireEvent.click(screen.getByRole('link', { name: 'History' }))

    await waitFor(() => {
      expect(screen.getByRole('link', { name: 'run-1' })).toBeInTheDocument()
    })

    fireEvent.click(screen.getByRole('link', { name: 'run-1' }))

    expect(screen.getByRole('heading', { name: /run run-1/i })).toBeInTheDocument()
    expect(window.location.pathname).toBe('/runs/run-1')
  })

  it('shows a not-found view with a way back for an unknown path', () => {
    window.history.pushState({}, '', '/no-such-page')

    render(<App />)

    expect(screen.getByRole('heading', { name: /page not found/i })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('link', { name: /go to the submit form/i }))
    expect(screen.getByRole('form', { name: /submit backup run/i })).toBeInTheDocument()
  })

  it('toggles dark mode via the header control and persists the choice', () => {
    render(<App />)

    expect(document.documentElement.classList.contains('dark')).toBe(false)

    fireEvent.click(screen.getByRole('button', { name: /switch to dark mode/i }))

    expect(document.documentElement.classList.contains('dark')).toBe(true)
    expect(window.localStorage.getItem('tape-archiver:theme')).toBe('dark')

    fireEvent.click(screen.getByRole('button', { name: /switch to light mode/i }))

    expect(document.documentElement.classList.contains('dark')).toBe(false)
    expect(window.localStorage.getItem('tape-archiver:theme')).toBe('light')
  })
})
