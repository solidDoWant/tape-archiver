import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import RunDetail from './RunDetail'
import { RouterProvider } from './router'

// FakeEventSource stands in for the browser's real EventSource (not
// implemented by jsdom): it records every constructed instance so a test can
// reach in and drive its listeners directly, mirroring how a real SSE
// connection would deliver "update"/"done"/"error" events.
class FakeEventSource {
  static instances: FakeEventSource[] = []

  closeCalls = 0
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
    this.closeCalls += 1
  }

  emit(type: 'update' | 'done', body: unknown) {
    const event = { data: JSON.stringify(body) } as MessageEvent<string>
    // Dispatch inside act() so the React state updates the listeners trigger
    // are flushed the way the browser would, without "not wrapped in act(...)"
    // warnings. The listeners are synchronous, so the returned thenable
    // resolves immediately and needs no await.
    act(() => {
      this.listeners[type]?.forEach((listener) => listener(event))
    })
  }

  emitError() {
    act(() => {
      this.listeners.error?.forEach((listener) => listener({ data: '' } as MessageEvent<string>))
    })
  }
}

const phaseOrder = ['Resolve', 'Prepare', 'Pack', 'Generate PAR2', 'Verify', 'Load', 'Write', 'Eject', 'Report', 'Burn', 'Deliver']

function pendingPhases() {
  return phaseOrder.map((name) => ({ name, status: 'pending', facts: [] }))
}

function runningPhases() {
  return phaseOrder.map((name, index) => {
    if (index < 3) {
      return { name, status: 'completed', startTime: '2026-07-09T12:00:00Z', endTime: '2026-07-09T12:05:00Z', facts: [] }
    }

    if (index === 3) {
      return {
        name,
        status: 'active',
        startTime: '2026-07-09T12:05:00Z',
        facts: [{ key: 'recoverySets', label: 'Recovery sets', value: '71' }],
      }
    }

    return { name, status: 'pending', facts: [] }
  })
}

function writePhases(status: 'active' | 'completed' = 'active') {
  return phaseOrder.map((name, index) => {
    if (index < 6) {
      return { name, status: 'completed', startTime: '2026-07-09T12:00:00Z', endTime: '2026-07-09T12:05:00Z', facts: [] }
    }

    if (index === 6) {
      return { name, status, startTime: '2026-07-09T12:05:00Z', facts: [] }
    }

    return { name, status: 'pending', facts: [] }
  })
}

const runningDetail = {
  workflowId: 'backup',
  runId: 'run-abc',
  status: 'Running',
  startTime: '2026-07-09T12:00:00Z',
  lastCompletedPhase: 'Verify',
  currentPause: { kind: '' },
}

const minimalConfig = {
  sources: [{ zfsPath: { name: 'bulk-pool-01/photos' }, compression: true }],
  copies: 2,
  redundancy: { targetPercentage: 10 },
}

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

// stub builds a fetch mock that answers every endpoint the redesigned run
// detail page can call, with sane empty/pending defaults, and per-test
// overrides keyed by the exact request path (query string stripped).
function stub(overrides: Record<string, { status: number; body: unknown }> = {}) {
  const fetchMock = vi.fn((input: RequestInfo | URL) => {
    const url = typeof input === 'string' ? input : String(input)
    const path = url.split('?')[0]

    if (overrides[path]) {
      return Promise.resolve(jsonResponse(overrides[path].status, overrides[path].body))
    }

    if (path.endsWith('/logs')) {
      return Promise.resolve(jsonResponse(200, { runId: 'run-abc', lines: [], live: false }))
    }
    if (path.endsWith('/metrics/drives')) {
      return Promise.resolve(jsonResponse(503, { error: 'unavailable' }))
    }
    if (/\/metrics\/drives\/[^/]+\/history$/.test(path)) {
      return Promise.resolve(jsonResponse(503, { error: 'unavailable' }))
    }
    if (path.endsWith('/phases')) {
      return Promise.resolve(jsonResponse(200, { runId: 'run-abc', phases: pendingPhases() }))
    }
    if (path.endsWith('/config')) {
      return Promise.resolve(jsonResponse(200, { runId: 'run-abc', config: minimalConfig }))
    }
    if (path.endsWith('/tapes')) {
      return Promise.resolve(jsonResponse(200, { runId: 'run-abc', tapes: [] }))
    }
    if (/^\/api\/runs\/[^/]+$/.test(path)) {
      return Promise.resolve(jsonResponse(200, runningDetail))
    }

    return Promise.resolve(jsonResponse(404, { error: `no stub for ${url}` }))
  })

  vi.stubGlobal('fetch', fetchMock)

  return fetchMock
}

