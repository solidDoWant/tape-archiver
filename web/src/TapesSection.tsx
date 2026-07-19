import { useEffect, useState } from 'react'
import { apiFetch, ApiError, describeNetworkError } from './api'
import { IconWarning } from './icons'

// WriteHealth/TapeOutcome mirror pkg/runsapi's GET /api/runs/{runID}/tapes
// JSON shapes (tapes.go) — the same contract DriveMetricsPanel.tsx's terminal
// view already decodes; duplicated here (rather than imported) because that
// file's copies are unexported and this component renders a different slice
// of the same fields (slot, copy/tape index — occupancy, not throughput).
interface WriteHealth {
  belowFloor: boolean
  tapeAlertFlags?: string[]
  measured: boolean
}

interface TapeOutcome {
  barcode: string
  tapeIndex: number
  copyIndex: number
  driveIndex: number
  slot: number
  result: string
  error?: string
  // overwroteNonBlank is true when this tape was found non-blank at load and
  // written over anyway because the run set library.allowNonBlankTapes (SPEC
  // §4.3 step 6) — surfaced as its own badge, since a deliberate overwrite is a
  // notable action, not a plain "written".
  overwroteNonBlank?: boolean
  writeHealth?: WriteHealth
}

interface RunTapesResponse {
  runId: string
  tapes: TapeOutcome[]
}

type TapesState =
  | { status: 'loading' }
  | { status: 'unavailable' }
  | { status: 'error'; message: string }
  | { status: 'no-data' }
  | { status: 'ready'; tapes: TapeOutcome[] }

function resultBadgeClass(result: string): string {
  switch (result) {
    case 'written':
      return 'bg-green-bg text-green border-green-line'
    case 'failed':
      return 'bg-red-bg text-red border-red-line'
    default:
      return 'bg-blue-bg text-blue border-blue/30'
  }
}

export interface TapesSectionProps {
  runId: string
  // terminal marks a run that has just reached a terminal status: tape
  // outcomes are reconstructed from workflow history (never live hardware
  // access, issue #277's read-only-historical acceptance criterion), so a
  // fresh fetch on that transition picks up the run's very last tapes/results
  // once, rather than polling a closed run's history forever.
  terminal: boolean
}

// TapesSection lists every physical tape this run has loaded so far — which
// storage slot it came from, which drive/copy/logical-tape index it fills,
// and its outcome — from GET /api/runs/{runID}/tapes (pkg/runsapi/tapes.go),
// reconstructed on demand from workflow history (SPEC §4.2: never a
// persisted catalog). This is the "which tapes/slots were written" half of
// issue #277's read-only historical acceptance criterion; it deliberately
// does not show live slot/drive *occupancy* for the library as a whole
// (epic #271's stated non-goal) — only this run's own recorded outcomes.
function TapesSection({ runId, terminal }: TapesSectionProps) {
  const [state, setState] = useState<TapesState>({ status: 'loading' })

  useEffect(() => {
    let cancelled = false

    apiFetch<RunTapesResponse>(`/api/runs/${encodeURIComponent(runId)}/tapes`)
      .then((response) => {
        if (!cancelled) {
          setState(response.tapes.length > 0 ? { status: 'ready', tapes: response.tapes } : { status: 'no-data' })
        }
      })
      .catch((error: unknown) => {
        if (cancelled) {
          return
        }

        // 410 and 404 deliberately share the "no longer available" copy
        // here — see ConfigSummary.tsx's identical catch for why the
        // page-level not-found/aged-out taxonomy does not apply inside
        // these panels.
        if (error instanceof ApiError && (error.status === 410 || error.status === 404)) {
          setState({ status: 'unavailable' })

          return
        }

        const message = error instanceof ApiError ? error.message : describeNetworkError(error)
        setState({ status: 'error', message })
      })

    return () => {
      cancelled = true
    }
    // terminal is a dependency deliberately: the run's tape outcomes can only
    // change up until it closes, so the transition to terminal (not a poll
    // interval) is what triggers the one refetch needed to pick up the
    // run's final results.
  }, [runId, terminal])

  if (state.status === 'loading') {
    return <p className="text-[12px] text-text-faint">Loading tape outcomes…</p>
  }

  if (state.status === 'unavailable') {
    return (
      <p className="rounded-xl border border-dashed border-border-strong bg-surface-2 p-3 text-[12px] text-text-dim">
        Tape outcomes are no longer available for this run.
      </p>
    )
  }

  if (state.status === 'error') {
    return (
      <p role="alert" className="rounded-xl border border-red-line bg-red-bg p-3 text-[12px] text-red">
        {state.message}
      </p>
    )
  }

  if (state.status === 'no-data') {
    return <p className="text-[12px] text-text-faint">No tapes have been loaded by this run yet.</p>
  }

  return (
    <ul className="flex flex-col gap-2">
      {state.tapes.map((tape) => (
        <li
          key={`${tape.tapeIndex}-${tape.copyIndex}-${tape.barcode}`}
          className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-xl border border-border bg-surface p-3 text-[12px] shadow-card"
        >
          <span className="font-mono font-semibold">{tape.barcode}</span>
          <span className="font-mono text-text-faint">tape {tape.tapeIndex}</span>
          <span className="font-mono text-text-faint">copy {tape.copyIndex}</span>
          <span className="font-mono text-text-faint">drive {tape.driveIndex}</span>
          <span className="font-mono text-text-faint">slot {tape.slot}</span>
          <span
            className={`rounded-full border px-2 py-0.5 font-mono text-[11px] font-medium ${resultBadgeClass(tape.result)}`}
          >
            {tape.result}
          </span>

          {tape.writeHealth?.measured && tape.writeHealth.belowFloor ? (
            <span className="inline-flex items-center gap-1 rounded-full border border-amber-line bg-amber-bg px-2 py-0.5 text-[11px] font-medium text-amber">
              <IconWarning className="h-3 w-3" />
              below floor
            </span>
          ) : null}

          {tape.writeHealth?.measured && (tape.writeHealth.tapeAlertFlags?.length ?? 0) > 0 ? (
            <span className="inline-flex items-center gap-1 rounded-full border border-red-line bg-red-bg px-2 py-0.5 text-[11px] font-medium text-red">
              <IconWarning className="h-3 w-3" />
              TapeAlert
            </span>
          ) : null}

          {tape.overwroteNonBlank ? (
            <span className="inline-flex items-center gap-1 rounded-full border border-amber-line bg-amber-bg px-2 py-0.5 text-[11px] font-medium text-amber">
              <IconWarning className="h-3 w-3" />
              overwrote non-blank
            </span>
          ) : null}

          {tape.result === 'failed' && tape.error ? <span className="w-full text-[11px] text-red">{tape.error}</span> : null}
        </li>
      ))}
    </ul>
  )
}

export default TapesSection
