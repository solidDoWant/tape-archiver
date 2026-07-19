import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import Dashboard from './Dashboard'
import { RouterProvider } from './router'

// FakeEventSource stands in for the browser's real EventSource (not
// implemented by jsdom), same minimal shape RunDetail.test.tsx's own fake
// uses — Dashboard's CurrentRunCard/RunsTable share the same SSE
// subscription (runEvents.ts) that page already exercises in detail.
class FakeEventSource {
  static instances: FakeEventSource[] = []
  private listeners: Record<string, ((event: MessageEvent<string>) => void)[]> = {}
  url: string

  constructor(url: string) {
    this.url = url
    FakeEventSource.instances.push(this)
  }

  addEventListener(type: string, listener: (event: MessageEvent<string>) => void) {
    ;(this.listeners[type] ??= []).push(listener)
  }

  close() {
    // no-op
  }

  emit(type: 'update' | 'done', body: unknown) {
    const event = { data: JSON.stringify(body) } as MessageEvent<string>
    // Dispatch inside act() so the state updates the listeners trigger flush
    // the way the browser would, without "not wrapped in act(...)" warnings.
    act(() => {
      this.listeners[type]?.forEach((listener) => listener(event))
    })
  }
}

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

function stubApi(routes: Record<string, { status: number; body: unknown }>) {
  const defaults: Record<string, { status: number; body: unknown }> = {
    '/api/tapes': { status: 200, body: { tapes: [] } },
    // The hardware & environment card sources deploy config from here (#318),
    // independent of any run — give every Dashboard test a real config to load.
    '/api/config/ui': {
      status: 200,
      body: {
        temporalUiBaseUrl: '',
        temporalNamespace: '',
        library: { changer: '/dev/sch0', drives: [], slotCount: 0, cleaningSlots: [], ioStationSlots: [] },
        delivery: { webhookConfigured: false, opticalBurnDrives: [] },
      },
    },
    ...routes,
  }

  vi.stubGlobal(
    'fetch',
    vi.fn((input: string) => {
      const url = typeof input === 'string' ? input : String(input)
      const route = defaults[url.split('?')[0]]

      if (!route) {
        return Promise.resolve(jsonResponse(404, { error: `no stub for ${url}` }))
      }

      return Promise.resolve(jsonResponse(route.status, route.body))
    }),
  )
}

beforeEach(() => {
  window.history.pushState({}, '', '/')
  FakeEventSource.instances = []
  vi.stubGlobal('EventSource', FakeEventSource)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

function renderDashboard(routes: Record<string, { status: number; body: unknown }>) {
  const onStartRun = vi.fn()
  stubApi(routes)

  render(
    <RouterProvider>
      <Dashboard onStartRun={onStartRun} />
    </RouterProvider>,
  )

  return { onStartRun }
}

describe('Dashboard', () => {
  it('shows the first-run empty state and an empty runs table when nothing has ever run', async () => {
    const { onStartRun } = renderDashboard({ '/api/runs': { status: 200, body: { runs: [] } } })

    // The current-run card's own first-run empty state, and the runs
    // table's own separate empty state — both derive from the same "no
    // runs" data, but are two distinct pieces of copy in two distinct
    // cards.
    await waitFor(() => {
      expect(screen.getAllByText(/no runs yet/i)).toHaveLength(2)
    })

    // The hardware & environment card renders deploy config sourced from
    // /api/config/ui even though no run has ever been submitted (#318) — every
    // card degrades (or here, sources) independently.
    await waitFor(() => {
      expect(screen.getByText('/dev/sch0')).toBeInTheDocument()
    })

    fireEvent.click(screen.getAllByRole('button', { name: /start a run/i })[0])
    expect(onStartRun).toHaveBeenCalled()
  })

  it('shows an error state on the current-run card and runs table when GET /api/runs fails', async () => {
    renderDashboard({ '/api/runs': { status: 500, body: { error: 'temporal unreachable' } } })

    await waitFor(() => {
      expect(screen.getAllByRole('alert').some((el) => el.textContent === 'temporal unreachable')).toBe(true)
    })
  })

  it('shows the active run live via SSE, and the same live phase in its runs-table row', async () => {
    renderDashboard({
      '/api/runs': {
        status: 200,
        body: {
          runs: [{ workflowId: 'backup', runId: 'run-live', status: 'Running', startTime: '2026-07-01T00:00:00Z' }],
        },
      },
    })

    await waitFor(() => {
      expect(FakeEventSource.instances).toHaveLength(1)
    })
    expect(FakeEventSource.instances[0].url).toBe('/api/events/runs/run-live')

    FakeEventSource.instances[0].emit('update', {
      workflowId: 'backup',
      runId: 'run-live',
      status: 'Running',
      startTime: '2026-07-01T00:00:00Z',
      lastCompletedPhase: 'Write',
      currentPause: { kind: '' },
    })

    await waitFor(() => {
      expect(screen.getByText('Phase 7 of 11')).toBeInTheDocument()
    })

    // The runs table's row for this same run shows the live phase too,
    // reusing the one SSE subscription rather than a second per-row fetch.
    const row = screen.getByRole('link', { name: 'run-live' })
    expect(row).toHaveTextContent('Write')

    // The hardware/environment card sources deploy config from /api/config/ui,
    // not this run's own submitted config (#318).
    await waitFor(() => {
      expect(screen.getByText('/dev/sch0')).toBeInTheDocument()
    })
  })
})
