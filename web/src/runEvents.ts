import { useEffect, useState } from 'react'
import { apiFetch } from './api'
import type { CurrentPauseInfo } from './PauseActions'

// RunEventDetail mirrors pkg/runsapi.RunDetail's JSON shape, as carried by
// both the "update" and "done" Server-Sent Events GET
// /api/events/runs/{runID} emits (pkg/runsapi/events.go).
export interface RunEventDetail {
  workflowId: string
  runId: string
  status: string
  startTime: string
  closeTime?: string
  // dryRun mirrors RunSummary.dryRun (this detail embeds the summary
  // server-side): true when the run was submitted as a dry-run.
  dryRun: boolean
  lastCompletedPhase: string
  // currentPause is which operator-in-the-loop pause (if any) is blocking
  // this run right now (backup.CurrentPauseQuery via pkg/runsapi). The SSE
  // poll loop behind this stream compares it on every tick alongside status/
  // phase (pkg/runsapi/events.go), so a pause starting or clearing (e.g.
  // after PauseActions sends a resume/abort) shows up here live, without a
  // manual refresh.
  currentPause: CurrentPauseInfo
}

export type RunConnectionState = 'connecting' | 'live' | 'terminal' | 'error'

export interface RunEventsState {
  state: RunConnectionState
  detail: RunEventDetail | null
}

// useRunEvents subscribes to GET /api/events/runs/{runID} via EventSource —
// cookies are sent automatically for a same-origin request, so this works
// transparently behind pkg/webauth's session gate the same way any other
// fetch does; no separate auth handling is needed here. It is the shared
// live-run-state subscription behind both RunDetail.tsx (the run's own
// detail page) and Dashboard.tsx's CurrentRunCard (issue #276) — the two
// need identical status/phase/pause semantics, so this is factored out here
// rather than each reimplementing the same event parsing/state machine.
//
// States: "connecting" until the first event arrives, "live" showing the
// current status/phase and updating in place as further "update" events
// arrive, "terminal" once the server sends its final "done" event (the run
// reached a status other than RUNNING), and "error" if the underlying
// connection drops before that — EventSource retries automatically on its
// own, so an "error" state can recover back to "live" if the retry
// succeeds; it does not recover once "terminal" has been reached, since the
// stream is explicitly closed at that point and not reopened.
//
// runId may be null/empty to mean "no run to watch yet" (e.g. Dashboard
// before it knows whether any run is currently active): the hook then opens
// no connection and returns the initial "connecting"/null state forever,
// until called again with a real ID.
//
// When runId changes, the hook resets to "connecting"/null before the new
// connection's first event arrives, so a caller that swaps the watched run in
// place — Dashboard's CurrentRunCard, when the active run flips from a just-
// finished run to a fresh one (activeRun.ts) — never renders the new run
// carrying the previous run's terminal status/phase/pause. (RunDetail avoids
// this a different way: App.tsx keys it on runId, remounting a fresh hook per
// run, so its runId never changes within one mount.) The reset happens during
// render via the previous-value ref below, not in an effect, so there is no
// intermediate commit showing the stale run's state.
export function useRunEvents(runId: string | null | undefined): RunEventsState {
  const [state, setState] = useState<RunConnectionState>('connecting')
  const [detail, setDetail] = useState<RunEventDetail | null>(null)

  // Reset display state the instant runId changes, in render, so the stale
  // previous run's status/detail is never committed to the screen under the new
  // run's ID. This is React's supported "adjusting state when a prop changes"
  // pattern (previous value tracked in state, compared during render): it
  // re-renders immediately without painting the stale values.
  const [previousRunId, setPreviousRunId] = useState(runId)
  if (previousRunId !== runId) {
    setPreviousRunId(runId)
    setState('connecting')
    setDetail(null)
  }

  useEffect(() => {
    if (!runId) {
      return
    }

    let cancelled = false
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
      if (!parsed) {
        // A malformed frame carries no state to show; leave the current state
        // (e.g. 'connecting' on the very first frame) rather than advancing to
        // 'live' with a null detail. The next well-formed frame moves us on.
        return
      }
      setDetail(parsed)
      setState('live')
    }

    // terminalReached records that the server sent its final "done" event and
    // the stream was closed on purpose, so handleError below can tell an
    // expected post-terminal drop from a real mid-stream one and skip both the
    // 'error' state and the /api/me probe for it.
    let terminalReached = false

    const handleDone = (event: MessageEvent<string>) => {
      const parsed = parseDetail(event)
      if (parsed) {
        setDetail(parsed)
      }
      setState('terminal')
      terminalReached = true
      source.close()
    }

    // sessionProbeInFlight guards handleError's follow-up /api/me probe
    // below against firing a second overlapping probe for every retry
    // EventSource attempts on its own (it retries automatically; a real
    // outage can mean several "error" events in a row before this effect's
    // cleanup ever runs).
    let sessionProbeInFlight = false

    const handleError = () => {
      // A dropped connection while already terminal is expected (the server
      // closed it on purpose after "done") and must not overwrite the
      // terminal state with an error.
      setState((current) => (current === 'terminal' ? current : 'error'))

      // EventSource cannot see the HTTP status of a failed connection (its
      // 'error' event carries no status code), so a dropped session and a
      // transient network blip look identical here. Disambiguate with a
      // follow-up authenticated fetch (issue #285): apiFetch itself already
      // treats a 401 as authoritative session loss and notifies
      // identity.ts's subscription (api.ts's onSessionExpired), which
      // routes the app back to the login page — this probe only needs to
      // fire that path, not act on its own result. Any other outcome
      // (success, or a non-401 failure such as the server being briefly
      // unreachable) is left alone: EventSource's own automatic
      // reconnection handles recovering from those without this hook doing
      // anything further.
      //
      // Skip the probe entirely once terminal: the server closes the stream on
      // purpose after "done", so that drop is expected and carries no session
      // signal — probing on it would be a wasted authenticated request.
      if (cancelled || terminalReached || sessionProbeInFlight) {
        return
      }

      sessionProbeInFlight = true
      void apiFetch('/api/me')
        .catch(() => {
          // Intentionally ignored: a 401 already triggered the session-loss
          // path as a side effect inside apiFetch, and any other failure
          // (network-level) is not session loss — see comment above.
        })
        .finally(() => {
          sessionProbeInFlight = false
        })
    }

    source.addEventListener('update', handleUpdate)
    source.addEventListener('done', handleDone)
    source.addEventListener('error', handleError)

    return () => {
      cancelled = true
      source.close()
    }
  }, [runId])

  return { state, detail }
}
