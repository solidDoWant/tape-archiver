import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import TapesPage from './TapesPage'
import { RouterProvider } from './router'

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

function renderTapesPage() {
  return render(
    <RouterProvider>
      <TapesPage />
    </RouterProvider>,
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('TapesPage', () => {
  it('always explains that the listing is derived from run history, not a live catalog', () => {
    vi.stubGlobal('fetch', vi.fn().mockReturnValue(new Promise(() => {})))

    renderTapesPage()

    expect(screen.getByText(/no persistent tape catalog/i)).toBeInTheDocument()
    expect(screen.getByText(/does not read live status from the tape changer/i)).toBeInTheDocument()
    // The listing's reach (the API's newest-runs default) must be disclosed
    // too — "derived from history" alone would wrongly imply everything
    // still within Temporal retention is covered.
    expect(screen.getByText(/50 most recent runs/i)).toBeInTheDocument()
  })

  it('shows a loading state before the response arrives', () => {
    vi.stubGlobal('fetch', vi.fn().mockReturnValue(new Promise(() => {})))

    renderTapesPage()

    expect(screen.getByRole('status')).toHaveTextContent(/loading tapes/i)
  })

  it('shows an error state when the fetch fails', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(jsonResponse(500, { error: 'Temporal is unreachable.' })),
    )

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent('Temporal is unreachable.')
    })
  })

  it('shows an empty state instead of an empty table when no tapes are found', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(200, { tapes: [] })))

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByText(/no tapes to show yet/i)).toBeInTheDocument()
    })

    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })

  it('lists each tape with its barcode, a link to the run that wrote it, its tape/copy index, and its outcome', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          tapes: [
            {
              barcode: 'TA0001L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              slot: 1,
              result: 'written',
              runId: 'run-1',
              runStartTime: '2026-07-01T00:00:00Z',
              runStatus: 'Completed',
              writeHealth: {
                measured: true,
                throughputMBps: 142,
                floorMBps: 50,
                floorKnown: true,
                belowFloor: false,
                repositions: 0,
                repositionsMeasured: true,
                healthy: true,
              },
            },
          ],
        }),
      ),
    )

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByRole('table')).toBeInTheDocument()
    })

    expect(screen.getByText('TA0001L6')).toBeInTheDocument()
    expect(screen.getByText('written')).toBeInTheDocument()
    expect(screen.getByText(/tape 0 · copy 0/i)).toBeInTheDocument()
    expect(screen.getByText('142 MB/s')).toBeInTheDocument()

    const runLink = screen.getByRole('link', { name: 'run-1' })
    expect(runLink).toHaveAttribute('href', '/runs/run-1')
  })

  it('does not crash on a measured tape whose throughput/floor fields are missing', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          tapes: [
            {
              barcode: 'TA0002L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              slot: 2,
              result: 'written',
              runId: 'run-2',
              runStartTime: '2026-07-01T00:00:00Z',
              runStatus: 'Completed',
              // measured, but the numeric fields the type marks required are
              // absent on the wire (a server-side invariant violation) — the
              // cell must not throw on throughputMBps.toFixed nor render a blank
              // "floor ".
              writeHealth: {
                measured: true,
                floorKnown: true,
                belowFloor: false,
                repositions: 0,
                repositionsMeasured: true,
                healthy: true,
              },
            },
          ],
        }),
      ),
    )

    renderTapesPage()

    await waitFor(() => expect(screen.getByRole('table')).toBeInTheDocument())

    expect(screen.getByText('TA0002L6')).toBeInTheDocument()
    expect(screen.queryByText(/undefined|NaN/)).not.toBeInTheDocument()
  })

  it('shows a below-floor warning and a TapeAlert warning on their respective tapes', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          tapes: [
            {
              barcode: 'TA0002L6',
              tapeIndex: 0,
              copyIndex: 1,
              driveIndex: 1,
              slot: 2,
              result: 'written',
              runId: 'run-2',
              runStartTime: '2026-07-02T00:00:00Z',
              runStatus: 'Completed',
              writeHealth: {
                measured: true,
                throughputMBps: 38,
                floorMBps: 50,
                floorKnown: true,
                belowFloor: true,
                repositionsMeasured: false,
                healthy: false,
              },
            },
            {
              barcode: 'TA0003L6',
              tapeIndex: 1,
              copyIndex: 0,
              driveIndex: 0,
              slot: 3,
              result: 'failed',
              error: 'drive reported a hard write error',
              runId: 'run-2',
              runStartTime: '2026-07-02T00:00:00Z',
              runStatus: 'Failed',
              writeHealth: {
                measured: true,
                throughputMBps: 60,
                floorKnown: false,
                belowFloor: false,
                repositionsMeasured: false,
                tapeAlertFlags: ['CLEANING_NEEDED'],
                healthy: false,
              },
            },
          ],
        }),
      ),
    )

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByText('failed')).toBeInTheDocument()
    })

    expect(screen.getByText(/below floor/i)).toBeInTheDocument()
    expect(screen.getByText(/tapealert/i)).toBeInTheDocument()
    // The failed tape's full failure text is deliberately NOT rendered here: an
    // ltfs/tar stderr dump makes the whole table unreadable. The badge marks
    // the failure; the run's detail page and PDF report carry the reason.
    expect(screen.queryByText('drive reported a hard write error')).not.toBeInTheDocument()
  })

  it('flags a tape that overwrote a non-blank tape, alongside its written outcome', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          tapes: [
            {
              barcode: 'TA0008L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              slot: 1,
              result: 'written',
              overwroteNonBlank: true,
              runId: 'run-7',
              runStartTime: '2026-07-07T00:00:00Z',
              runStatus: 'Completed',
              writeHealth: {
                measured: true,
                throughputMBps: 140,
                floorMBps: 50,
                floorKnown: true,
                belowFloor: false,
                repositions: 0,
                repositionsMeasured: true,
                healthy: true,
              },
            },
          ],
        }),
      ),
    )

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByText(/overwrote non-blank/i)).toBeInTheDocument()
    })

    // The safety flag sits alongside the ordinary "written" outcome, never
    // replacing it — the operator sees both.
    expect(screen.getByText('written')).toBeInTheDocument()
  })

  it('shows a repositions warning when a tape recorded repositions, alongside any other warning', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          tapes: [
            {
              barcode: 'TA0006L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              slot: 1,
              result: 'written',
              runId: 'run-5',
              runStartTime: '2026-07-05T00:00:00Z',
              runStatus: 'Completed',
              writeHealth: {
                measured: true,
                throughputMBps: 41,
                floorMBps: 50,
                floorKnown: true,
                belowFloor: true,
                repositions: 3,
                repositionsMeasured: true,
                healthy: false,
              },
            },
          ],
        }),
      ),
    )

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByText(/3 repositions/i)).toBeInTheDocument()
    })

    // Repositions and below-floor are independent problems — one badge must
    // never suppress the other, and neither may masquerade as healthy.
    expect(screen.getByText(/below floor/i)).toBeInTheDocument()
    expect(screen.queryByText('healthy')).not.toBeInTheDocument()
  })

  it('says so explicitly when repositions could not be measured, instead of implying zero', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          tapes: [
            {
              barcode: 'TA0007L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              slot: 1,
              result: 'written',
              runId: 'run-6',
              runStartTime: '2026-07-06T00:00:00Z',
              runStatus: 'Completed',
              writeHealth: {
                measured: true,
                throughputMBps: 140,
                floorMBps: 50,
                floorKnown: true,
                belowFloor: false,
                repositionsMeasured: false,
                healthy: false,
              },
            },
          ],
        }),
      ),
    )

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByText(/repositions not measured/i)).toBeInTheDocument()
    })

    expect(screen.queryByText('healthy')).not.toBeInTheDocument()
  })

  it('shows an unmeasured note when a tape has no write-health measurement', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          tapes: [
            {
              barcode: 'TA0004L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              slot: 1,
              result: 'loaded',
              runId: 'run-3',
              runStartTime: '2026-07-03T00:00:00Z',
              runStatus: 'Running',
            },
          ],
        }),
      ),
    )

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByText('loaded')).toBeInTheDocument()
    })

    expect(screen.getByText(/not measured/i)).toBeInTheDocument()
  })

  it('renders a non-fatal degradation notice listing runs that could not be reconstructed, alongside the tapes that could', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          tapes: [
            {
              barcode: 'TA0005L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              slot: 1,
              result: 'written',
              runId: 'run-4',
              runStartTime: '2026-07-04T00:00:00Z',
              runStatus: 'Completed',
            },
          ],
          runErrors: [{ runId: 'run-old', error: 'workflow history not found' }],
        }),
      ),
    )

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByText(/could not be reconstructed/i)).toBeInTheDocument()
    })

    expect(screen.getByText(/run-old/)).toBeInTheDocument()
    expect(screen.getByText(/workflow history not found/)).toBeInTheDocument()
    // The degraded run must not hide the tapes that DID reconstruct fine.
    expect(screen.getByText('TA0005L6')).toBeInTheDocument()
  })

  it('hides dry-run (mhvtl) tapes by default so the page reflects the physical library', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          tapes: [
            {
              barcode: 'PHYS0001L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              slot: 1,
              result: 'written',
              runId: 'run-prod',
              runStartTime: '2026-07-08T00:00:00Z',
              runStatus: 'Completed',
              dryRun: false,
            },
            {
              barcode: 'VIRT0001L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              slot: 1,
              result: 'written',
              runId: 'run-dry',
              runStartTime: '2026-07-09T00:00:00Z',
              runStatus: 'Completed',
              dryRun: true,
            },
          ],
        }),
      ),
    )

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByText('PHYS0001L6')).toBeInTheDocument()
    })

    // The dry-run tape is hidden by default; the physical tape is shown.
    expect(screen.queryByText('VIRT0001L6')).not.toBeInTheDocument()
  })

  it('shows dry-run tapes tagged with a DRY-RUN badge when the toggle is enabled', async () => {
    const user = userEvent.setup()

    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          tapes: [
            {
              barcode: 'PHYS0001L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              slot: 1,
              result: 'written',
              runId: 'run-prod',
              runStartTime: '2026-07-08T00:00:00Z',
              runStatus: 'Completed',
              dryRun: false,
            },
            {
              barcode: 'VIRT0001L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              slot: 1,
              result: 'written',
              runId: 'run-dry',
              runStartTime: '2026-07-09T00:00:00Z',
              runStatus: 'Completed',
              dryRun: true,
            },
          ],
        }),
      ),
    )

    renderTapesPage()

    const toggle = await screen.findByRole('checkbox', { name: /show dry-run tapes/i })
    expect(screen.queryByText('VIRT0001L6')).not.toBeInTheDocument()

    await user.click(toggle)

    // Now the dry-run tape is listed, distinguishable by its DRY-RUN badge so a
    // virtual barcode is never mistaken for physical media.
    expect(screen.getByText('VIRT0001L6')).toBeInTheDocument()
    expect(screen.getByText('DRY-RUN')).toBeInTheDocument()
  })

  it('explains an all-dry-run listing rather than reading as empty', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          tapes: [
            {
              barcode: 'VIRT0001L6',
              tapeIndex: 0,
              copyIndex: 0,
              driveIndex: 0,
              slot: 1,
              result: 'written',
              runId: 'run-dry',
              runStartTime: '2026-07-09T00:00:00Z',
              runStatus: 'Completed',
              dryRun: true,
            },
          ],
        }),
      ),
    )

    renderTapesPage()

    await waitFor(() => {
      expect(screen.getByText(/only dry-run .* tapes were found/i)).toBeInTheDocument()
    })

    // The toggle is still offered so the operator can reveal them.
    expect(screen.getByRole('checkbox', { name: /show dry-run tapes/i })).toBeInTheDocument()
  })
})
