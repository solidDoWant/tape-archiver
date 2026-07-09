import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import RunHistory from './RunHistory'
import { RouterProvider } from './router'

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

function renderHistory() {
  return render(
    <RouterProvider>
      <RunHistory />
    </RouterProvider>,
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('RunHistory', () => {
  it('shows a loading state before the list arrives', () => {
    vi.stubGlobal('fetch', vi.fn().mockReturnValue(new Promise(() => {})))

    renderHistory()

    expect(screen.getByRole('status')).toHaveTextContent(/loading run history/i)
  })

  it('shows an empty state when there are no runs yet', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(200, { runs: [] })))

    renderHistory()

    await waitFor(() => {
      expect(screen.getByText(/no runs yet/i)).toBeInTheDocument()
    })
  })

  it('lists runs with status, timing, and a link to each run\'s detail page', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          runs: [
            {
              workflowId: 'backup',
              runId: 'run-1',
              status: 'Completed',
              startTime: '2026-07-01T00:00:00Z',
              closeTime: '2026-07-01T02:00:00Z',
            },
            {
              workflowId: 'backup',
              runId: 'run-2',
              status: 'Failed',
              startTime: '2026-06-01T00:00:00Z',
            },
          ],
        }),
      ),
    )

    renderHistory()

    await waitFor(() => {
      expect(screen.getByRole('link', { name: 'run-1' })).toBeInTheDocument()
    })

    expect(screen.getByRole('link', { name: 'run-1' })).toHaveAttribute(
      'href',
      '/runs/run-1',
    )
    expect(screen.getByText('Completed')).toBeInTheDocument()
    expect(screen.getByText('Failed')).toBeInTheDocument()
  })

  it('enriches a currently-running row with its live last-completed-phase', async () => {
    const fetchMock = vi.fn((input: string) => {
      if (input === '/api/runs') {
        return Promise.resolve(
          jsonResponse(200, {
            runs: [
              {
                workflowId: 'backup',
                runId: 'run-live',
                status: 'Running',
                startTime: '2026-07-09T00:00:00Z',
              },
            ],
          }),
        )
      }

      if (input === '/api/runs/run-live') {
        return Promise.resolve(
          jsonResponse(200, {
            workflowId: 'backup',
            runId: 'run-live',
            status: 'Running',
            startTime: '2026-07-09T00:00:00Z',
            lastCompletedPhase: 'Stage',
          }),
        )
      }

      throw new Error(`unexpected fetch: ${input}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    renderHistory()

    await waitFor(() => {
      expect(screen.getByText('Stage')).toBeInTheDocument()
    })
    expect(fetchMock).toHaveBeenCalledWith('/api/runs/run-live', undefined)
  })

  it('does not fetch per-row detail for closed runs, showing phase as unavailable', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse(200, {
        runs: [
          {
            workflowId: 'backup',
            runId: 'run-closed',
            status: 'Completed',
            startTime: '2026-07-01T00:00:00Z',
            closeTime: '2026-07-01T02:00:00Z',
          },
        ],
      }),
    )
    vi.stubGlobal('fetch', fetchMock)

    renderHistory()

    await waitFor(() => {
      expect(screen.getByRole('link', { name: 'run-closed' })).toBeInTheDocument()
    })

    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock).toHaveBeenCalledWith('/api/runs', undefined)
  })

  it('shows an error state when the list fetch fails', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(jsonResponse(500, { error: 'Temporal is unreachable.' })),
    )

    renderHistory()

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent('Temporal is unreachable.')
    })
  })
})
