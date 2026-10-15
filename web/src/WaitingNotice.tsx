import type { PhaseInfo } from './RunDetail'
import { phaseLabel, waitingSummary } from './phaseFormat'

// WaitingNotice flags a phase whose activity has been scheduled for longer
// than a worker cold start but never picked up (issue #344), so a run that
// cannot progress does not read as merely running.
function WaitingNotice({ phase }: { phase: PhaseInfo }) {
  if (!phase.waiting?.stalled) {
    return null
  }

  return (
    <div role="status" className="rounded-lg border border-amber-line bg-amber-bg px-4 py-3 text-[12px] text-amber">
      <div className="font-semibold">
        {phaseLabel(phase.name)} is waiting for a worker on the “{phase.waiting.taskQueue}” task queue
      </div>
      <div className="mt-1">{waitingSummary(phase.waiting)}</div>
    </div>
  )
}

export default WaitingNotice
