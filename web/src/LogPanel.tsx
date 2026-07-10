import { useEffect, useRef, useState } from 'react'
import { apiFetch, ApiError, describeNetworkError, formatTimestamp } from './api'

// LogLine mirrors one entry of pkg/runsapi.RunLogsResponse's "lines" array
// (pkg/runsapi/logs.go's LogLine), itself a projection of one matched
// VictoriaLogs record down to what this panel renders.
export interface LogLine {
  time: string
  level?: string
  message: string
}

// RunLogsResponse mirrors pkg/runsapi.RunLogsResponse, the GET
// /api/runs/{runID}/logs response body (docs/configuration.md).
interface RunLogsResponse {
  runId: string
  phase?: string
  lines: LogLine[]
  live: boolean
}

export interface LogPanelProps {
  runId: string
  // phase, if given, scopes the panel to one pipeline phase's log window
  // (one of GET /api/runs/{runID}/phases' 11 phase names); omitted, the
  // panel covers the whole run. Reusable both ways — issue #277's run-detail
  // redesign (a per-phase rail) is expected to pass phase per selected
  // phase; this issue's own RunDetail.tsx wiring uses whole-run mode only,
  // since RunDetail has no phase rail yet.
  phase?: string
}

type PanelState =
  | { status: 'loading' }
  | { status: 'unavailable' }
  | { status: 'error'; message: string }
  | { status: 'ready'; lines: LogLine[]; live: boolean }

// logPollIntervalMs is how often LogPanel re-fetches while the most
// recently seen response's "live" field was true (the run, or the
// requested phase, has not finished yet — more lines can still arrive).
//
// This is client-side polling (GET /api/runs/{runID}/logs via api.ts's
// apiFetch), not the Server-Sent Events pattern RunDetail.tsx/
// pkg/runsapi/events.go use for run status elsewhere in this app — a
// deliberate departure, not an oversight: a browser EventSource gives
// calling JS no access to a failed connection's HTTP status code, so an
// SSE-based design could not distinguish pkg/runsapi's 503 "unavailable"
// response (issue #274 AC1/AC2) from an ordinary transient network drop.
// apiFetch's ApiError carries that status cleanly instead, which is what
// makes the "unavailable" state below possible at all.
const logPollIntervalMs = 3000

// buildLogsURL constructs the GET /api/runs/{runID}/logs request URL for
// one poll: phase scopes the window (pkg/runsapi/logs.go), since (an
// RFC3339 timestamp — always a prior response's own last line's "time",
// verbatim) asks the server for only the lines that arrived after it, so a
// live tail does not re-fetch and re-render the whole window every tick.
function buildLogsURL(runId: string, phase: string | undefined, since: string | undefined): string {
  const params = new URLSearchParams()
  if (phase) {
    params.set('phase', phase)
  }
  if (since) {
    params.set('since', since)
  }

  const query = params.toString()

  return `/api/runs/${encodeURIComponent(runId)}/logs${query ? `?${query}` : ''}`
}

// levelClass maps a log line's level to the console panel's tag color, per
// DESIGN_ANALYSIS.md §3's "colored ok/err/info/fmt/wr tags" — scoped down
// to the levels the logging pipeline actually emits (pkg/logging:
// DEBUG/INFO/WARN/ERROR) rather than the mockup's illustrative free-form
// tags, which are not real fields any current log line carries.
//
// These are fixed (non-theme-varying) Tailwind palette colors, not this
// app's light/dark design tokens (--color-red etc., web/src/design/tokens.css)
// — the console background itself never changes between light and dark app
// theme (DESIGN_ANALYSIS.md §3: "always dark-themed regardless of app
// theme"), so its text colors must not shift either, unlike this app's
// token-driven colors elsewhere, which are intentionally different hexes
// per theme.
function levelClass(level: string | undefined): string {
  switch ((level ?? '').toUpperCase()) {
    case 'ERROR':
      return 'text-red-400'
    case 'WARN':
    case 'WARNING':
      return 'text-amber-400'
    case 'DEBUG':
      return 'text-console-dim'
    default:
      return 'text-green-400'
  }
}

