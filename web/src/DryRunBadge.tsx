// DryRunBadge marks a run that was submitted as a dry-run (the mhvtl override,
// runsubmit.ApplyDryRun) — driven by the run's `dryRun` flag (pkg/runsapi
// RunSummary.DryRun, read from the Temporal memo). Blue is the tone the config
// Review step already uses for "Dry-run (mhvtl)", so a run reads the same way
// when it is submitted and when it is later browsed. Renders nothing for a
// production run, so callers can drop it in unconditionally next to a status.
export default function DryRunBadge({ dryRun }: { dryRun: boolean }) {
  if (!dryRun) {
    return null
  }

  return (
    <span
      title="Submitted as a dry-run — the mhvtl virtual library, not real tape hardware"
      className="rounded-full border border-blue/30 bg-blue-bg px-2 py-0.5 font-mono text-[11px] font-semibold tracking-[0.03em] text-blue"
    >
      DRY-RUN
    </span>
  )
}
