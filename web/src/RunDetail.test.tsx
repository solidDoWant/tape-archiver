import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import RunDetail from './RunDetail'

// FakeEventSource stands in for the browser's real EventSource (not
// implemented by jsdom) so RunDetail can be exercised without a real server:
// it records every constructed instance (RunDetail creates one per mount) so
// a test can reach in and drive its listeners directly, mirroring how a real
// SSE connection would deliver "update"/"done"/"error" events.
class FakeEventSource {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSED = 2
  static instances: FakeEventSource[] = []

  readyState = FakeEventSource.CONNECTING
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

  removeEventListener() {
    // Unused by RunDetail (it relies on the effect cleanup calling close()
    // instead of removing individual listeners), but a real EventSource has
    // this method, so the fake does too for shape-compatibility.
  }

  close() {
    this.closeCalls += 1
    this.readyState = FakeEventSource.CLOSED
  }

  emit(type: 'update' | 'done', body: unknown) {
    this.readyState = FakeEventSource.OPEN
    const event = { data: JSON.stringify(body) } as MessageEvent<string>
    this.listeners[type]?.forEach((listener) => listener(event))
  }

  emitError() {
    this.listeners.error?.forEach((listener) => listener({ data: '' } as MessageEvent<string>))
  }
}

const runningDetail = {
  workflowId: 'backup',
  runId: 'run-abc',
  status: 'Running',
  startTime: '2026-07-09T12:00:00Z',
  lastCompletedPhase: 'Stage',
  currentPause: { kind: '' },
}

const completedDetail = {
  ...runningDetail,
  status: 'Completed',
  closeTime: '2026-07-09T13:00:00Z',
  lastCompletedPhase: 'Verify',
}

const pausedDetail = {
  ...runningDetail,
  lastCompletedPhase: 'Load',
  currentPause: {
    kind: 'write-failure',
    phase: 'Write',
    affectedTapes: ['TA0001L6'],
    reloadSlots: [101],
    errorSummary: 'mkltfs: drive reported a hard write error',
  },
}

// emptyLogsResponse answers LogPanel's GET /api/runs/{runID}/logs (wired
// into RunDetail once `detail` arrives — issue #274) with an empty,
// non-live window, so tests that don't care about the log panel itself
// aren't broken by an unmocked/mismatched fetch call. Tests that stub their
// own fetch for a different purpose (e.g. the resume/abort action below)
// reuse this via logsAwareFetchMock, so LogPanel's own request still gets a
// sane answer instead of colliding with a mock meant for another endpoint.
function emptyLogsResponse() {
  return { ok: true, status: 200, json: async () => ({ runId: 'run-abc', lines: [], live: false }) }
}

// logsAwareFetchMock wraps handleOther (a mock for whatever endpoint a
// specific test cares about) so any request to a /logs URL is answered by
// emptyLogsResponse instead, regardless of call order relative to the
// endpoint under test.
function logsAwareFetchMock(handleOther: (input: RequestInfo | URL, init?: RequestInit) => unknown) {
  return vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    if (typeof input === 'string' && input.includes('/logs')) {
      return Promise.resolve(emptyLogsResponse())
    }

    return handleOther(input, init)
  })
}

