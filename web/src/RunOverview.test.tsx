import type { ReactElement } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen, waitFor } from '@testing-library/react'
import RunOverview from './RunOverview'
import { RouterProvider } from './router'
import type { PhaseInfo, RunEventDetail } from './RunDetail'

// renderOverview wraps RunOverview in a RouterProvider: its hero now renders
// RestartRunButton (terminal runs) / CancelRunButton, and RestartRunButton
// calls useNavigate, which needs the router context. useActiveRun's own
// GET /api/runs falls through to the tests' 503 fetch default, leaving the
// Restart button disabled — fine for these render-focused assertions.
function renderOverview(ui: ReactElement) {
  return render(<RouterProvider>{ui}</RouterProvider>)
}

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

// settlePanels flushes the fetch-on-mount state updates of RunOverview's
// embedded panels (ConfigSummary, TapesSection, LiveDriveMetrics, Footer,
// RestartRunButton's active-run check) inside act(). Every test here renders
// those panels, which each resolve a fetch after a test's synchronous
// assertions have run; without awaiting this flush React reports their late
// setState as "not wrapped in act(...)". Awaiting async act() ticks drains the
// resolved-promise microtask chains those single-shot fetches sit on — twice,
// so a panel that chains a second request off its first (LiveDriveMetrics'
// per-drive history poll) also settles. Called explicitly at the end of each
// test (an afterEach runs after RTL's auto-cleanup has already unmounted, too
// late to capture the settles in act).
async function settlePanels() {
  await act(async () => {})
  await act(async () => {})
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
  dryRun: false,
  lastCompletedPhase: 'Prepare',
  currentPause: { kind: '' },
}