// LogPanel shows the VictoriaLogs-backed console for one run (phase
// omitted) or one pipeline phase (phase given) — GET /api/runs/{runID}/logs
// (pkg/runsapi/logs.go, docs/configuration.md).
//
// States: "loading" until the first fetch resolves; "unavailable" when the
// server reports VictoriaLogs is unconfigured or unreachable (503 — issue
// #274 AC1/AC2 both collapse to this one state, since a client cannot act
// differently on the distinction); "error" for anything else fetching the
// *first* batch (a genuine bug, or cmd/web itself unreachable); "ready"
// showing the matched lines in order, oldest first (AC3) — which may be
// empty (e.g. a phase that has not started yet), rendered as its own empty
// message, never conflated with "unavailable".
//
// While "ready" and the most recent response's live flag was true, it polls
// for new lines every logPollIntervalMs and appends them (AC4: new lines
// appear without a full page reload), stopping once live is false. A poll
// failure that happens *after* the panel is already showing real lines does
// not discard them or flip to an error/unavailable state over one bad
// tick — it is treated as transient and silently retried next interval,
// mirroring pkg/runsapi/events.go's own "log and continue" handling of a
// mid-stream blip; only the very first fetch's failure surfaces as
// "error"/"unavailable".
function LogPanel({ runId, phase }: LogPanelProps) {
  // Keyed on runId/phase: the standard React way to reset a component's
  // whole internal state when an identity-defining prop changes (here,
  // "which window am I polling") is to remount it, not to reach into an
  // effect and call setState directly — the latter is flagged by
  // react-hooks' set-state-in-effect rule, and for good reason (it can
  // cause a visible flash of stale state from the old window before the
  // reset lands). Remounting also means LogPanelWindow's own effect below
  // never needs to distinguish "first run" from "runId/phase changed
  // under me" — every mount is unambiguously a fresh window.
  return <LogPanelWindow key={`${runId}::${phase ?? ''}`} runId={runId} phase={phase} />
}

function LogPanelWindow({ runId, phase }: LogPanelProps) {
  const [state, setState] = useState<PanelState>({ status: 'loading' })
  const stateRef = useRef(state)

  // Mirrors state into a ref purely so the effect below's async poll
  // closures (invoked later, from a setTimeout callback — not during
  // render) can read the *latest* state without retriggering the effect
  // (and therefore restarting the poll loop / losing its pending timer) on
  // every state change. Written only here, in an effect, never during
  // render itself.
  useEffect(() => {
    stateRef.current = state
  }, [state])

  useEffect(() => {
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | undefined

    const scheduleNext = () => {
      timer = setTimeout(() => {
        void poll()
      }, logPollIntervalMs)
    }

    const poll = async () => {
      const current = stateRef.current
      const since =
        current.status === 'ready' && current.lines.length > 0
          ? current.lines[current.lines.length - 1].time
          : undefined

      try {
        const response = await apiFetch<RunLogsResponse>(buildLogsURL(runId, phase, since))
        if (cancelled) {
          return
        }

        setState((previous) => {
          const previousLines = previous.status === 'ready' ? previous.lines : []

          return { status: 'ready', lines: [...previousLines, ...response.lines], live: response.live }
        })

        if (response.live) {
          scheduleNext()
        }
      } catch (error) {
        if (cancelled) {
          return
        }

        if (stateRef.current.status === 'ready') {
          scheduleNext()

          return
        }

        if (error instanceof ApiError && error.status === 503) {
          setState({ status: 'unavailable' })

          return
        }

        const message = error instanceof ApiError ? error.message : describeNetworkError(error)
        setState({ status: 'error', message })
      }
    }

    void poll()

    return () => {
      cancelled = true
      if (timer) {
        clearTimeout(timer)
      }
    }
  }, [runId, phase])

  return (
    <div className="flex flex-col gap-2">
      {state.status === 'loading' ? (
        <p role="status" className="text-text-dim">
          Loading logs…
        </p>
      ) : null}

      {state.status === 'unavailable' ? (
        <div
          role="status"
          className="rounded border border-dashed border-border-strong bg-surface-2 p-3 text-text-dim"
        >
          Logs unavailable — VictoriaLogs is not configured or could not be reached.
        </div>
      ) : null}

      {state.status === 'error' ? (
        <div
          role="alert"
          className="rounded border border-red-600 bg-red-50 p-3 text-red-900 dark:border-red-500 dark:bg-red-950 dark:text-red-100"
        >
          {state.message}
        </div>
      ) : null}

      {state.status === 'ready' ? (
        <div className="w-full max-w-full overflow-hidden rounded-lg border border-console-border bg-console-bg shadow-card">
          <div
            role="log"
            className="max-h-80 overflow-y-auto overflow-x-hidden p-3 font-mono text-[11.5px] leading-[1.95] text-console-text"
          >
            {state.lines.length === 0 ? (
              <p className="text-console-dim">
                {state.live ? 'No log lines yet.' : 'No log lines for this window.'}
              </p>
            ) : (
              state.lines.map((line, index) => (
                <div key={`${line.time}-${index}`} className="whitespace-pre-wrap break-words">
                  <span className="text-console-dim">{formatTimestamp(line.time)}</span>{' '}
                  {line.level ? <span className={levelClass(line.level)}>[{line.level}]</span> : null}{' '}
                  <span>{line.message}</span>
                </div>
              ))
            )}
          </div>
        </div>
      ) : null}
    </div>
  )
}

export default LogPanel
