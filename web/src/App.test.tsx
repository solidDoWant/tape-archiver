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

beforeEach(() => {
  vi.stubGlobal('EventSource', MinimalEventSource)
  window.history.pushState({}, '', '/')
})

afterEach(() => {
  vi.unstubAllGlobals()
  window.history.pushState({}, '', '/')
})

describe('App', () => {
  it('renders the shell heading and the submit-run form at the root path', () => {
    render(<App />)

    expect(
      screen.getByRole('heading', { name: 'tape-archiver' }),
    ).toBeInTheDocument()
    expect(screen.getByRole('form', { name: /submit backup run/i })).toBeInTheDocument()
  })

  it('navigates to the run detail view via the submit form\'s "View run" link', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 201,
      json: async () => ({ workflowId: 'backup', runId: 'run-abc-123' }),
    })
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

  it('navigates back to the submit form via the run detail view\'s back link', () => {
    window.history.pushState({}, '', '/runs/run-xyz')

    render(<App />)

    fireEvent.click(screen.getByRole('button', { name: /back to submit a run/i }))

    expect(screen.getByRole('form', { name: /submit backup run/i })).toBeInTheDocument()
    expect(window.location.pathname).toBe('/')
  })
})
