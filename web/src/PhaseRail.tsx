import type { PhaseInfo, PhaseStatus } from './RunDetail'
import { formatDuration, phaseLabel } from './phaseFormat'

// PhaseMarker is the 18x18 circular status indicator (DESIGN_ANALYSIS.md §3's
// "phase stepper/rail"): a solid check for completed, a solid cross for
// failed, a pulsing ring for active, and a plain outline ring for pending.
// The glyph is decorative (aria-hidden) — PhaseRailRow's button carries the
// status in its own accessible name instead, so a screen-reader user gets it
// without relying on the glyph's shape/color alone.
function PhaseMarker({ status }: { status: PhaseStatus }) {
  const base = 'flex h-[18px] w-[18px] flex-none items-center justify-center rounded-full text-[10px] font-bold'

  switch (status) {
    case 'completed':
      return (
        <span aria-hidden="true" className={`${base} bg-green text-bg`}>
          ✓
        </span>
      )
    case 'failed':
      return (
        <span aria-hidden="true" className={`${base} bg-red text-bg`}>
          ✕
        </span>
      )
    case 'active':
      return (
        <span
          aria-hidden="true"
          className={`${base} border-2 border-amber border-t-transparent bg-transparent text-amber animate-spin`}
        />
      )
    case 'pending':
    default:
      return <span aria-hidden="true" className={`${base} border-2 border-border-strong bg-transparent`} />
  }
}

export interface PhaseRailProps {
  phases: PhaseInfo[]
  // selected is 'overview' or one phase's stable name (PhaseInfo.Name).
  selected: string
  onSelect: (selection: string) => void
}

// PhaseRail is the run detail page's left-hand navigation (DESIGN_ANALYSIS.md
// §2.B/§7 PhaseRail): a "Run overview" item plus one row per pipeline phase,
// in the exact order the server returns them (workflows/backup's
// backupPhases() order — this component never re-sorts). Selecting a row
// (issue #277 AC2) is purely a local selection change owned by the caller;
// this component is otherwise a stateless presentational list driven by
// props, re-rendering with fresh statuses/durations whenever the caller
// passes a new phases array (its own refresh-on-SSE-update policy lives in
// RunDetail.tsx).
//
// Responsive per issue #277 AC10 (narrow viewport, no page-body horizontal
// scroll): below md this lays out as a horizontally-scrollable strip (the
// scrolling is contained to this rail, never the page body) instead of the
// desktop's fixed-width vertical column with a right border.
function PhaseRail({ phases, selected, onSelect }: PhaseRailProps) {
  const itemBase =
    'flex shrink-0 cursor-pointer items-center gap-2.5 rounded-lg px-2.5 py-2 text-left text-[12.5px] transition-colors md:w-full'

  return (
    <nav
      aria-label="Run phases"
      className="flex shrink-0 gap-1 overflow-x-auto border-b border-border p-2.5 md:w-60 md:flex-col md:gap-1 md:overflow-x-visible md:border-r md:border-b-0 md:p-4"
    >
      <button
        type="button"
        onClick={() => onSelect('overview')}
        aria-current={selected === 'overview' ? 'true' : undefined}
        className={`${itemBase} ${selected === 'overview' ? 'bg-nav-active font-semibold text-text' : 'text-text-dim hover:bg-nav-hover'}`}
      >
        <span aria-hidden="true" className="flex w-[18px] flex-none justify-center text-text-faint">
          ▤
        </span>
        <span>Run overview</span>
      </button>

      <div className="mt-1 hidden shrink-0 px-2.5 pt-2 pb-1 font-mono text-[11px] tracking-[0.08em] text-text-faint md:block">
        PHASES · {phases.length}
      </div>

      {phases.map((phase) => {
        const isSelected = selected === phase.name
        const label = phaseLabel(phase.name)

        return (
          <button
            key={phase.name}
            type="button"
            onClick={() => onSelect(phase.name)}
            aria-current={isSelected ? 'true' : undefined}
            data-status={phase.status}
            className={`${itemBase} ${isSelected ? 'bg-nav-active font-semibold text-text' : 'text-text hover:bg-nav-hover'}`}
          >
            <PhaseMarker status={phase.status} />
            <span className="flex-1">
              {label}
              <span className="sr-only"> — {phase.status}</span>
            </span>
            <span className="hidden font-mono text-[11px] text-text-faint md:inline">
              {formatDuration(phase.startTime, phase.endTime)}
            </span>
          </button>
        )
      })}
    </nav>
  )
}

export default PhaseRail
