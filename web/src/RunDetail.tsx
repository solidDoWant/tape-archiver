import { useEffect, useState } from 'react'
import { apiFetch, ApiError, describeNetworkError, formatTimestamp } from './api'
import { Link } from './router'
import PhaseRail from './PhaseRail'
import PhaseDetail from './PhaseDetail'
import RunOverview from './RunOverview'
import PauseActions, { type CurrentPauseInfo } from './PauseActions'
import { useRunEvents, type RunEventDetail } from './runEvents'

// RunEventDetail's definition lives in runEvents.ts alongside the shared
// useRunEvents hook (issue #276's dashboard factored the SSE subscription
// out of this file); re-exported here so this page's own satellite modules
// (RunOverview.tsx) and tests keep one import site for the page's types.
export type { RunEventDetail } from './runEvents'

// PhaseStatus mirrors pkg/runsapi.PhaseStatus (phases.go).
export type PhaseStatus = 'pending' | 'active' | 'completed' | 'failed'

// PhaseFact mirrors pkg/runsapi.PhaseFact.
export interface PhaseFact {
  key: string
  label: string
  value: string
}

// PhaseInfo mirrors pkg/runsapi.PhaseInfo, one entry of GET
// /api/runs/{runID}/phases' 11-phase timeline, in the exact pipeline order
// workflows/backup's backupPhases() runs them (Resolve, Prepare, Pack,
// "Generate PAR2", Verify, Load, Write, Eject, Report, Burn, Deliver —
// issue #277's technical context: this code order, not the design mock's,
// is authoritative).
export interface PhaseInfo {
  name: string
  status: PhaseStatus
  startTime?: string
  endTime?: string
  facts: PhaseFact[]
  error?: string
}

interface RunPhasesResponse {
  runId: string
  phases: PhaseInfo[]
}

export interface RunDetailProps {
  runId: string
}

// normalizePauseInfo fills in a safe "not paused" default when a response
// omits currentPause — defensive against a malformed/unexpected frame (real
// pkg/runsapi responses always include it), matching this file's previous
// stance of never letting one bad frame crash the page.
function normalizePauseInfo(pause: CurrentPauseInfo | undefined): CurrentPauseInfo {
  return pause ?? { kind: '' }
}

type InitialState =
  | { status: 'loading' }
  | { status: 'not-found' }
  | { status: 'error'; message: string }
  | { status: 'ready'; detail: RunEventDetail }

// RunDetail is the redesigned `/runs/{id}` page (issue #277): a phase rail
// (PhaseRail) plus a detail pane that shows either the run overview
// (RunOverview) or one selected phase's facts/logs (PhaseDetail), fed by a
// mix of a one-shot existence check, a live SSE stream for status/phase/
// pause, and the history-derived GET /api/runs/{runID}/phases timeline.
//
// The existence check (a plain GET /api/runs/{runID} before anything else)
// exists because the live stream below cannot: a browser EventSource gives
// calling JS no access to a failed connection's HTTP status code (the same
// reason LogPanel.tsx polls instead of using SSE for its own unavailable
// detection), so distinguishing "this run does not exist" (404, issue #277
// AC7's "not-found" state) from "the connection dropped" would be
// impossible from the stream alone. Only once existence is confirmed does
// this component open the live stream and fetch the phase timeline.
//
// This component relies on being remounted for a new runId (App.tsx keys
// <RunDetail> on the route's run ID, same rationale as the pre-redesign
// version) rather than resetting `initial` back to "loading" itself from
// inside the effect on every runId change — an effect synchronously calling
// setState at its own start is the exact anti-pattern
// react-hooks/set-state-in-effect flags, and "remount to reset" is what
// App.tsx's key does instead.
function RunDetail({ runId }: RunDetailProps) {
  const [initial, setInitial] = useState<InitialState>({ status: 'loading' })

  useEffect(() => {
    let cancelled = false

    apiFetch<RunEventDetail>(`/api/runs/${encodeURIComponent(runId)}`)
      .then((detail) => {
        if (!cancelled) {
          setInitial({ status: 'ready', detail: { ...detail, currentPause: normalizePauseInfo(detail.currentPause) } })
        }
      })
      .catch((error: unknown) => {
        if (cancelled) {
          return
        }

        if (error instanceof ApiError && error.status === 404) {
          setInitial({ status: 'not-found' })

          return
        }

        const message = error instanceof ApiError ? error.message : describeNetworkError(error)
        setInitial({ status: 'error', message })
      })

    return () => {
      cancelled = true
    }
  }, [runId])

  return (
    <div className="flex min-w-0 flex-1 flex-col">
      {/* The run name is shown by the app shell's page-title header (App.tsx),
          the same one every page uses — no second title bar here, matching the
          dashboard and config pages. */}
      {initial.status === 'loading' ? (
        <p role="status" className="p-5 text-[12px] text-text-faint sm:p-7">
          Loading run…
        </p>
      ) : null}

      {initial.status === 'not-found' ? <NotFoundNotice runId={runId} /> : null}

      {initial.status === 'error' ? <ErrorNotice message={initial.message} /> : null}

      {initial.status === 'ready' ? <RunDetailLive runId={runId} initialDetail={initial.detail} /> : null}
    </div>
  )
}

