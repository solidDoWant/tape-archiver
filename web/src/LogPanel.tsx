import { useEffect, useRef, useState } from 'react'
import { apiFetch, ApiError, describeNetworkError, formatTimestamp } from './api'

// LogLine mirrors one entry of pkg/runsapi.RunLogsResponse's "lines" array
// (pkg/runsapi/logs.go's LogLine), itself a projection of one matched
// VictoriaLogs record down to what this panel renders. error is the entry's
// error detail when it has one — the actual cause behind a terse message like
// a failing activity's "Activity error." — shown under the message so it is
// visible without opening VictoriaLogs directly.
export interface LogLine {
  time: string
  level?: string
  message: string
  error?: string
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

// catchUpDelayMs is how long after observing a live:false response the
// panel waits before its single final catch-up poll. Log shipping into
// VictoriaLogs is asynchronous and batched (the dev stack's vector tails
// files on an interval; production collectors behave the same way), so the
// poll that first observes live:false can easily race ahead of the run's
// last few lines — typically the final summary or error, the most
// operator-relevant lines of all. One delayed catch-up closes that gap;
// polling a finished window forever would not (nothing else is coming).
const catchUpDelayMs = 5000

// maxPollBackoffMs caps the exponential backoff applied to mid-stream poll
// failures (see the retry handling in LogPanelWindow's effect): 3s → 6s →
// 12s → 24s → 30s cap, reset to the base interval on the next success.
// Backing off keeps a long-lived tab from hammering a server that is down
// anyway, while the cap keeps recovery reasonably prompt once it is back.
const maxPollBackoffMs = 30_000

// maxRenderedLines bounds how many log lines the panel retains and renders. A
// multi-hour run's Write phase streams thousands of lines, and every line
// lives in React state and in the DOM (the console is not virtualized), so
// without a cap memory and paint cost climb until the tab janks. Keeping the
// most recent maxRenderedLines is safe for the tail: the since bound only ever
// advances (it is the newest line's time), so trimmed older lines are never
// re-requested and cannot reappear as duplicates. Older output is dropped from
// the live view — the full history remains in VictoriaLogs.
const maxRenderedLines = 5000

// buildLogsURL constructs the GET /api/runs/{runID}/logs request URL for
// one poll: phase scopes the window (pkg/runsapi/logs.go), since (an
// RFC3339 timestamp — always a prior response's own last line's "time",
// verbatim) asks the server for only the lines from that instant on, so a
// live tail does not re-fetch and re-render the whole window every tick.
// The server treats since as INCLUSIVE (pkg/runsapi's buildLogsQLQuery
// explains why an exclusive bound would permanently drop same-timestamp
// lines split across a poll boundary), so appendNewLines below dedups the
// re-sent boundary lines by time+message identity.
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

// lineKey is a line's dedup identity: its timestamp plus its level and
// message. Two genuinely distinct lines that share all three are
// indistinguishable to this panel anyway (they would render identically),
// so collapsing them loses nothing an operator could act on.
function lineKey(line: LogLine): string {
  // NUL-joined so the delimiter can never collide with log content. error is
  // part of the identity: two lines with the same terse message at the same
  // instant (e.g. "Activity error.") can carry different underlying causes, and
  // dropping it from the key would silently collapse them and hide the second.
  return [line.time, line.level ?? '', line.message, line.error ?? ''].join('\u0000')
}

// appendNewLines appends incoming to existing, dropping any incoming line
// already present. Needed because the server's since bound is inclusive
// (see buildLogsURL's doc comment): each poll re-sends the lines sharing
// the previous batch's final timestamp, so the boundary overlap must be
// deduplicated here — but same-timestamp lines that had NOT yet been
// ingested when the previous poll ran are new, kept, and appended, which is
// the whole point of the inclusive bound.
function appendNewLines(existing: LogLine[], incoming: LogLine[]): LogLine[] {
  if (existing.length === 0) {
    return capLines(incoming)
  }

  const seen = new Set(existing.map(lineKey))
  const fresh = incoming.filter((line) => !seen.has(lineKey(line)))

  return fresh.length === 0 ? existing : capLines([...existing, ...fresh])
}

// capLines keeps only the most recent maxRenderedLines (see that constant),
// returning the input untouched when it is already within bounds so an
// unchanged batch keeps its array identity and does not force a re-render.
function capLines(lines: LogLine[]): LogLine[] {
  return lines.length > maxRenderedLines ? lines.slice(lines.length - maxRenderedLines) : lines
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
// appear without a full page reload; appendNewLines dedups the inclusive
// since boundary). On observing live:false it does not stop immediately:
// log shipping is asynchronous, so it schedules exactly one delayed
// catch-up poll (catchUpDelayMs) to collect any lines still in flight —
// typically the run's final summary/error lines — and only then stops for
// good (unless that catch-up reports live:true again, in which case normal
// polling resumes and a future live:false gets its own catch-up).
//
// A poll failure that happens *after* the panel is already showing real
// lines does not discard them or flip to an error/unavailable state over
// one bad tick — it is treated as transient and retried, mirroring
// pkg/runsapi/events.go's own "log and continue" handling of a mid-stream
// blip, with capped exponential backoff (logPollIntervalMs doubling up to
// maxPollBackoffMs, reset on the next success) so a long-lived tab does not
// hammer a server that is down; only the very first fetch's failure
// surfaces as "error"/"unavailable".
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

  // Autoscroll the console to the newest line as lines arrive, but only while
  // the operator is already pinned to the bottom: if they have scrolled up to
  // read earlier output, new lines must not yank them back down. onScroll
  // keeps pinnedToBottom current; it starts true so the first batch scrolls to
  // the bottom.
  const logContainerRef = useRef<HTMLDivElement>(null)
  const pinnedToBottomRef = useRef(true)

  const readyLines = state.status === 'ready' ? state.lines : null

  useEffect(() => {
    const container = logContainerRef.current
    if (container && pinnedToBottomRef.current) {
      container.scrollTop = container.scrollHeight
    }
  }, [readyLines])

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
    // catchUpDone: whether the single post-live:false catch-up poll (see
    // the component doc comment) has already run for the current live:false
    // streak; reset whenever a response reports live:true again.
    let catchUpDone = false
    // consecutiveFailures drives the transient-failure backoff below.
    let consecutiveFailures = 0

    const scheduleNext = (delayMs: number) => {
      timer = setTimeout(() => {
        void poll()
      }, delayMs)
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

        consecutiveFailures = 0

        setState((previous) => {
          const previousLines = previous.status === 'ready' ? previous.lines : []

          return {
            status: 'ready',
            lines: appendNewLines(previousLines, response.lines),
            live: response.live,
          }
        })

        if (response.live) {
          catchUpDone = false
          scheduleNext(logPollIntervalMs)
        } else if (!catchUpDone) {
          catchUpDone = true
          scheduleNext(catchUpDelayMs)
        }
        // live:false with the catch-up already done: stop for good.
      } catch (error) {
        if (cancelled) {
          return
        }

        if (stateRef.current.status === 'ready') {
          // Transient mid-stream failure: keep what's shown, retry with
          // capped exponential backoff (3s → 6s → 12s → 24s → 30s cap).
          consecutiveFailures += 1
          scheduleNext(Math.min(logPollIntervalMs * 2 ** (consecutiveFailures - 1), maxPollBackoffMs))

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
          className="rounded-lg border border-dashed border-border-strong bg-surface-2 p-3 text-text-dim"
        >
          Logs unavailable — VictoriaLogs is not configured or could not be reached.
        </div>
      ) : null}

      {state.status === 'error' ? (
        <div
          role="alert"
          className="rounded-lg border border-red-line bg-red-bg p-3 text-red"
        >
          {state.message}
        </div>
      ) : null}

      {state.status === 'ready' ? (
        <div className="w-full max-w-full overflow-hidden rounded-xl border border-console-border bg-console-bg shadow-card">
          <div
            ref={logContainerRef}
            onScroll={(event) => {
              const el = event.currentTarget
              // A small tolerance so being within a line's height of the
              // bottom still counts as pinned.
              pinnedToBottomRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24
            }}
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
                  {line.error ? (
                    // The actual cause behind a terse message (e.g. a failing
                    // activity's "Activity error."), indented under it so a long
                    // error wraps readably rather than being lost.
                    <span className="mt-0.5 block pl-4 text-red-300">{line.error}</span>
                  ) : null}
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
