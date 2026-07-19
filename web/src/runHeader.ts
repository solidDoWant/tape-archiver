// runHeader.ts holds the run page's status/runtime model plus the tiny context
// that lets the app-shell header (App.tsx) surface it. The run page has a single
// header — the shell's page-title bar (RunDetail.tsx no longer renders its own).
// The design's header (`.claude/tasks/design-redesign` mockup) also shows, for a
// run, the run's status pill and a runtime line; RunDetail can't render those in
// the shell header directly (the header is the shell's, the data is RunDetail's),
// so RunDetail *publishes* them here and the shell header consumes them.
//
// This carries no JSX (the RunHeaderProvider/RunStatusPill/ShellHeader components
// live in App.tsx): a component-free module keeps fast-refresh happy, the same
// split route.ts/router.tsx and phaseFormat.ts/PhaseRail.tsx use.

import { createContext, useContext, useEffect } from 'react'
import { formatDuration } from './phaseFormat'

// RunHeaderTone is the shared status palette key used by both the run overview's
// hero badge (RunOverview.tsx) and the shell header's status pill (App.tsx), so
// the two never disagree on a run's colour.
export type RunHeaderTone = 'running' | 'paused' | 'complete' | 'failed' | 'neutral'

// RunHeaderInfo is what RunDetail publishes for the shell header to show.
export interface RunHeaderInfo {
  statusLabel: string
  tone: RunHeaderTone
  runtime: string
}

const toneBadgeClass: Record<RunHeaderTone, string> = {
  running: 'text-blue',
  paused: 'text-amber',
  complete: 'text-green',
  failed: 'text-red',
  neutral: 'text-text-dim',
}

// pillToneClass/pillDotClass style the shell header's status pill per tone. blue
// has no `-line` border token (tokens.css defines --blue/--blue-bg only), so the
// running pill uses the same opacity-modified border TapesPage.tsx does.
export const pillToneClass: Record<RunHeaderTone, string> = {
  running: 'border-blue/30 bg-blue-bg text-blue',
  paused: 'border-amber-line bg-amber-bg text-amber',
  complete: 'border-green-line bg-green-bg text-green',
  failed: 'border-red-line bg-red-bg text-red',
  neutral: 'border-border bg-inset text-text-dim',
}

export const pillDotClass: Record<RunHeaderTone, string> = {
  running: 'bg-blue',
  paused: 'bg-amber',
  complete: 'bg-green',
  failed: 'bg-red',
  neutral: 'bg-text-dim',
}

export interface RunStatusView {
  label: string
  title: string
  tone: RunHeaderTone
  badgeClass: string
}

// livePauseState collapses a run's reported pause to what the operator can
// actually act on right now. A *closed* run is never "currently" paused: a run
// terminated (or completed) while waiting at a pause still reports its last
// pause kind in currentPause, but Resume/Abort no longer apply, so it must not
// read as PAUSED or show the operator-action card. Both the run overview (hero +
// pause zone) and the shell header derive isPaused/pauseUnknown through this, so
// they can never disagree on whether a closed run is paused — the omission of
// this guard in one place but not the other is exactly what left a terminated
// run showing "Operator action required".
export function livePauseState(
  pause: { kind: string; unknown?: boolean },
  terminal: boolean,
): { isPaused: boolean; pauseUnknown: boolean } {
  if (terminal) {
    return { isPaused: false, pauseUnknown: false }
  }

  return { isPaused: pause.kind !== '', pauseUnknown: Boolean(pause.unknown) }
}

// runStatusView maps a run's raw status (+ pause state) to the operator-facing
// label, hero title, tone, and badge colour class. Single source for both the
// RunOverview hero and the shell header pill, so a status can never read one way
// in the header and another in the page body. Order matters: a confirmed pause
// wins, but a failed pause query (unknown) must not assert "paused" — the run
// may or may not be waiting on an operator, so it states the uncertainty and the
// pause zone renders the full warning (RunOverview.tsx's original note).
export function runStatusView(status: string, paused: boolean, pauseUnknown: boolean): RunStatusView {
  const base = ((): { label: string; title: string; tone: RunHeaderTone } => {
    if (paused) {
      return { label: 'PAUSED', title: 'Backup paused', tone: 'paused' }
    }

    if (pauseUnknown) {
      return { label: 'PAUSE STATUS UNKNOWN', title: 'Backup in progress', tone: 'paused' }
    }

    switch (status) {
      case 'Running':
        return { label: 'RUNNING', title: 'Backup in progress', tone: 'running' }
      case 'Completed':
        return { label: 'COMPLETE', title: 'Backup completed', tone: 'complete' }
      case 'Failed':
      case 'Terminated':
      case 'TimedOut':
        return { label: status.toUpperCase(), title: 'Backup failed', tone: 'failed' }
      case 'Canceled':
        return { label: 'CANCELED', title: 'Backup canceled', tone: 'neutral' }
      default:
        return { label: status.toUpperCase(), title: status, tone: 'neutral' }
    }
  })()

  return { ...base, badgeClass: toneBadgeClass[base.tone] }
}

// headerRuntime renders the run's runtime line for the shell header — "started
// 2h 41m ago" while running, "paused · 2h 41m in" while paused, "ran 5h 14m"
// once closed — mirroring the design mockup's runMeta. Empty when there is no
// start time yet to measure from (formatDuration returns an em dash), so the
// header just shows the title until the run has actually begun.
export function headerRuntime(
  startTime: string | undefined,
  closeTime: string | undefined,
  paused: boolean,
  terminal: boolean,
): string {
  // A closed run with no recorded close time yet (a brief window where the
  // status is terminal but the projected CloseTime has not populated) must not
  // show a runtime that counts up as if still running: formatDuration(start,
  // undefined) is the live "elapsed until now" branch, so "ran 2h 1m" would tick
  // upward for a run that has already ended. Wait for closeTime instead.
  if (terminal && !closeTime) {
    return ''
  }

  const elapsed = formatDuration(startTime, closeTime)
  if (elapsed === '—') {
    return ''
  }

  if (terminal) {
    return `ran ${elapsed}`
  }

  if (paused) {
    return `paused · ${elapsed} in`
  }

  return `started ${elapsed} ago`
}

// The header state is split across two contexts so a publishing page (RunDetail,
// via usePublishRunHeader) subscribes only to the stable setter and never
// re-renders when the info it just set flows back out to the header consumer.
export const RunHeaderStateContext = createContext<RunHeaderInfo | null>(null)
export const RunHeaderSetContext = createContext<((info: RunHeaderInfo | null) => void) | null>(null)

// useRunHeaderInfo reads the current run-header info (null on every non-run page,
// and on a run page until its detail has loaded) for the shell header to render.
export function useRunHeaderInfo(): RunHeaderInfo | null {
  return useContext(RunHeaderStateContext)
}

// usePublishRunHeader publishes info to the shell header while mounted and clears
// it on unmount, so navigating away from a run leaves other pages' headers plain.
// A no-op outside a provider (isolated component tests that don't wrap one).
export function usePublishRunHeader(info: RunHeaderInfo | null): void {
  const setInfo = useContext(RunHeaderSetContext)
  const present = info !== null
  const statusLabel = info?.statusLabel ?? ''
  const tone = info?.tone ?? 'neutral'
  const runtime = info?.runtime ?? ''

  useEffect(() => {
    if (!setInfo) {
      return
    }

    setInfo(present ? { statusLabel, tone, runtime } : null)

    return () => setInfo(null)
  }, [setInfo, present, statusLabel, tone, runtime])
}