type PhasesState =
  | { status: 'loading' }
  | { status: 'aged-out' }
  | { status: 'degraded'; message?: string }
  | { status: 'ready'; phases: PhaseInfo[] }

// RunDetailLive owns the live half of the page: the shared useRunEvents SSE
// subscription (runEvents.ts — issue #276 factored it out of this file, and
// the dashboard's current-run card shares it) for status/phase/pause, the
// phase-rail selection state, and the GET /api/runs/{runID}/phases
// timeline — refetched whenever a fresh SSE frame arrives (the effect below
// keys on the frame's object identity: useRunEvents stores a new detail
// object per update/done event) so the rail's per-phase statuses stay
// current (issue #277's "refresh on SSE updates"), without resetting to a
// loading flicker on every tick (only the very first fetch shows "loading";
// a background refetch silently replaces the phases once it resolves).
function RunDetailLive({ runId, initialDetail }: { runId: string; initialDetail: RunEventDetail }) {
  const { state: connection, detail: liveDetail } = useRunEvents(runId)
  const [selected, setSelected] = useState<string>('overview')
  const [phases, setPhases] = useState<PhasesState>({ status: 'loading' })

  // The latest SSE frame once one has arrived, else the parent's existence-
  // check snapshot; currentPause normalized defensively either way (the
  // parent already normalized initialDetail's).
  const detail: RunEventDetail = liveDetail
    ? { ...liveDetail, currentPause: normalizePauseInfo(liveDetail.currentPause) }
    : initialDetail

  useEffect(() => {
    let cancelled = false

    apiFetch<RunPhasesResponse>(`/api/runs/${encodeURIComponent(runId)}/phases`)
      .then((response) => {
        if (cancelled) {
          return
        }

        setPhases({
          status: 'ready',
          phases: response.phases.map((phase) => ({ ...phase, facts: phase.facts ?? [] })),
        })
      })
      .catch((error: unknown) => {
        if (cancelled) {
          return
        }

        setPhases((previous) => {
          // A failed refetch after the rail has already loaded keeps the
          // current (slightly stale) timeline rather than clobbering a
          // healthy rail into a degraded state — the same keep-and-retry
          // stance LogPanel.tsx takes for a mid-stream poll blip. This
          // matters most for the refetch the terminal "done" event
          // triggers: the SSE stream is closed at that point, so a
          // degraded state reached there would be permanent, over data
          // the page had already successfully shown. The next SSE event
          // (if any) bumps refreshKey and retries naturally. Only the
          // very first load's failure decides aged-out/degraded below.
          if (previous.status === 'ready') {
            return previous
          }

          if (error instanceof ApiError && error.status === 410) {
            return { status: 'aged-out' }
          }

          if (error instanceof ApiError && error.status === 404) {
            // This run's own existence was already confirmed by RunDetail's
            // initial fetch, so a 404 here specifically is an unexpected
            // inconsistency rather than a genuinely missing run — degrade
            // honestly to the basic status view instead of a misleading
            // "not found" (issue #277's phases-endpoint fallback).
            return { status: 'degraded' }
          }

          const message = error instanceof ApiError ? error.message : describeNetworkError(error)

          return { status: 'degraded', message }
        })
      })

    return () => {
      cancelled = true
    }
    // liveDetail deliberately included: useRunEvents stores a brand-new
    // detail object per SSE update/done event, so its identity changing is
    // exactly "a fresh frame arrived" — the phase timeline is re-derived
    // from the latest workflow history as the run progresses, not just once
    // at mount.
  }, [runId, liveDetail])

  // terminal: closeTime is authoritative (present the moment the initial
  // fetch or any SSE frame reports the run closed) and does not require
  // waiting for this connection's own "done" event, which matters for a run
  // that was ALREADY closed when this page was first opened (a historical
  // run) — connection stays 'connecting'/'live' briefly in that case, but
  // detail.closeTime is already set from the initial fetch.
  const terminal = Boolean(detail.closeTime) || connection === 'terminal'

  const selectedPhaseIndex = phases.status === 'ready' ? phases.phases.findIndex((phase) => phase.name === selected) : -1
  const selectedPhase = selectedPhaseIndex >= 0 && phases.status === 'ready' ? phases.phases[selectedPhaseIndex] : undefined

  return (
    <div className="flex min-w-0 flex-1 flex-col">
      {connection === 'error' ? (
        <div role="alert" className="mx-4 mt-3 rounded-lg border border-amber-line bg-amber-bg px-3.5 py-2 text-[12px] text-amber sm:mx-6">
          Live updates disconnected — this page will keep retrying automatically.
        </div>
      ) : null}

      {terminal ? (
        <div className="mx-4 mt-3 flex flex-wrap items-center gap-3 rounded-lg border border-border bg-surface-2 px-3.5 py-2 sm:mx-6">
          <span className="rounded-full border border-border bg-inset px-2 py-0.5 font-mono text-[11px] font-semibold text-text-dim">
            READ-ONLY
          </span>
          <span className="font-mono text-[11px] text-text-dim">
            Viewing a closed run, reconstructed from Temporal history — no live hardware access is performed.
          </span>
        </div>
      ) : null}

      {phases.status === 'loading' ? (
        <p role="status" className="p-5 text-[12px] text-text-faint sm:p-7">
          Loading phases…
        </p>
      ) : phases.status === 'aged-out' ? (
        <AgedOutNotice runId={runId} />
      ) : phases.status === 'ready' ? (
        <div className="flex min-w-0 flex-1 flex-col md:flex-row">
          <PhaseRail phases={phases.phases} selected={selected} onSelect={setSelected} />

          <div className="min-w-0 flex-1 p-5 sm:p-7">
            {selected === 'overview' || !selectedPhase ? (
              <RunOverview runId={runId} detail={detail} phases={phases.phases} terminal={terminal} />
            ) : (
              <PhaseDetail runId={runId} index={selectedPhaseIndex + 1} phase={selectedPhase} terminal={terminal} />
            )}
          </div>
        </div>
      ) : (
        <DegradedNotice runId={runId} detail={detail} message={phases.status === 'degraded' ? phases.message : undefined} />
      )}
    </div>
  )
}