describe('RunOverview', () => {
  it('shows a running hero and the "no action needed" placeholder when not paused', async () => {
    stubPanels()

    renderOverview(<RunOverview runId="run-1" detail={runningDetail} phases={phases} terminal={false} />)

    expect(screen.getByRole('heading', { name: /backup in progress/i })).toBeInTheDocument()
    expect(screen.getByText(/no operator action needed/i)).toBeInTheDocument()
    expect(screen.getByText('2 of 11 phases complete')).toBeInTheDocument()

    await settlePanels()
  })

  it('offers a Cancel run button while the run is still in progress', async () => {
    stubPanels()

    renderOverview(<RunOverview runId="run-1" detail={runningDetail} phases={phases} terminal={false} />)

    expect(screen.getByRole('button', { name: /cancel run/i })).toBeInTheDocument()

    await settlePanels()
  })

  it('does not offer a Cancel run button for a terminal run (nothing left to stop)', async () => {
    stubPanels()

    renderOverview(
      <RunOverview
        runId="run-1"
        detail={{ ...runningDetail, status: 'Completed', closeTime: '2026-07-09T13:00:00Z' }}
        phases={phases}
        terminal
      />,
    )

    // Terminal runs swap the Cancel control for Restart in the same hero slot.
    expect(screen.queryByRole('button', { name: /cancel run/i })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /restart run/i })).toBeInTheDocument()

    await settlePanels()
  })

  it('shows the pause zone instead of the placeholder when paused', async () => {
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

    await settlePanels()
  })

  it('does not show the operator-action card for a run terminated while paused', async () => {
    stubPanels()

    // A run terminated (or completed) while waiting at a pause still reports its
    // last pause kind in currentPause, but it cannot be resumed/aborted, so the
    // closed run must read as terminal — never PAUSED, never an action card.
    renderOverview(
      <RunOverview
        runId="run-1"
        detail={{
          ...runningDetail,
          status: 'Terminated',
          closeTime: '2026-07-09T13:00:00Z',
          currentPause: { kind: 'eject', affectedTapes: ['TA0001L6'], awaitingExport: 1 },
        }}
        phases={phases}
        terminal
      />,
    )

    expect(screen.getByText('TERMINATED')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: /backup paused/i })).not.toBeInTheDocument()
    expect(screen.queryByText(/operator action required/i)).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /resume/i })).not.toBeInTheDocument()

    await settlePanels()
  })

  it('shows an uncertainty hero — not PAUSED — when the pause status is unknown', async () => {
    stubPanels()

    render(
      <RunOverview
        runId="run-1"
        detail={{ ...runningDetail, currentPause: { kind: '', unknown: true } }}
        phases={phases}
        terminal={false}
      />,
    )

    // The hero must not assert "Backup paused" (the pause query failed —
    // the run may or may not be paused); PauseActions' own warning carries
    // the detail.
    expect(screen.getByText('PAUSE STATUS UNKNOWN')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: /backup paused/i })).not.toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent(/pause status unavailable/i)

    await settlePanels()
  })

  it('shows a failed-run error console for the failing phase', async () => {
    stubPanels()

    const failedPhases: PhaseInfo[] = phases.map((phase) =>
      phase.name === 'Write' ? { ...phase, status: 'failed', error: 'drive reported a hard write error' } : phase,
    )

    renderOverview(
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

    await settlePanels()
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

    renderOverview(<RunOverview runId="run-1" detail={runningDetail} phases={phases} terminal={false} />)

    await waitFor(() => {
      expect(screen.getByText('6')).toBeInTheDocument() // 3 logical tapes × 2 copies.
    })

    await settlePanels()
  })

  it('links to the Temporal workflow history when a UI base URL is configured', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : String(input)

        if (url.endsWith('/api/config/ui')) {
          return Promise.resolve(jsonResponse(200, { temporalUiBaseUrl: 'https://temporal.example.com', temporalNamespace: 'prod' }))
        }

        return Promise.resolve(jsonResponse(503, { error: 'unavailable' }))
      }),
    )

    renderOverview(<RunOverview runId="run-1" detail={runningDetail} phases={phases} terminal={false} />)

    const link = await screen.findByRole('link', { name: /temporal workflow/i })
    expect(link).toHaveAttribute('href', 'https://temporal.example.com/namespaces/prod/workflows/backup/run-1/history')
    expect(link).toHaveAttribute('target', '_blank')

    await settlePanels()
  })

  it('shows no Temporal workflow link when no UI base URL is configured', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : String(input)

        if (url.endsWith('/api/config/ui')) {
          return Promise.resolve(jsonResponse(200, { temporalUiBaseUrl: '', temporalNamespace: '' }))
        }

        return Promise.resolve(jsonResponse(503, { error: 'unavailable' }))
      }),
    )

    renderOverview(<RunOverview runId="run-1" detail={runningDetail} phases={phases} terminal={false} />)

    // The overview's own content is synchronous; wait a tick for the config
    // fetch to resolve, then confirm the link never appears.
    await waitFor(() => expect(screen.getByRole('heading', { name: /backup in progress/i })).toBeInTheDocument())
    expect(screen.queryByRole('link', { name: /temporal workflow/i })).not.toBeInTheDocument()

    await settlePanels()
  })

  it('links to the Discord report message when the run delivered one', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : String(input)

        if (url.endsWith('/delivery')) {
          return Promise.resolve(jsonResponse(200, { runId: 'run-1', messageUrl: 'https://discord.com/channels/g1/c1/m1' }))
        }

        return Promise.resolve(jsonResponse(503, { error: 'unavailable' }))
      }),
    )

    renderOverview(<RunOverview runId="run-1" detail={{ ...runningDetail, status: 'Completed' }} phases={phases} terminal />)

    const link = await screen.findByRole('link', { name: /discord report/i })
    expect(link).toHaveAttribute('href', 'https://discord.com/channels/g1/c1/m1')
    expect(link).toHaveAttribute('target', '_blank')

    await settlePanels()
  })

  it('shows no Discord report link when the run delivered none', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : String(input)

        if (url.endsWith('/delivery')) {
          return Promise.resolve(jsonResponse(200, { runId: 'run-1', messageUrl: '' }))
        }

        return Promise.resolve(jsonResponse(503, { error: 'unavailable' }))
      }),
    )

    renderOverview(<RunOverview runId="run-1" detail={{ ...runningDetail, status: 'Completed' }} phases={phases} terminal />)

    await waitFor(() => expect(screen.getByRole('heading', { name: /backup completed/i })).toBeInTheDocument())
    expect(screen.queryByRole('link', { name: /discord report/i })).not.toBeInTheDocument()

    await settlePanels()
  })

  // writingPhases advances the running fixture to the Write phase, so the
  // overview's live drive-write-health section (issue #307) is in scope.
  const writingPhases: PhaseInfo[] = phases.map((phase) => {
    if (phase.name === 'Pack' || phase.name === 'Generate PAR2' || phase.name === 'Verify' || phase.name === 'Load') {
      return { ...phase, status: 'completed' }
    }

    return phase.name === 'Write' ? { ...phase, status: 'active' } : phase
  })

  it('surfaces the live per-drive write-rate gauges once the run is writing (AC1)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : String(input)

        if (url.includes('/metrics/drives/TA0001L6/history')) {
          return Promise.resolve(
            jsonResponse(200, {
              runId: 'run-1',
              barcode: 'TA0001L6',
              metric: 'throughput',
              points: [{ time: '2026-07-09T12:30:00Z', value: 142 }],
            }),
          )
        }

        if (url.includes('/metrics/drives')) {
          return Promise.resolve(
            jsonResponse(200, {
              runId: 'run-1',
              drives: [
                {
                  barcode: 'TA0001L6',
                  tapeIndex: 0,
                  copyIndex: 0,
                  driveIndex: 0,
                  result: 'loaded',
                  hasData: true,
                  throughputMBps: 142,
                  repositions: 0,
                  tapeAlertFlagCount: 0,
                  belowFloor: false,
                  floorMBps: 50,
                  floorKnown: true,
                },
              ],
            }),
          )
        }

        return Promise.resolve(jsonResponse(503, { error: 'unavailable' }))
      }),
    )

    renderOverview(<RunOverview runId="run-1" detail={runningDetail} phases={writingPhases} terminal={false} />)

    // The rate / floor / reposition figures come from the existing
    // VictoriaMetrics-backed /metrics/drives endpoint — no new backend.
    expect(await screen.findByText('142 MB/s')).toBeInTheDocument()
    expect(screen.getByText('floor 50')).toBeInTheDocument()
    expect(screen.getByText(/0 repositions/)).toBeInTheDocument()

    await settlePanels()
  })

  it('degrades the gauges to the unavailable state when VictoriaMetrics is unset, keeping the overview intact (AC2)', async () => {
    // Every fetch (including /metrics/drives) 503s, standing in for a
    // deployment with VICTORIAMETRICS_URL unset.
    stubPanels()

    renderOverview(<RunOverview runId="run-1" detail={runningDetail} phases={writingPhases} terminal={false} />)

    // The section renders its styled "unavailable" state, never a broken panel,
    // and the rest of the overview is unaffected.
    expect(await screen.findByText(/metrics unavailable/i)).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: /backup in progress/i })).toBeInTheDocument()
    expect(screen.getByText('6 of 11 phases complete')).toBeInTheDocument()

    await settlePanels()
  })

  it('omits the live drive-health section before the Write phase begins', async () => {
    stubPanels()

    // phases has Write still pending — the section must not render an empty
    // gauge during earlier phases.
    renderOverview(<RunOverview runId="run-1" detail={runningDetail} phases={phases} terminal={false} />)

    expect(screen.queryByText('Drive write health')).not.toBeInTheDocument()

    await settlePanels()
  })

  it('omits the live drive-health section for a terminal run (TapesSection already reports final health)', async () => {
    stubPanels()

    const doneWrite: PhaseInfo[] = writingPhases.map((phase) => (phase.name === 'Write' ? { ...phase, status: 'completed' } : phase))

    renderOverview(
      <RunOverview
        runId="run-1"
        detail={{ ...runningDetail, status: 'Completed', closeTime: '2026-07-09T13:00:00Z' }}
        phases={doneWrite}
        terminal
      />,
    )

    expect(screen.queryByText('Drive write health')).not.toBeInTheDocument()

    await settlePanels()
  })
})
