import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import PhaseDetail from './PhaseDetail'
import type { PhaseInfo } from './RunDetail'

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

// Every render below reaches LogPanel (and, for Write, DriveMetricsPanel) —
// stub every fetch it could issue with a quiet, non-live/unavailable
// default so these tests exercise PhaseDetail's own rendering, not those
// panels' internals (already covered by their own colocated test files).
function stubPanels() {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : String(input)

      if (url.includes('/logs')) {
        return Promise.resolve(jsonResponse(200, { runId: 'run-1', lines: [], live: false }))
      }

      return Promise.resolve(jsonResponse(503, { error: 'unavailable' }))
    }),
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('PhaseDetail', () => {
  it('renders a completed phase’s facts and its phase-scoped log panel', async () => {
    stubPanels()

    const phase: PhaseInfo = {
      name: 'Resolve',
      status: 'completed',
      startTime: '2026-07-09T12:00:00Z',
      endTime: '2026-07-09T12:01:00Z',
      facts: [{ key: 'archives', label: 'Archives', value: '71' }],
    }

    render(<PhaseDetail runId="run-1" index={1} phase={phase} terminal={false} />)

    expect(screen.getByRole('heading', { name: 'Resolve' })).toBeInTheDocument()
    expect(screen.getByText('PHASE 1')).toBeInTheDocument()
    expect(screen.getByText('Archives')).toBeInTheDocument()
    expect(screen.getByText('71')).toBeInTheDocument()

    await waitFor(() => {
      expect(screen.getByRole('log')).toBeInTheDocument()
    })
  })

  it('renders a pending placeholder without a log panel', () => {
    stubPanels()

    const phase: PhaseInfo = { name: 'Burn', status: 'pending', facts: [] }

    render(<PhaseDetail runId="run-1" index={10} phase={phase} terminal={false} />)

    expect(screen.getByText(/not started/i)).toBeInTheDocument()
    expect(screen.queryByRole('log')).not.toBeInTheDocument()
  })

  it('renders a failed phase’s error console alongside its log', async () => {
    stubPanels()

    const phase: PhaseInfo = {
      name: 'Verify',
      status: 'failed',
      startTime: '2026-07-09T12:00:00Z',
      error: 'checksum mismatch for archive 004',
      facts: [],
    }

    render(<PhaseDetail runId="run-1" index={5} phase={phase} terminal={false} />)

    expect(screen.getByText('checksum mismatch for archive 004')).toBeInTheDocument()
    expect(screen.getByText('FAILED')).toBeInTheDocument()
  })

  it('embeds DriveMetricsPanel alongside the log for the Write phase only', () => {
    stubPanels()

    const write: PhaseInfo = { name: 'Write', status: 'active', startTime: '2026-07-09T12:00:00Z', facts: [] }
    const { rerender } = render(<PhaseDetail runId="run-1" index={7} phase={write} terminal={false} />)

    expect(screen.getByText(/drive 0/i)).toBeInTheDocument()

    const verify: PhaseInfo = { name: 'Verify', status: 'active', startTime: '2026-07-09T12:00:00Z', facts: [] }
    rerender(<PhaseDetail runId="run-1" index={5} phase={verify} terminal={false} />)

    expect(screen.queryByText(/drive 0/i)).not.toBeInTheDocument()
  })
})
