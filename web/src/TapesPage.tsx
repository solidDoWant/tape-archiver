import { useEffect, useState } from 'react'
import { apiFetch, ApiError, describeNetworkError, formatTimestamp } from './api'
import { Link } from './router'
import { runPath } from './route'
import { IconWarning } from './icons'

// WriteHealthInfo/TapeOutcome mirror pkg/runsapi's tape-outcome JSON shapes
// (tapes.go) — the same shapes DriveMetricsPanel.tsx's FinalDriveMetrics
// already consumes for a single run's GET /api/runs/{runID}/tapes. This page
// consumes the aggregate GET /api/tapes instead (every tape across the most
// recent runs still in Temporal visibility), so the outcome/write-health
// shape is duplicated here rather than imported from DriveMetricsPanel.tsx —
// that file's copy is private to its own single-run view and this page's
// AggregateTapeOutcome has extra run-attribution fields TapeOutcome there
// doesn't carry.
interface WriteHealthInfo {
  measured: boolean
  throughputMBps: number
  floorMBps?: number
  floorKnown: boolean
  belowFloor: boolean
  repositions?: number
  repositionsMeasured: boolean
  tapeAlertFlags?: string[]
  healthy: boolean
}

// AggregateTapeOutcome mirrors pkg/runsapi.AggregateTapeOutcome's JSON shape
// (tapes.go): one physical tape a run loaded, plus which run wrote it.
interface AggregateTapeOutcome {
  barcode: string
  tapeIndex: number
  copyIndex: number
  driveIndex: number
  slot: number
  result: string
  error?: string
  overwroteNonBlank?: boolean
  writeHealth?: WriteHealthInfo
  runId: string
  runStartTime: string
  runStatus: string
}

// RunError mirrors pkg/runsapi.RunError's JSON shape: a run still in
// Temporal visibility whose tape outcomes could not be reconstructed.
interface RunError {
  runId: string
  error: string
}

// AggregateTapesResponse mirrors pkg/runsapi.AggregateTapesResponse's JSON
// shape, the GET /api/tapes response body.
interface AggregateTapesResponse {
  tapes: AggregateTapeOutcome[]
  runErrors?: RunError[]
}

type LoadState =
  | { status: 'loading' }
  | { status: 'error'; error: string }
  | { status: 'loaded'; tapes: AggregateTapeOutcome[]; runErrors: RunError[] }

// outcomeBadgeClass colors a tape's write outcome (pkg/runsapi's
// tapeOutcomeLoaded/tapeOutcomeWritten/tapeOutcomeFailed): written is the
// only unambiguously good state (green); failed is unambiguously bad (red);
// loaded means the tape was loaded but its write pipeline never reached a
// terminal state in this run's history (still in progress, or the run ended
// before finishing it) — neither good nor bad on its own, so it gets the
// same neutral "in progress" blue RunHistory.tsx uses for a live run
// (tokens.css defines --blue/--blue-bg but no --blue-line, hence the
// opacity-modified border here instead of a *-line token).
function outcomeBadgeClass(result: string): string {
  switch (result) {
    case 'written':
      return 'bg-green-bg text-green border-green-line'
    case 'failed':
      return 'bg-red-bg text-red border-red-line'
    default:
      return 'bg-blue-bg text-blue border-blue/30'
  }
}

// WriteHealthCell renders a compact one-line summary of a tape's measured
// write health (SPEC §14): unmeasured tapes (still in progress, or a run
// that ended before MeasureWriteHealth ran for this barcode) show a plain
// dash rather than an empty cell that could be mistaken for "healthy". A
// TapeAlert flag takes priority over "below floor" as the more serious
// signal when a tape has both.
function WriteHealthCell({ health }: { health?: WriteHealthInfo }) {
  if (!health || !health.measured) {
    return <span className="text-text-faint">not measured</span>
  }

  const hasTapeAlert = (health.tapeAlertFlags?.length ?? 0) > 0

  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
      <span className="font-mono text-text">{health.throughputMBps.toFixed(0)} MB/s</span>
      {health.floorKnown ? (
        <span className="font-mono text-[11px] text-text-faint">floor {health.floorMBps}</span>
      ) : null}
      {hasTapeAlert ? (
        <span className="inline-flex items-center gap-1 rounded-full border border-red-line bg-red-bg px-2 py-0.5 text-[11px] font-medium text-red">
          <IconWarning className="h-3 w-3" />
          TapeAlert
        </span>
      ) : health.belowFloor ? (
        <span className="inline-flex items-center gap-1 rounded-full border border-amber-line bg-amber-bg px-2 py-0.5 text-[11px] font-medium text-amber">
          <IconWarning className="h-3 w-3" />
          below floor
        </span>
      ) : health.healthy ? (
        <span className="rounded-full border border-green-line bg-green-bg px-2 py-0.5 text-[11px] font-medium text-green">
          healthy
        </span>
      ) : null}
    </div>
  )
}

