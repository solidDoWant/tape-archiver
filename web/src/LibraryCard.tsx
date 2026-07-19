import { useEffect, useState } from 'react'
import { apiFetch, ApiError, describeNetworkError } from './api'
import { IconWarning } from './icons'

// TapeOutcome/AggregateTapeOutcome/RunError/AggregateTapesResponse mirror
// pkg/runsapi's GET /api/tapes JSON shapes (tapes.go) — the same
// history-derived, per-run-degrading aggregate the (future) Tapes page will
// use, reused here for a much smaller summary. Only the fields this card
// actually renders are declared.
interface AggregateTapeOutcome {
  barcode: string
  result: string
  runId: string
  runStartTime: string
}

interface RunError {
  runId: string
  error: string
}

interface AggregateTapesResponse {
  tapes: AggregateTapeOutcome[]
  runErrors?: RunError[]
}

type LoadState =
  | { status: 'loading' }
  | { status: 'error'; error: string }
  | { status: 'loaded'; tapes: AggregateTapeOutcome[]; runErrors: RunError[] }

// resultLabel/resultClass render one of tapes.go's three outcome values as a
// short operator-facing label and color.
const resultOrder = ['written', 'failed', 'loaded'] as const

function resultLabel(result: (typeof resultOrder)[number]): string {
  switch (result) {
    case 'written':
      return 'Written'
    case 'failed':
      return 'Failed'
    case 'loaded':
      return 'In progress'
  }
}

function resultClass(result: (typeof resultOrder)[number]): string {
  switch (result) {
    case 'written':
      return 'text-green'
    case 'failed':
      return 'text-red'
    case 'loaded':
      return 'text-amber'
  }
}

// LibraryCard is the dashboard's library summary (issue #276 AC7,
// DESIGN_ANALYSIS.md §2.A #3). The epic (#271) explicitly descopes live
// drive/slot-occupancy — no SCSI element-status source exists anywhere in
// this stack yet — so, unlike the design mock's DRIVE 0/DRIVE 1/I/O STATION
// live occupancy view, this card must never claim to show the library's
// current physical state. What it *can* show honestly is a summary of tape
// outcomes reconstructed from recent run history (GET /api/tapes, the same
// history-derived, per-run-degrading endpoint issue #273 built for the
// Tapes page) — always labeled as derived from history, not live, per
// DESIGN_ANALYSIS.md §4's warning against ever implying live SCSI state.
function LibraryCard() {
  const [state, setState] = useState<LoadState>({ status: 'loading' })

  useEffect(() => {
    let cancelled = false

    async function load() {
      try {
        const response = await apiFetch<AggregateTapesResponse>('/api/tapes')
        if (!cancelled) {
          setState({ status: 'loaded', tapes: response.tapes, runErrors: response.runErrors ?? [] })
        }
      } catch (error) {
        if (!cancelled) {
          const message = error instanceof ApiError ? error.message : describeNetworkError(error)
          setState({ status: 'error', error: message })
        }
      }
    }

    void load()

    return () => {
      cancelled = true
    }
  }, [])

  return (
    <div className="rounded-xl border border-border bg-surface p-5 shadow-card">
      <div className="mb-3.5 text-[12.5px] font-semibold">Library</div>

      <div className="mb-3.5 flex items-start gap-2 rounded-lg border border-dashed border-border-strong bg-surface-2 p-2.5 text-[11.5px] text-text-faint">
        <IconWarning className="mt-0.5 h-3.5 w-3.5 flex-none" />
        <span>
          Live drive and slot occupancy is not available — no SCSI element-status source is wired up yet. The
          summary below is reconstructed from recent run history, not the library's current physical state.
        </span>
      </div>

      {state.status === 'loading' ? (
        <p role="status" className="text-[12.5px] text-text-faint">
          Loading tape history…
        </p>
      ) : null}

      {state.status === 'error' ? (
        <p role="alert" className="text-[12.5px] text-red">
          {state.error}
        </p>
      ) : null}

      {state.status === 'loaded' && state.tapes.length === 0 && state.runErrors.length === 0 ? (
        <p className="text-[12.5px] text-text-faint">No tapes recorded in run history yet.</p>
      ) : null}

      {state.status === 'loaded' && state.tapes.length > 0 ? (
        <div className="grid grid-cols-[repeat(auto-fit,minmax(120px,1fr))] gap-4">
          {resultOrder.map((result) => {
            const count = state.tapes.filter((tape) => tape.result === result).length

            return (
              <div key={result}>
                <div className="mb-1 font-mono text-[11px] text-text-faint">{resultLabel(result).toUpperCase()}</div>
                <div className={`font-mono text-[15px] font-semibold ${resultClass(result)}`}>{count}</div>
              </div>
            )
          })}
        </div>
      ) : null}

      {state.status === 'loaded' && state.runErrors.length > 0 ? (
        <p className="mt-3 text-[11px] text-text-faint">
          {state.runErrors.length} run{state.runErrors.length === 1 ? '' : 's'} could not be reconstructed and{' '}
          {state.runErrors.length === 1 ? 'is' : 'are'} not reflected above.
        </p>
      ) : null}
    </div>
  )
}

export default LibraryCard