// NotFoundNotice is issue #277 AC7's not-found state: a run ID Temporal has
// no record of at all, distinct from AgedOutNotice below (a run that DID
// exist but whose history has since aged out).
function NotFoundNotice({ runId }: { runId: string }) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-3 px-6 py-16 text-center">
      <div className="flex h-14 w-14 items-center justify-center rounded-2xl border border-border bg-surface-2 text-2xl text-text-faint">
        ≣
      </div>
      <h2 className="text-lg font-semibold">No run named {runId}</h2>
      <p className="max-w-[420px] text-[13px] text-text-dim">
        Temporal has no record of this run — check the run ID, or it may never have been submitted.
      </p>
      <Link
        to="/"
        className="mt-1 rounded-lg border border-border-strong bg-surface px-4 py-2 text-[12.5px] font-medium transition-colors hover:bg-surface-2"
      >
        Back to runs
      </Link>
    </div>
  )
}

// AgedOutNotice is issue #277 AC7's aged-out state: this run ID exists (it
// is still in Temporal's visibility index — RunDetail's own initial fetch
// above proved that), but its event history has fallen out of Temporal's
// retention window, so GET /api/runs/{runID}/phases (and /config, /tapes)
// report 410 Gone — phases/contents can no longer be reconstructed at all,
// distinct from NotFoundNotice's "never existed" case.
function AgedOutNotice({ runId }: { runId: string }) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-3 px-6 py-16 text-center">
      <div className="flex h-14 w-14 items-center justify-center rounded-2xl border border-border bg-surface-2 text-2xl text-text-faint">
        ▤
      </div>
      <h2 className="text-lg font-semibold">{runId} has aged out of history</h2>
      <p className="max-w-[440px] text-[13px] text-text-dim">
        This run is older than Temporal's retention window, so its phases and contents can no longer be
        reconstructed.
      </p>
      <Link
        to="/"
        className="mt-1 rounded-lg border border-border-strong bg-surface px-4 py-2 text-[12.5px] font-medium transition-colors hover:bg-surface-2"
      >
        Back to runs
      </Link>
    </div>
  )
}

