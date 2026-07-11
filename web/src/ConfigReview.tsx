import type { RunConfig } from './configModel'

export interface ConfigReviewProps {
  config: RunConfig
  dryRun: boolean
}

function summaryLabel(source: RunConfig['sources'][number]): string {
  if (source.label) {
    return source.label
  }

  return source.zfsPath?.name ?? source.k8s?.name ?? source.k8s?.labelSelector ?? '(unlabeled)'
}

// ConfigReview is the config page's Review step (issue #279's acceptance
// criterion: "the resulting run config is shown as JSON and validates
// against the committed schema before it can be submitted"). It only ever
// renders a config ConfigPage has already validated (the "Review →"
// transition blocks on any schema issue — see ConfigPage.tsx), so this
// component itself is a pure, read-only display: the summary table shows
// only facts genuinely knowable client-side (source labels, copies,
// redundancy policy, recipient count, recovery-disc setting) — never a
// fabricated bin-packed tape count, which depends on measured archive sizes
// only the Resolve/Pack phases know (DESIGN_ANALYSIS.md flags the design
// mock's hardcoded "6 physical tapes" style figures as exactly the kind of
// frontend-invented number to avoid).
function ConfigReview({ config, dryRun }: ConfigReviewProps) {
  const redundancyLabel = config.redundancy.fillToCapacity
    ? `fill to capacity · floor ${config.redundancy.fillToCapacity.floor}%`
    : `fixed ${config.redundancy.targetPercentage}%`

  return (
    <div className="rounded-xl border border-border bg-surface p-5 shadow-card">
      <div className="text-[14px] font-semibold text-text">Review before submitting</div>
      <p className="mt-1 max-w-xl text-[12.5px] text-text-dim">
        Confirm what this run will do. It has already validated against the run-config schema — nothing is
        submitted until you press Submit.
      </p>

      <div className="mt-4 rounded-lg border border-border">
        <dl className="divide-y divide-border">
          <div className="flex items-center justify-between px-4 py-2.5">
            <dt className="text-[12.5px] text-text-dim">Mode</dt>
            <dd className={`font-mono text-[12px] font-semibold ${dryRun ? 'text-blue' : 'text-amber'}`}>
              {dryRun ? 'Dry-run (mhvtl)' : 'Production'}
            </dd>
          </div>
          <div className="flex items-center justify-between px-4 py-2.5">
            <dt className="text-[12.5px] text-text-dim">Sources</dt>
            <dd className="font-mono text-[12px] text-text">
              {config.sources.length} · {config.sources.map(summaryLabel).join(', ') || '—'}
            </dd>
          </div>
          <div className="flex items-center justify-between px-4 py-2.5">
            <dt className="text-[12.5px] text-text-dim">Copies</dt>
            <dd className="font-mono text-[12px] text-text">{config.copies}</dd>
          </div>
          <div className="flex items-center justify-between px-4 py-2.5">
            <dt className="text-[12.5px] text-text-dim">Redundancy</dt>
            <dd className="font-mono text-[12px] text-text">PAR2 {redundancyLabel}</dd>
          </div>
          <div className="flex items-center justify-between px-4 py-2.5">
            <dt className="text-[12.5px] text-text-dim">Encryption</dt>
            <dd className="font-mono text-[12px] text-text">
              age · {config.encryption.recipients.length} recipient(s)
            </dd>
          </div>
          <div className="flex items-center justify-between px-4 py-2.5">
            <dt className="text-[12.5px] text-text-dim">Recovery discs</dt>
            <dd className="font-mono text-[12px] text-text">
              {config.delivery.opticalBurn
                ? `on · ${config.delivery.opticalBurn.copies} cop${config.delivery.opticalBurn.copies === 1 ? 'y' : 'ies'}`
                : 'off'}
            </dd>
          </div>
        </dl>
      </div>

      <div className="mt-3.5 flex items-start gap-2 rounded-lg border border-border-strong bg-surface-2 p-3">
        <span aria-hidden="true" className="text-text-dim">
          ⓘ
        </span>
        <span className="text-[12px] leading-relaxed text-text-dim">
          Blank status can&rsquo;t be checked ahead of time. The run reads each target slot just before writing
          and fails before any write if one holds a non-blank tape, unless you allowed overwrite above.
        </span>
      </div>

      <div className="mt-3.5">
        <label htmlFor="config-review-json" className={'mb-1.5 block text-[11px] text-text-dim'}>
          Final run-config JSON
        </label>
        <pre
          id="config-review-json"
          className="max-h-96 overflow-auto rounded-lg border border-console-border bg-console-bg p-3.5 font-mono text-[11.5px] leading-relaxed text-console-text"
        >
          {JSON.stringify(config, null, 2)}
        </pre>
      </div>
    </div>
  )
}

export default ConfigReview
