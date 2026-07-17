// phaseFormat.ts: small, pure formatting helpers shared by PhaseRail.tsx,
// PhaseDetail.tsx, and RunOverview.tsx — split into their own file (rather
// than exported from PhaseRail.tsx alongside its component) purely to
// satisfy eslint's react-refresh/only-export-components rule, which wants a
// component file to export components only. Same rationale as route.ts's
// split from router.tsx.

import { formatDuration as formatClosedDuration } from './api'

// phaseDisplayLabels maps a workflow phase's stable code-order name
// (workflows/backup's Phase* constants, as returned verbatim by GET
// /api/runs/{runID}/phases) to its operator-facing label. Only one phase
// needs this: PhaseGeneratePAR2's Go constant value is literally "Generate
// PAR2" (issue #277's technical context — the workflow's own string, kept
// stable for history/logging), but SPEC's terminology and this redesign
// display it as the shorter "PAR2". Every other phase's stable name already
// doubles as its display label.
const phaseDisplayLabels: Record<string, string> = {
  'Generate PAR2': 'PAR2',
}

// phaseLabel renders a phase's stable name as operator-facing text.
export function phaseLabel(name: string): string {
  return phaseDisplayLabels[name] ?? name
}

// formatDuration renders the elapsed time between a phase's start and end (or
// now, for a still-active phase with a start but no end yet) as short
// operator-facing text — "1m 42s"/"2h 3m" — or an em dash when there is no
// start time to measure from at all (a pending phase). The rendering itself
// delegates to api.ts's shared formatDuration (issue #276 consolidated the
// h/m/s math there); this wrapper only adds the two semantics that helper
// deliberately does not have — a missing *start* (a pending phase; api.ts's
// callers always have one) and an open end meaning "elapsed until now"
// (api.ts's renders an em dash for a run that has not closed, where the
// phase rail's active phase wants its live elapsed instead).
export function formatDuration(startTime?: string, endTime?: string): string {
  if (!startTime) {
    return '—'
  }

  if (endTime !== undefined) {
    return formatClosedDuration(startTime, endTime)
  }

  // Active phase: elapsed until now. Under modest client/server clock skew a
  // just-started phase can carry a start a hair in the future, which would make
  // api.ts's formatDuration render its negative-elapsed em dash — blanking the
  // live elapsed for a second or two until the clocks cross. Clamp the end to no
  // earlier than the start so it reads "0s" instead. A normal past start is
  // unaffected (now is already the later of the two).
  const startMillis = Date.parse(startTime)
  const nowMillis = Date.now()
  const endMillis = Number.isNaN(startMillis) ? nowMillis : Math.max(startMillis, nowMillis)

  return formatClosedDuration(startTime, new Date(endMillis).toISOString())
}
