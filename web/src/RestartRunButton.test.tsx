import type { ReactElement } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import RestartRunButton from './RestartRunButton'
import { RouterProvider } from './router'

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

// stubRuns makes GET /api/runs (useActiveRun) return the given run list.
function stubRuns(runs: Array<{ status: string }>) {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(jsonResponse(200, { runs }))),
  )
}

function renderWithRouter(ui: ReactElement) {
  return render(<RouterProvider>{ui}</RouterProvider>)
}

afterEach(() => {
  vi.unstubAllGlobals()
  window.history.pushState({}, '', '/')
})

describe('RestartRunButton', () => {
  it('navigates to the config page with ?from=<runId> when nothing is running', async () => {
    stubRuns([{ status: 'Completed' }])

    renderWithRouter(<RestartRunButton runId="run 1" />)

    const button = await screen.findByRole('button', { name: /restart run/i })
    await waitFor(() => expect(button).not.toBeDisabled())

    fireEvent.click(button)

    // submitPath percent-encodes the run ID into the query.
    expect(window.location.pathname + window.location.search).toBe('/submit?from=run%201')
  })

  it('is disabled while a run is in progress, and does not navigate', async () => {
    stubRuns([{ status: 'Running' }])

    renderWithRouter(<RestartRunButton runId="run-1" />)

    const button = await screen.findByRole('button', { name: /restart run/i })
    await waitFor(() => expect(button).toBeDisabled())

    expect(screen.getByText(/a run is already in progress/i)).toBeInTheDocument()

    fireEvent.click(button)
    expect(window.location.pathname).toBe('/')
  })

  it('stays disabled until the active-run check has resolved', () => {
    // A never-resolving fetch keeps useActiveRun in its loading state.
    vi.stubGlobal(
      'fetch',
      vi.fn(() => new Promise(() => {})),
    )

    renderWithRouter(<RestartRunButton runId="run-1" />)

    expect(screen.getByRole('button', { name: /restart run/i })).toBeDisabled()
  })
})
