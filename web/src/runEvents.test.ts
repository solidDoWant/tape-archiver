import { act, renderHook, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { onSessionExpired } from './api'
import { useRunEvents } from './runEvents'

// FakeEventSource stands in for the browser's real EventSource (not
// implemented by jsdom) — same minimal shape RunDetail.test.tsx's own fake
// uses, duplicated here rather than shared because it is test scaffolding,
// not exportable product code.
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

  emitError() {
    this.listeners.error?.forEach((listener) => listener({ data: '' } as MessageEvent<string>))
  }

  emit(type: string, data: string) {
    this.listeners[type]?.forEach((listener) => listener({ data } as MessageEvent<string>))
  }
}

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

beforeEach(() => {
  FakeEventSource.instances = []
  vi.stubGlobal('EventSource', FakeEventSource)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('useRunEvents SSE error handling (issue #285)', () => {
  // EventSource cannot see HTTP status codes on a dropped connection, so a
  // real session loss and a transient network blip look identical at the
  // 'error' event alone — useRunEvents disambiguates with a follow-up
  // apiFetch('/api/me') probe; a 401 on that probe is what actually
  // triggers session-loss handling (via apiFetch's own onSessionExpired
  // notification — api.test.ts covers that wiring directly).
  it('probes /api/me on a dropped connection and notifies session expiry when the probe itself 401s', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(401, { error: 'unauthorized' }))
    vi.stubGlobal('fetch', fetchMock)

    const listener = vi.fn()
    const unsubscribe = onSessionExpired(listener)

    renderHook(() => useRunEvents('run-1'))

    expect(FakeEventSource.instances).toHaveLength(1)
    FakeEventSource.instances[0].emitError()

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith('/api/me', undefined)
    })
    await waitFor(() => {
      expect(listener).toHaveBeenCalledTimes(1)
    })

    unsubscribe()
  })

  it('does not notify session expiry when the probe fails at the network level, not with a 401', async () => {
    const fetchMock = vi.fn().mockRejectedValue(new TypeError('Failed to fetch'))
    vi.stubGlobal('fetch', fetchMock)

    const listener = vi.fn()
    const unsubscribe = onSessionExpired(listener)

    renderHook(() => useRunEvents('run-1'))

    FakeEventSource.instances[0].emitError()

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith('/api/me', undefined)
    })

    // Give the rejected probe's .catch()/.finally() a turn to run.
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(listener).not.toHaveBeenCalled()

    unsubscribe()
  })

  it('does not notify session expiry when the probe succeeds (a transient drop that already recovered)', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { subject: 'user-1' }))
    vi.stubGlobal('fetch', fetchMock)

    const listener = vi.fn()
    const unsubscribe = onSessionExpired(listener)

    renderHook(() => useRunEvents('run-1'))

    FakeEventSource.instances[0].emitError()

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith('/api/me', undefined)
    })

    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(listener).not.toHaveBeenCalled()

    unsubscribe()
  })

  it('does not overlap probes for repeated error events while one is already in flight', async () => {
    // A manually-resolvable probe response, so the test controls exactly
    // when the first (and only) probe settles while further error events
    // keep arriving. (Definite-assignment `!`: a Promise executor runs
    // synchronously, so resolveFirstProbe is assigned before use, but
    // TypeScript cannot see that through the callback.)
    let resolveFirstProbe!: (response: ReturnType<typeof jsonResponse>) => void
    const firstProbe = new Promise<ReturnType<typeof jsonResponse>>((resolve) => {
      resolveFirstProbe = resolve
    })
    const fetchMock = vi.fn().mockImplementation(() => firstProbe)
    vi.stubGlobal('fetch', fetchMock)

    const listener = vi.fn()
    const unsubscribe = onSessionExpired(listener)

    renderHook(() => useRunEvents('run-1'))

    FakeEventSource.instances[0].emitError()
    FakeEventSource.instances[0].emitError()

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(1)
    })

    resolveFirstProbe(jsonResponse(401, { error: 'unauthorized' }))

    await waitFor(() => {
      expect(listener).toHaveBeenCalledTimes(1)
    })

    unsubscribe()
  })
})

describe('useRunEvents terminal handling', () => {
  it('does not probe /api/me on a connection drop that follows the terminal done event', () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)

    renderHook(() => useRunEvents('run-1'))

    const source = FakeEventSource.instances[0]
    act(() => {
      source.emit('done', JSON.stringify({ runId: 'run-1', status: 'Completed' }))
    })

    // The server closes the stream on purpose after "done"; a resulting error
    // event is expected and carries no session signal, so no probe must fire.
    source.emitError()

    expect(fetchMock).not.toHaveBeenCalled()
  })
})

describe('useRunEvents run switching', () => {
  it('resets state and detail when runId changes so the new run does not inherit the old terminal state', () => {
    const { result, rerender } = renderHook(({ id }) => useRunEvents(id), { initialProps: { id: 'run-A' } })

    // Run A finishes: its terminal "done" frame lands, so state is 'terminal'
    // and detail carries run A.
    act(() => {
      FakeEventSource.instances[0].emit('done', JSON.stringify({ runId: 'run-A', status: 'Completed' }))
    })

    expect(result.current.state).toBe('terminal')
    expect(result.current.detail?.runId).toBe('run-A')

    // The watched run flips to a fresh run B (e.g. Dashboard's active-run swap).
    // Before B's first frame, the hook must not still report run A's terminal
    // state under run B — it resets to connecting/null.
    rerender({ id: 'run-B' })

    expect(result.current.state).toBe('connecting')
    expect(result.current.detail).toBeNull()
  })
})
