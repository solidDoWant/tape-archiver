import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import RunOverview from './RunOverview'
import type { PhaseInfo, RunEventDetail } from './RunDetail'

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

// RunOverview embeds ConfigSummary and TapesSection, each of which fetches
// on its own — stub every call with a quiet default so these tests exercise
// RunOverview's own hero/pause-zone/progress rendering, not those panels'
// internals (covered by their own colocated test files).
function stubPanels() {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(jsonResponse(503, { error: 'unavailable' }))),
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
})

const phases: PhaseInfo[] = [
  { name: 'Resolve', status: 'completed', facts: [] },
  { name: 'Prepare', status: 'completed', facts: [] },
  { name: 'Pack', status: 'active', facts: [{ key: 'logicalTapes', label: 'Logical tapes', value: '3' }, { key: 'copies', label: 'Copies', value: '2' }] },
  { name: 'Generate PAR2', status: 'pending', facts: [] },
  { name: 'Verify', status: 'pending', facts: [] },
  { name: 'Load', status: 'pending', facts: [] },
  { name: 'Write', status: 'pending', facts: [] },
  { name: 'Eject', status: 'pending', facts: [] },
  { name: 'Report', status: 'pending', facts: [] },
  { name: 'Burn', status: 'pending', facts: [] },
  { name: 'Deliver', status: 'pending', facts: [] },
]

const runningDetail: RunEventDetail = {
  workflowId: 'backup',
  runId: 'run-1',
  status: 'Running',
  startTime: '2026-07-09T12:00:00Z',
  lastCompletedPhase: 'Prepare',
  currentPause: { kind: '' },
}

describe('RunOverview', () => {
  it('shows a running hero and the "no action needed" placeholder when not paused', () => {
    stubPanels()

    render(<RunOverview runId="run-1" detail={runningDetail} phases={phases} terminal={false} />)

    expect(screen.getByRole('heading', { name: /backup in progress/i })).toBeInTheDocument()
    expect(screen.getByText(/no operator action needed/i)).toBeInTheDocument()
    expect(screen.getByText('2 of 11 phases complete')).toBeInTheDocument()
  })

  it('shows the pause zone instead of the placeholder when paused', () => {
    stubPanels()

    render(
      <RunOverview
        runId="run-1"
        detail={{ ...runningDetail, currentPause: { kind: 'write-failure', reloadSlots: [12] } }}
        phases={phases}
        terminal={false}
      />,
    )

    expect(screen.getByRole('heading', { name: /backup paused/i })).toBeInTheDocument()
    expect(screen.getByText(/Load\/Write failure/)).toBeInTheDocument()
    expect(screen.queryByText(/no operator action needed/i)).not.toBeInTheDocument()
  })

  it('shows a failed-run error console for the failing phase', () => {
    stubPanels()

    const failedPhases: PhaseInfo[] = phases.map((phase) =>
      phase.name === 'Write' ? { ...phase, status: 'failed', error: 'drive reported a hard write error' } : phase,
    )

    render(
      <RunOverview
        runId="run-1"
        detail={{ ...runningDetail, status: 'Failed', closeTime: '2026-07-09T13:00:00Z' }}
        phases={failedPhases}
        terminal
      />,
    )

    expect(screen.getByRole('heading', { name: /backup failed/i })).toBeInTheDocument()
    expect(screen.getByText('drive reported a hard write error')).toBeInTheDocument()
    expect(screen.getByText(/write phase · workflow failed/i)).toBeInTheDocument()
  })

  it('passes the Pack phase’s observed logical-tape/copy facts down to the config summary', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : String(input)

        if (url.endsWith('/config')) {
          return Promise.resolve(jsonResponse(200, { runId: 'run-1', config: { sources: [], copies: 2, redundancy: {} } }))
        }

        return Promise.resolve(jsonResponse(503, { error: 'unavailable' }))
      }),
    )

    render(<RunOverview runId="run-1" detail={runningDetail} phases={phases} terminal={false} />)

    await waitFor(() => {
      expect(screen.getByText('6')).toBeInTheDocument() // 3 logical tapes × 2 copies.
    })
  })
})