beforeEach(() => {
  FakeEventSource.instances = []
  vi.stubGlobal('EventSource', FakeEventSource)
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(emptyLogsResponse()))
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('RunDetail', () => {
  it('shows a connecting state before the first event arrives', () => {
    render(<RunDetail runId="run-abc" />)

    expect(screen.getByRole('status')).toHaveTextContent(/connecting/i)
  })

  it('connects to the SSE endpoint for the given run ID', () => {
    render(<RunDetail runId="run-abc" />)

    expect(FakeEventSource.instances).toHaveLength(1)
    expect(FakeEventSource.instances[0].url).toBe('/api/events/runs/run-abc')
  })

  it('shows live status/phase on an update event', async () => {
    render(<RunDetail runId="run-abc" />)

    FakeEventSource.instances[0].emit('update', runningDetail)

    await waitFor(() => {
      expect(screen.getByText('Running')).toBeInTheDocument()
    })
    expect(screen.getByText('Stage')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('updates in place as further update events arrive', async () => {
    render(<RunDetail runId="run-abc" />)

    const source = FakeEventSource.instances[0]

    source.emit('update', runningDetail)
    await waitFor(() => expect(screen.getByText('Stage')).toBeInTheDocument())

    source.emit('update', { ...runningDetail, lastCompletedPhase: 'Verify' })
    await waitFor(() => expect(screen.getByText('Verify')).toBeInTheDocument())
    expect(screen.queryByText('Stage')).not.toBeInTheDocument()
  })

  it('shows a terminal state and closes the connection on a done event', async () => {
    render(<RunDetail runId="run-abc" />)

    const source = FakeEventSource.instances[0]

    source.emit('update', runningDetail)
    await waitFor(() => expect(screen.getByText('Running')).toBeInTheDocument())

    source.emit('done', completedDetail)

    await waitFor(() => {
      expect(screen.getByText(/run finished/i)).toBeInTheDocument()
    })
    expect(screen.getByText('Completed')).toBeInTheDocument()
    expect(source.closeCalls).toBeGreaterThanOrEqual(1)
  })

  it('shows a connection-lost state on an error event', async () => {
    render(<RunDetail runId="run-abc" />)

    FakeEventSource.instances[0].emitError()

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/connection lost/i)
    })
  })

  it('recovers from an error state once a later update event arrives', async () => {
    render(<RunDetail runId="run-abc" />)

    const source = FakeEventSource.instances[0]

    source.emitError()
    await waitFor(() => expect(screen.getByRole('alert')).toBeInTheDocument())

    source.emit('update', runningDetail)

    await waitFor(() => {
      expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    })
    expect(screen.getByText('Running')).toBeInTheDocument()
  })

  it('does not fall back into an error state after a done event closes the stream', async () => {
    render(<RunDetail runId="run-abc" />)

    const source = FakeEventSource.instances[0]

    source.emit('done', completedDetail)
    await waitFor(() => expect(screen.getByText(/run finished/i)).toBeInTheDocument())

    // The browser dispatching a trailing error once the connection actually
    // closes (following our own close() call) must not overwrite the
    // terminal state.
    source.emitError()

    expect(screen.getByText(/run finished/i)).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('closes the EventSource on unmount', () => {
    const { unmount } = render(<RunDetail runId="run-abc" />)

    const source = FakeEventSource.instances[0]

    unmount()

    expect(source.closeCalls).toBeGreaterThanOrEqual(1)
  })

  it('reconnects to a new run ID when the prop changes', () => {
    const { rerender } = render(<RunDetail runId="run-abc" />)

    expect(FakeEventSource.instances).toHaveLength(1)
    const first = FakeEventSource.instances[0]

    rerender(<RunDetail runId="run-xyz" />)

    expect(FakeEventSource.instances).toHaveLength(2)
    expect(first.closeCalls).toBeGreaterThanOrEqual(1)
    expect(FakeEventSource.instances[1].url).toBe('/api/events/runs/run-xyz')
  })

  it('shows no pause panel while the run is not paused', async () => {
    render(<RunDetail runId="run-abc" />)

    FakeEventSource.instances[0].emit('update', runningDetail)

    await waitFor(() => expect(screen.getByText('Running')).toBeInTheDocument())

    expect(screen.queryByText(/^paused:/i)).not.toBeInTheDocument()
  })

  it('shows the pause panel and lets an operator resume a paused run', async () => {
    const fetchMock = logsAwareFetchMock(() =>
      Promise.resolve({
        ok: true,
        status: 202,
        json: async () => ({ status: 'resume signal sent' }),
      }),
    )
    vi.stubGlobal('fetch', fetchMock)
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))

    render(<RunDetail runId="run-abc" />)

    const source = FakeEventSource.instances[0]

    source.emit('update', pausedDetail)

    await waitFor(() => {
      expect(screen.getByText(/Load\/Write failure/)).toBeInTheDocument()
    })

    expect(screen.getByText(/TA0001L6/)).toBeInTheDocument()

    screen.getByRole('button', { name: /^resume$/i }).click()

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/runs/run-abc/resume',
        expect.objectContaining({ method: 'POST' }),
      )
    })

    // The pause panel itself does not clear on its own — that happens when
    // the SSE stream's next poll observes CurrentPause changed and pushes a
    // fresh update event (pkg/runsapi/events.go), which this test proves
    // separately by driving that event directly:
    source.emit('update', runningDetail)

    await waitFor(() => {
      expect(screen.queryByText(/^paused:/i)).not.toBeInTheDocument()
    })
  })

  it('hides the Abort button for an eject pause', async () => {
    render(<RunDetail runId="run-abc" />)

    FakeEventSource.instances[0].emit('update', {
      ...runningDetail,
      lastCompletedPhase: 'Eject',
      currentPause: { kind: 'eject', affectedTapes: ['TA0001L6'], awaitingExport: 1 },
    })

    await waitFor(() => {
      expect(screen.getByText(/Eject — import\/export station full/)).toBeInTheDocument()
    })

    expect(screen.getByRole('button', { name: /^resume$/i })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^abort$/i })).not.toBeInTheDocument()
  })

  it('wires in a whole-run LogPanel once the first event arrives (issue #274)', async () => {
    const fetchMock = vi.fn().mockResolvedValue(emptyLogsResponse())
    vi.stubGlobal('fetch', fetchMock)

    render(<RunDetail runId="run-abc" />)

    // No phase rail exists yet on this page (issue #277 owns that), so the
    // panel covers the whole run — no ?phase= query parameter.
    expect(fetchMock).not.toHaveBeenCalled()

    FakeEventSource.instances[0].emit('update', runningDetail)

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith('/api/runs/run-abc/logs', undefined)
    })
    expect(screen.getByRole('log')).toBeInTheDocument()
  })
})