// TapesPage lists every physical tape resolved from run history (issue
// #278): the epic's non-goal explicitly drops the design's live
// "IN THE LIBRARY NOW" changer-element table (DESIGN_ANALYSIS.md §2 "C.
// Tapes" — that needs a live SCSI element-status endpoint this epic never
// builds, SPEC §4.2's no-cross-run-catalog principle), so this page ships
// only the second, history-resolved table: one row per physical tape a run
// loaded, backed entirely by issue #273's GET /api/tapes aggregate endpoint
// (itself reconstructed live from Temporal workflow history on every
// request — nothing here is persisted UI-owned state).
function TapesPage() {
  const [state, setState] = useState<LoadState>({ status: 'loading' })

  useEffect(() => {
    let cancelled = false

    async function load() {
      setState({ status: 'loading' })

      try {
        // No `?limit=` override: the API's own default
        // (defaultListTapesRunLimit, 50 of the newest runs — tapes.go)
        // already bounds this to a page-worthy amount of history, and the
        // reference design has no "show more"/limit control on this page
        // (dc.html's Tapes section) — see docs/web-ui-design.md's decisions
        // log for this issue.
        const response = await apiFetch<AggregateTapesResponse>('/api/tapes')
        if (cancelled) {
          return
        }

        setState({ status: 'loaded', tapes: response.tapes, runErrors: response.runErrors ?? [] })
      } catch (error) {
        if (cancelled) {
          return
        }

        const message = error instanceof ApiError ? error.message : describeNetworkError(error)
        setState({ status: 'error', error: message })
      }
    }

    void load()

    return () => {
      cancelled = true
    }
  }, [])

  return (
    <div className="flex flex-col gap-5 p-6 sm:p-7">
      <div>
        <h1 className="text-lg font-semibold tracking-tight">Tapes</h1>
        <p className="mt-1 max-w-2xl text-[12.5px] text-text-dim">
          Every physical tape written by a run still inside Temporal's history window,
          resolved from that run's own execution history.
        </p>
      </div>

      <div className="flex max-w-3xl gap-2.5 rounded-xl border border-border bg-surface-2 p-3.5 text-[12px] leading-relaxed text-text-dim">
        <span aria-hidden="true" className="flex-none text-text-faint">
          ⓘ
        </span>
        <p>
          The archiver keeps no persistent tape catalog (SPEC §4.2) — there is no
          permanent inventory to show, and this page does not read live status from the
          tape changer. Every row below is reconstructed on the fly from a completed or
          in-progress run's own Temporal execution history. Once a run ages out of
          Temporal's visibility retention, the tapes it wrote drop off this list — its
          PDF report is the permanent record of what it wrote.
        </p>
      </div>

      {state.status === 'loading' ? (
        <p role="status" className="text-[12.5px] text-text-dim">
          Loading tapes…
        </p>
      ) : null}

      {state.status === 'error' ? (
        <div
          role="alert"
          className="max-w-3xl rounded-xl border border-red-line bg-red-bg p-3.5 text-[12.5px] text-red"
        >
          {state.error}
        </div>
      ) : null}

      {state.status === 'loaded' ? (
        <>
          {state.runErrors.length > 0 ? (
            <div
              role="status"
              className="flex max-w-3xl flex-col gap-2 rounded-xl border border-amber-line bg-amber-bg p-3.5 text-[12px] text-amber"
            >
              <div className="flex items-center gap-2 font-medium">
                <IconWarning className="h-3.5 w-3.5" />
                {state.runErrors.length === 1
                  ? '1 run could not be reconstructed'
                  : `${state.runErrors.length} runs could not be reconstructed`}
              </div>
              <ul className="flex flex-col gap-1 font-mono text-[11px]">
                {state.runErrors.map((runError) => (
                  <li key={runError.runId}>
                    {runError.runId} — {runError.error}
                  </li>
                ))}
              </ul>
            </div>
          ) : null}

          {state.tapes.length === 0 ? (
            <div className="max-w-3xl rounded-xl border border-dashed border-border-strong bg-surface p-6 text-center text-[12.5px] text-text-faint">
              No tapes to show yet. This list fills in once a run has loaded at least one
              tape — check back after a run's Load phase, or submit a new run.
            </div>
          ) : (
            <div className="overflow-x-auto rounded-xl border border-border bg-surface shadow-card">
              <table className="w-full min-w-[720px] border-collapse text-left text-[12.5px]">
                <thead>
                  <tr className="border-b border-border bg-surface-2 text-[11px] tracking-wide text-text-faint uppercase">
                    <th scope="col" className="px-4 py-2.5 font-mono font-medium">
                      Barcode
                    </th>
                    <th scope="col" className="px-4 py-2.5 font-mono font-medium">
                      Run
                    </th>
                    <th scope="col" className="px-4 py-2.5 font-mono font-medium">
                      Tape / copy
                    </th>
                    <th scope="col" className="px-4 py-2.5 font-mono font-medium">
                      Outcome
                    </th>
                    <th scope="col" className="px-4 py-2.5 font-mono font-medium">
                      Write health
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {state.tapes.map((tape) => (
                    <tr
                      key={`${tape.runId}-${tape.barcode}`}
                      className="border-b border-border last:border-0"
                    >
                      <td className="px-4 py-3 font-mono font-semibold text-text">{tape.barcode}</td>
                      <td className="px-4 py-3">
                        <Link
                          to={runPath(tape.runId)}
                          className="font-mono font-medium text-blue transition-opacity hover:opacity-70"
                        >
                          {tape.runId}
                        </Link>
                        <div className="font-mono text-[11px] text-text-faint">
                          {formatTimestamp(tape.runStartTime)}
                        </div>
                      </td>
                      <td className="px-4 py-3 font-mono text-text-dim">
                        tape {tape.tapeIndex} · copy {tape.copyIndex}
                      </td>
                      <td className="px-4 py-3">
                        <span
                          className={`inline-flex rounded-full border px-2.5 py-0.5 text-[11px] font-semibold ${outcomeBadgeClass(tape.result)}`}
                          title={tape.result === 'failed' ? tape.error : undefined}
                        >
                          {tape.result}
                        </span>
                      </td>
                      <td className="px-4 py-3">
                        <WriteHealthCell health={tape.writeHealth} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          <p className="max-w-3xl text-[11px] text-text-faint">
            Tapes from runs that have aged out of Temporal's history are not listed —
            their contents can no longer be reconstructed here.
          </p>
        </>
      ) : null}
    </div>
  )
}

export default TapesPage
