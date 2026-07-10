import { useEffect, useState } from 'react'
import { formatTimestamp } from './api'
import PauseActions, { type CurrentPauseInfo } from './PauseActions'
import DriveMetricsPanel from './DriveMetricsPanel'

// RunEventDetail mirrors pkg/runsapi.RunDetail's JSON shape, as carried by
// both the "update" and "done" Server-Sent Events GET
// /api/events/runs/{runID} emits (pkg/runsapi/events.go).
export interface RunEventDetail {
  workflowId: string
  runId: string
  status: string
  startTime: string
  closeTime?: string
  lastCompletedPhase: string
  // currentPause is which operator-in-the-loop pause (if any) is blocking
  // this run right now (backup.CurrentPauseQuery via pkg/runsapi). The SSE
  // poll loop behind this stream compares it on every tick alongside status/
  // phase (pkg/runsapi/events.go), so a pause starting or clearing (e.g.
  // after PauseActions below sends a resume/abort) shows up here live,
  // without a manual refresh.
  currentPause: CurrentPauseInfo
}

type ConnectionState = 'connecting' | 'live' | 'terminal' | 'error'

export interface RunDetailProps {
  runId: string
}

// RunDetail shows a single run's live status/phase, fed by GET
// /api/events/runs/{runID} (pkg/runsapi) via EventSource — cookies are sent
// automatically by EventSource for a same-origin request, so this works
// transparently behind pkg/webauth's session gate the same way any other
// fetch on this page does; no separate auth handling is needed here.
//
// States shown: "connecting" until the first event arrives, "live" showing
// the current status/phase and updating in place as further "update" events
// arrive, "terminal" once the server sends its final "done" event (the run
// reached a status other than RUNNING), and "error" if the underlying
// connection drops before that — EventSource retries automatically on its
// own, so an "error" state can recover back to "live" if the retry
// succeeds; it does not recover once "terminal" has been reached, since the
// stream is explicitly closed at that point and not reopened.
// RunDetail keys its state to the connection it currently holds open: if a
// caller ever renders it with a new runId without remounting it (App.tsx
// avoids this by keying <RunDetail> on the run ID, so a navigation to a
// different run always starts from a fresh mount), the effect below still
// tears down the old EventSource and opens a new one, but the previously
// displayed status/phase intentionally stays on screen until the new
// connection's first event replaces it, rather than being reset to
// "connecting" from inside the effect — an effect synchronously calling
// setState at its own start (rather than from within a subscription
// callback reacting to an external event) is the exact anti-pattern
// react-hooks/set-state-in-effect flags, and the "remount to reset" pattern
// it points at is what App.tsx's key does instead.
//
// This component no longer renders its own "back" link (it used to, to
// whatever view had navigated here) — App.tsx's persistent shell nav
// (sub-issue 7) makes Submit and History reachable from every view,
// including this one, so a special-cased in-page back link is redundant.
function RunDetail({ runId }: RunDetailProps) {
  const [state, setState] = useState<ConnectionState>('connecting')
  const [detail, setDetail] = useState<RunEventDetail | null>(null)

  useEffect(() => {
    const source = new EventSource(`/api/events/runs/${encodeURIComponent(runId)}`)

    const parseDetail = (event: MessageEvent<string>): RunEventDetail | null => {
      try {
        return JSON.parse(event.data) as RunEventDetail
      } catch {
        // A malformed event body is not expected from pkg/runsapi's own
        // encoder, but ignoring it (keeping whatever was last shown) is
        // safer than crashing the whole view over one bad frame.
        return null
      }
    }

    const handleUpdate = (event: MessageEvent<string>) => {
      const parsed = parseDetail(event)
      if (parsed) {
        setDetail(parsed)
      }
      setState('live')
    }

    const handleDone = (event: MessageEvent<string>) => {
      const parsed = parseDetail(event)
      if (parsed) {
        setDetail(parsed)
      }
      setState('terminal')
      source.close()
    }

    const handleError = () => {
      // A dropped connection while already terminal is expected (the server
      // closed it on purpose after "done") and must not overwrite the
      // terminal state with an error.
      setState((current) => (current === 'terminal' ? current : 'error'))
    }

    source.addEventListener('update', handleUpdate)
    source.addEventListener('done', handleDone)
    source.addEventListener('error', handleError)

    return () => {
      source.close()
    }
  }, [runId])

  return (
    <div className="flex w-full max-w-2xl flex-col gap-4 text-left">
      <h2 className="text-xl font-semibold">Run {runId}</h2>

      {state === 'connecting' ? (
        <p role="status" className="text-slate-600 dark:text-slate-400">
          Connecting…
        </p>
      ) : null}

      {state === 'error' ? (
        <p
          role="alert"
          className="rounded border border-amber-600 bg-amber-50 p-3 text-amber-900 dark:border-amber-500 dark:bg-amber-950 dark:text-amber-100"
        >
          Connection lost; the page will keep retrying automatically.
        </p>
      ) : null}

      {detail ? (
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1">
          <dt className="font-medium">Status</dt>
          <dd>{detail.status}</dd>
          <dt className="font-medium">Last completed phase</dt>
          <dd>{detail.lastCompletedPhase || '—'}</dd>
          <dt className="font-medium">Started</dt>
          <dd>{formatTimestamp(detail.startTime)}</dd>
          {detail.closeTime ? (
            <>
              <dt className="font-medium">Closed</dt>
              <dd>{formatTimestamp(detail.closeTime)}</dd>
            </>
          ) : null}
        </dl>
      ) : null}

      {detail ? (
        // Fallback to "not paused" if an older/unexpected event body ever
        // omits currentPause (pkg/runsapi always includes it as of this
        // sub-issue, but this keeps a malformed frame from crashing the
        // view rather than just showing stale data, matching parseDetail's
        // own defensive stance above).
        <PauseActions runId={runId} pause={detail.currentPause ?? { kind: '' }} />
      ) : null}

      {detail ? (
        // Live VictoriaMetrics-backed drive metrics (issue #275): shown
        // whenever the run is being observed, not gated to the Write phase
        // specifically — DriveMetricsPanel itself renders a graceful
        // "no-data" placeholder outside the Write phase (the underlying
        // per-tape metrics are simply empty then), so no phase-aware
        // wiring is needed here. This is a minimal drop-in; issue #277's
        // redesigned run page re-homes it into the fuller Write-phase
        // layout (DESIGN_ANALYSIS.md §3).
        <div>
          <h3 className="mb-1.5 text-sm font-medium text-text-dim">Drive metrics</h3>
          <DriveMetricsPanel runId={runId} />
        </div>
      ) : null}

      {state === 'terminal' ? (
        <p
          role="status"
          className="rounded border border-green-600 bg-green-50 p-3 text-green-900 dark:border-green-500 dark:bg-green-950 dark:text-green-100"
        >
          Run finished.
        </p>
      ) : null}
    </div>
  )
}

export default RunDetail