beforeEach(() => {
  FakeEventSource.instances = []
  vi.stubGlobal('EventSource', FakeEventSource)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

async function renderReady(overrides: Record<string, { status: number; body: unknown }> = {}) {
  const fetchMock = stub(overrides)

  render(<RunDetail runId="run-abc" />)

  await waitFor(() => {
    expect(screen.getByRole('button', { name: /run overview/i })).toBeInTheDocument()
  })

  return fetchMock
}

describe('RunDetail', () => {
  it('does not render its own run-name title bar (the app shell header shows it — no double header)', async () => {
    render(<RunDetail runId="run-abc" />)

    // RunDetail no longer duplicates the shell's page-title header; the shell
    // (App.tsx) is the single "Run {runId}" header. App.test.tsx covers that the
    // shell shows the run name for a run route.
    expect(screen.queryByRole('heading', { name: /run run-abc/i })).not.toBeInTheDocument()

    // This assertion is synchronous, but RunDetail still fetches its config /
    // phases / detail on mount; flush those inside act() so their settle does
    // not land after the test as a "not wrapped in act(...)" warning.
    await act(async () => {})
  })

  it('renders the phase rail with all 11 phases, in pipeline order, once loaded', async () => {
    await renderReady({ '/api/runs/run-abc/phases': { status: 200, body: { runId: 'run-abc', phases: runningPhases() } } })

    const nav = screen.getByRole('navigation', { name: /run phases/i })
    const buttons = within(nav).getAllByRole('button')
    // "Run overview" + 11 phases.
    expect(buttons).toHaveLength(12)
    expect(buttons[1]).toHaveTextContent('Resolve')
    expect(buttons[4]).toHaveTextContent('PAR2') // "Generate PAR2" displays as "PAR2".
    expect(buttons[7]).toHaveTextContent('Write')
    expect(buttons[12 - 1]).toHaveTextContent('Deliver')
  })

  it('defaults to the run overview view', async () => {
    await renderReady()

    expect(screen.getByRole('heading', { name: /backup in progress/i })).toBeInTheDocument()
  })

  it('shows a non-active phase’s facts and logs when selected from the rail (AC2)', async () => {
    await renderReady({ '/api/runs/run-abc/phases': { status: 200, body: { runId: 'run-abc', phases: runningPhases() } } })

    // Pack (index 2, completed) is not the active phase (PAR2, index 3).
    fireEvent.click(screen.getByRole('button', { name: /^pack/i }))

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: 'Pack' })).toBeInTheDocument()
    })
    expect(screen.getByRole('log')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: /^par2/i }))

    await waitFor(() => {
      expect(screen.getByText('Recovery sets')).toBeInTheDocument()
      expect(screen.getByText('71')).toBeInTheDocument()
    })
  })

  it('fetches the PAR2 phase’s logs by its stable name "Generate PAR2", not its "PAR2" display label', async () => {
    const fetchMock = await renderReady({
      '/api/runs/run-abc/phases': { status: 200, body: { runId: 'run-abc', phases: runningPhases() } },
    })

    fireEvent.click(screen.getByRole('button', { name: /^par2/i }))

    // The rail's button says "PAR2" (display label), but the log window must
    // be scoped by the phase's stable workflow name — VictoriaLogs records
    // carry "Generate PAR2" (the Go constant's value), never the label.
    // URLSearchParams encodes the space as "+".
    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith('/api/runs/run-abc/logs?phase=Generate+PAR2', undefined)
    })
  })

  it('refreshes the phase rail’s statuses when an SSE update event arrives', async () => {
    await renderReady() // default stub: all 11 phases pending.

    expect(screen.getByRole('button', { name: /^resolve/i })).toHaveAttribute('data-status', 'pending')

    // The workflow has progressed: the next /phases fetch reports it. Swap
    // the stub before driving the SSE event that triggers the refetch.
    stub({ '/api/runs/run-abc/phases': { status: 200, body: { runId: 'run-abc', phases: runningPhases() } } })

    FakeEventSource.instances[0].emit('update', { ...runningDetail, lastCompletedPhase: 'Pack' })

    await waitFor(() => {
      expect(screen.getByRole('button', { name: /^resolve/i })).toHaveAttribute('data-status', 'completed')
    })
    expect(screen.getByRole('button', { name: /^par2/i })).toHaveAttribute('data-status', 'active')
  })

  it('keeps the already-loaded phase rail when an SSE-triggered refetch fails', async () => {
    await renderReady({
      '/api/runs/run-abc/phases': { status: 200, body: { runId: 'run-abc', phases: runningPhases() } },
    })

    // The refetch the next SSE event triggers fails outright (e.g. a
    // transient 500 or cmd/web blip)...
    const failingFetch = vi.fn((input: RequestInfo | URL) => {
      const path = (typeof input === 'string' ? input : String(input)).split('?')[0]

      if (path.endsWith('/phases')) {
        return Promise.resolve(jsonResponse(500, { error: 'boom' }))
      }

      return Promise.resolve(jsonResponse(200, { runId: 'run-abc', lines: [], live: false }))
    })
    vi.stubGlobal('fetch', failingFetch)

    FakeEventSource.instances[0].emit('update', runningDetail)

    await waitFor(() => {
      expect(failingFetch).toHaveBeenCalledWith('/api/runs/run-abc/phases', undefined)
    })

    // ...but the rail the page already showed is kept, not clobbered into
    // the degraded fallback over one bad refetch.
    expect(screen.getByRole('navigation', { name: /run phases/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^resolve/i })).toHaveAttribute('data-status', 'completed')
    expect(screen.queryByText(/unavailable/i)).not.toBeInTheDocument()
  })

  it('shows a pending placeholder for a phase that has not started', async () => {
    await renderReady({ '/api/runs/run-abc/phases': { status: 200, body: { runId: 'run-abc', phases: runningPhases() } } })

    fireEvent.click(screen.getByRole('button', { name: /^deliver/i }))

    await waitFor(() => {
      expect(screen.getByText(/not started/i)).toBeInTheDocument()
    })
  })

  it('embeds live drive metrics in the Write phase view (AC3)', async () => {
    await renderReady({
      '/api/runs/run-abc/phases': { status: 200, body: { runId: 'run-abc', phases: writePhases('active') } },
      '/api/runs/run-abc/metrics/drives': {
        status: 200,
        body: { runId: 'run-abc', drives: [{ barcode: 'TA0001L6', driveIndex: 0, hasData: true, throughputMBps: 140, floorKnown: true, floorMBps: 50 }] },
      },
    })

    fireEvent.click(screen.getByRole('button', { name: /^write/i }))

    await waitFor(() => {
      expect(screen.getByText(/140 MB\/s/)).toBeInTheDocument()
    })
  })

  it('shows a write-failure pause narrative with reload slots and rejects nothing (abort allowed)', async () => {
    const fetchMock = await renderReady()

    FakeEventSource.instances[0].emit('update', {
      ...runningDetail,
      currentPause: {
        kind: 'write-failure',
        phase: 'Write',
        affectedTapes: ['TA0001L6'],
        reloadSlots: [101],
        errorSummary: 'mkltfs: drive reported a hard write error',
        canAbort: true,
      },
    })

    await waitFor(() => {
      expect(screen.getByText(/Load\/Write failure/)).toBeInTheDocument()
    })
    expect(screen.getByText(/101/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^abort$/i })).toBeInTheDocument()

    void fetchMock
  })

  it('shows an eject pause narrative and rejects abort (button hidden)', async () => {
    await renderReady()

    FakeEventSource.instances[0].emit('update', {
      ...runningDetail,
      currentPause: { kind: 'eject', affectedTapes: ['TA0001L6'], awaitingExport: 1, canAbort: false },
    })

    await waitFor(() => {
      expect(screen.getByText(/Eject — import\/export station full/)).toBeInTheDocument()
    })
    expect(screen.queryByRole('button', { name: /^abort$/i })).not.toBeInTheDocument()
  })

  it('shows a burn pause narrative naming the affected devices', async () => {
    await renderReady()

    FakeEventSource.instances[0].emit('update', {
      ...runningDetail,
      currentPause: { kind: 'burn', devices: ['/dev/sr0'], canAbort: true },
    })

    await waitFor(() => {
      expect(screen.getByText(/Burn phase/)).toBeInTheDocument()
    })
    expect(screen.getByText(/\/dev\/sr0/)).toBeInTheDocument()
  })

  it('switches from live metrics to this run’s final recorded write-health once the run closes (terminal vs live)', async () => {
    const fetchMock = await renderReady({
      '/api/runs/run-abc/phases': { status: 200, body: { runId: 'run-abc', phases: writePhases('completed') } },
    })

    fireEvent.click(screen.getByRole('button', { name: /^write/i }))

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith('/api/runs/run-abc/metrics/drives', undefined)
    })

    const metricsCallsBeforeTerminal = fetchMock.mock.calls.filter((call) => call[0] === '/api/runs/run-abc/metrics/drives').length
    const tapesCallsBeforeTerminal = fetchMock.mock.calls.filter((call) => call[0] === '/api/runs/run-abc/tapes').length

    FakeEventSource.instances[0].emit('done', { ...runningDetail, status: 'Completed', closeTime: '2026-07-09T13:00:00Z' })

    await waitFor(() => {
      expect(screen.getByText(/READ-ONLY/)).toBeInTheDocument()
    })

    // Once terminal: DriveMetricsPanel's terminal=true half (FinalDriveMetrics)
    // fetches this run's own recorded /tapes outcomes instead of continuing
    // to poll VictoriaMetrics — no further /metrics/drives calls, but at
    // least one new /tapes call (both TapesSection's and DriveMetricsPanel's
    // own terminal-triggered refetch land on the same endpoint).
    await waitFor(() => {
      const tapesCallsAfterTerminal = fetchMock.mock.calls.filter((call) => call[0] === '/api/runs/run-abc/tapes').length
      expect(tapesCallsAfterTerminal).toBeGreaterThan(tapesCallsBeforeTerminal)
    })
    const metricsCallsAfterTerminal = fetchMock.mock.calls.filter((call) => call[0] === '/api/runs/run-abc/metrics/drives').length
    expect(metricsCallsAfterTerminal).toBe(metricsCallsBeforeTerminal)
  })

  it('falls back to an aged-out state, distinct from not-found, when the phases endpoint reports 410', async () => {
    stub({ '/api/runs/run-abc/phases': { status: 410, body: { error: 'gone' } } })

    render(
      <RouterProvider>
        <RunDetail runId="run-abc" />
      </RouterProvider>,
    )

    await waitFor(() => {
      expect(screen.getByText(/aged out of history/i)).toBeInTheDocument()
    })
    expect(screen.queryByText(/no run named/i)).not.toBeInTheDocument()
  })

  it('shows a distinct not-found state when the run itself does not exist', async () => {
    stub({ '/api/runs/run-abc': { status: 404, body: { error: 'not found' } } })

    render(
      <RouterProvider>
        <RunDetail runId="run-abc" />
      </RouterProvider>,
    )

    await waitFor(() => {
      expect(screen.getByText(/no run named run-abc/i)).toBeInTheDocument()
    })
    expect(screen.queryByText(/aged out of history/i)).not.toBeInTheDocument()
  })

  it('degrades honestly to the basic status view when the phases endpoint 404s despite the run existing', async () => {
    stub({ '/api/runs/run-abc/phases': { status: 404, body: { error: 'not found' } } })

    render(<RunDetail runId="run-abc" />)

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/unavailable/i)
    })
    expect(screen.getByText('Running')).toBeInTheDocument()
    expect(screen.queryByRole('navigation', { name: /run phases/i })).not.toBeInTheDocument()
  })

  it('reports the last completed phase as temporarily unavailable in the degraded view when the phase query failed', async () => {
    // Issue #323: in the degraded fallback (phases endpoint down), the
    // last-completed-phase row is sourced from the run detail's own query-backed
    // field. When that query failed (lastCompletedPhaseUnknown), the row must say
    // "Temporarily unavailable", not the "—" of a genuinely-not-started run.
    stub({
      '/api/runs/run-abc/phases': { status: 404, body: { error: 'not found' } },
      '/api/runs/run-abc': {
        status: 200,
        body: { ...runningDetail, lastCompletedPhase: '', lastCompletedPhaseUnknown: true },
      },
    })

    render(<RunDetail runId="run-abc" />)

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/unavailable/i)
    })
    expect(screen.getByText('Temporarily unavailable')).toBeInTheDocument()
  })
})