// DegradedNotice is the honest fallback for GET /api/runs/{runID}/phases
// failing in a way that is neither aged-out nor not-found (issue #277's
// "phases endpoint 410/404 → honest fallback to the basic status view"): the
// page still shows whatever it already has — status, timing, and pause
// controls (all sourced independently of the phases endpoint) — with a
// visible note that phase-by-phase detail could not be loaded, rather than
// a blank or broken page.
function DegradedNotice({ runId, detail, message }: { runId: string; detail: RunEventDetail; message?: string }) {
  return (
    <div className="flex max-w-xl flex-col gap-4 p-5 sm:p-7">
      <p role="alert" className="rounded-lg border border-dashed border-border-strong bg-surface-2 p-3 text-[12px] text-text-dim">
        {message ?? 'Phase-by-phase detail is unavailable for this run right now.'}
      </p>

      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-[12.5px]">
        <dt className="text-text-dim">Status</dt>
        <dd>{detail.status}</dd>
        <dt className="text-text-dim">Last completed phase</dt>
        <dd>{detail.lastCompletedPhase || '—'}</dd>
        <dt className="text-text-dim">Started</dt>
        <dd>{formatTimestamp(detail.startTime)}</dd>
        {detail.closeTime ? (
          <>
            <dt className="text-text-dim">Closed</dt>
            <dd>{formatTimestamp(detail.closeTime)}</dd>
          </>
        ) : null}
      </dl>

      {detail.currentPause.kind !== '' || detail.currentPause.unknown ? (
        <PauseActions runId={runId} pause={detail.currentPause} />
      ) : null}
    </div>
  )
}

// ErrorNotice is a generic fetch failure on RunDetail's own initial
// existence check (network failure, an upstream 5xx, etc.) — distinct from
// "not found" (a clean 404).
function ErrorNotice({ message }: { message: string }) {
  return (
    <p role="alert" className="m-5 rounded-lg border border-red-line bg-red-bg p-3 text-[12px] text-red sm:m-7">
      {message}
    </p>
  )
}

export default RunDetail
