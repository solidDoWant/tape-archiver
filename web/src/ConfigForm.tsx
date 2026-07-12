import { useState, type ChangeEvent } from 'react'
import AgeKeygenPanel, { type AgeKeypair } from './AgeKeygenPanel'
import {
  k8sApiVersions,
  newSourceFormState,
  type DeployConfig,
  type FormState,
  type SourceFormState,
} from './configModel'
import type { UiConfigState } from './uiConfig'
import { ltoGenerations } from './ltoGenerations'

export interface ConfigFormProps {
  form: FormState
  setForm: (updater: (previous: FormState) => FormState) => void
  // deploy carries the deploy-owned library devices and Discord webhook (issue
  // #304) the Library and Delivery sections render read-only instead of as
  // per-run inputs; deployStatus drives their loading/unavailable/unconfigured
  // messaging. Sourced by ConfigPage from GET /api/config/ui (uiConfig.ts).
  deploy: DeployConfig
  deployStatus: UiConfigState['status']
}

const cardClass = 'rounded-xl border border-border bg-surface p-4.5 shadow-card sm:p-5'
const sectionTitleClass = 'mb-3.5 text-[13px] font-semibold text-text'
const fieldLabelClass = 'mb-1.5 block text-[11px] text-text-dim'
const inputClass =
  'w-full rounded-lg border border-border-strong bg-surface-2 px-2.5 py-2 font-mono text-[12px] text-text outline-none'
const segmentedWrapClass =
  'flex w-fit items-center gap-0.5 rounded-lg border border-border bg-inset p-0.5'

function segmentButtonClass(active: boolean): string {
  return active
    ? 'rounded-md border border-border-strong bg-surface px-2.5 py-1.5 font-mono text-[11px] font-semibold text-text'
    : 'rounded-md border border-transparent px-2.5 py-1.5 font-mono text-[11px] font-semibold text-text-faint transition-colors hover:text-text'
}

// stringListEditor renders a repeatable list of plain-text values (drives,
// recipients, optical drives) with add/remove controls — the shared shape
// several sections below need, so it is factored out once rather than
// duplicated per field.
function StringListEditor({
  label,
  values,
  placeholder,
  onChange,
}: {
  label: string
  values: string[]
  placeholder: string
  onChange: (values: string[]) => void
}) {
  return (
    <div>
      <label className={fieldLabelClass}>{label}</label>
      <div className="flex flex-col gap-1.5">
        {values.map((value, index) => (
          // Index keys are fine here: values are plain strings edited in
          // place, and entries are only ever appended or removed, never
          // reordered.
          <div key={index} className="flex items-center gap-1.5">
            <input
              value={value}
              placeholder={placeholder}
              onChange={(event) => {
                const next = [...values]
                next[index] = event.target.value
                onChange(next)
              }}
              className={inputClass}
            />
            <button
              type="button"
              aria-label={`Remove ${label} entry ${index + 1}`}
              onClick={() => onChange(values.filter((_, i) => i !== index))}
              className="h-7 w-7 flex-none rounded-md border border-border text-text-faint transition-colors hover:border-red-line hover:bg-red-bg hover:text-red"
            >
              ×
            </button>
          </div>
        ))}
      </div>
      <button
        type="button"
        onClick={() => onChange([...values, ''])}
        className="mt-1.5 rounded-lg border border-dashed border-border-strong px-3 py-1.5 text-[11px] font-medium text-text-dim transition-colors hover:bg-surface-2 hover:text-text"
      >
        + Add
      </button>
    </div>
  )
}

// SlotListEditor is the Library section's blank-slot editor: a free-form
// add/remove list of slot numbers rather than a fixed-size button grid.
// The design mock's grid hardcodes one physical library's 44-slot layout
// (DESIGN_ANALYSIS.md §6 flags this as wrong to bake into the frontend —
// library.blankSlots in the schema is an arbitrary []integer with no fixed
// count), so this works for any library size.
function SlotListEditor({ slots, onChange }: { slots: number[]; onChange: (slots: number[]) => void }) {
  const [draft, setDraft] = useState('')

  const addSlot = () => {
    const parsed = Number.parseInt(draft, 10)
    if (Number.isFinite(parsed) && parsed >= 0 && !slots.includes(parsed)) {
      onChange([...slots, parsed].sort((a, b) => a - b))
    }
    setDraft('')
  }

  return (
    <div>
      <label className={fieldLabelClass}>
        blank storage slots <span className="text-text-faint">— slot numbers holding blank tapes</span>
      </label>
      <div className="flex flex-wrap gap-1.5">
        {slots.map((slot) => (
          <span
            key={slot}
            className="flex items-center gap-1.5 rounded-md border border-border-strong bg-surface-2 px-2 py-1 font-mono text-[11px] text-text"
          >
            {slot}
            <button
              type="button"
              aria-label={`Remove slot ${slot}`}
              onClick={() => onChange(slots.filter((existing) => existing !== slot))}
              className="text-text-faint hover:text-red"
            >
              ×
            </button>
          </span>
        ))}
      </div>
      <div className="mt-2 flex items-center gap-1.5">
        <input
          value={draft}
          onChange={(event: ChangeEvent<HTMLInputElement>) => setDraft(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') {
              event.preventDefault()
              addSlot()
            }
          }}
          placeholder="slot number"
          aria-label="New blank slot number"
          className="w-32 rounded-lg border border-border-strong bg-surface-2 px-2.5 py-1.5 font-mono text-[12px] text-text outline-none"
        />
        <button
          type="button"
          onClick={addSlot}
          className="rounded-lg border border-dashed border-border-strong px-3 py-1.5 text-[11px] font-medium text-text-dim transition-colors hover:bg-surface-2 hover:text-text"
        >
          + Add slot
        </button>
      </div>
      <div className="mt-1.5 font-mono text-[11px] text-text-faint">{slots.length} blank slot(s) configured</div>
    </div>
  )
}

// DeployField renders one deploy-owned value (issue #304) read-only: the
// library changer/drive devices and Discord webhook are properties of the
// deployment/host (GET /api/config/ui), not per-run choices, so the operator
// sees the value that will be used rather than a free-text input they must
// re-type. It shows a loading state while the config fetch is in flight, an
// unavailable state if it failed, the value(s) once loaded, or — when the
// deployment configured nothing — an actionable notice naming the env var to
// set (or JSON mode as the escape hatch), so an unconfigured deployment reads
// as a fix-me rather than a silent blank that only fails at Review.
function DeployField({
  label,
  status,
  values,
  envHint,
}: {
  label: string
  status: UiConfigState['status']
  values: string[]
  envHint: string
}) {
  return (
    <div>
      <label className={fieldLabelClass}>
        {label} <span className="text-text-faint">— from deploy config</span>
      </label>

      {status === 'loading' ? (
        <p role="status" className="font-mono text-[11px] text-text-faint">
          Loading deploy config…
        </p>
      ) : null}

      {status === 'error' ? (
        <p className="font-mono text-[11px] text-amber">
          Deploy config unavailable — could not load {label.toLowerCase()}.
        </p>
      ) : null}

      {status === 'loaded' && values.length > 0 ? (
        <div className="flex flex-col gap-1.5">
          {values.map((value) => (
            <div
              key={value}
              className="w-full rounded-lg border border-border bg-inset px-2.5 py-2 font-mono text-[12px] text-text-dim break-all"
            >
              {value}
            </div>
          ))}
        </div>
      ) : null}

      {status === 'loaded' && values.length === 0 ? (
        <p className="font-mono text-[11px] text-amber">Not configured for this deployment — set {envHint}.</p>
      ) : null}
    </div>
  )
}

// ConfigForm is the config page's guided Form mode (DESIGN_ANALYSIS.md §2
// "D. Config"): sources, copies & redundancy, library, encryption, and
// delivery sections that together build a FormState, converted to a
// schema-shaped RunConfig by configModel.ts's buildConfig for the Review
// step. Every single-choice input here (the ZFS/k8s toggle, the k8s
// name/labelSelector toggle, the redundancy fixed/fill-to-capacity toggle)
// is a segmented control rather than independent checkboxes, so the built
// config can never carry both halves of a schema "exactly one of" pair —
// see configModel.ts's buildConfig doc comment.
function ConfigForm({ form, setForm, deploy, deployStatus }: ConfigFormProps) {
  const deployDrives = deploy.drives.filter((drive) => drive.trim() !== '')
  const deployChanger = deploy.changer.trim() !== '' ? [deploy.changer] : []
  const deployWebhook = deploy.webhookUrl.trim() !== '' ? [deploy.webhookUrl] : []

  const updateSource = (id: string, patch: Partial<SourceFormState>) => {
    setForm((previous) => ({
      ...previous,
      sources: previous.sources.map((source) => (source.id === id ? { ...source, ...patch } : source)),
    }))
  }

  const addSource = () => {
    setForm((previous) => ({ ...previous, sources: [...previous.sources, newSourceFormState()] }))
  }

  const removeSource = (id: string) => {
    setForm((previous) => ({ ...previous, sources: previous.sources.filter((source) => source.id !== id) }))
  }

  return (
    <div className="flex flex-col gap-4">
      <div className={cardClass}>
        <div className="mb-3.5 flex items-center gap-2">
          <span className={sectionTitleClass}>Sources</span>
          <span className="font-mono text-[11px] text-text-faint">ZFS datasets or k8s VolumeSnapshots</span>
        </div>

        <div className="flex flex-col gap-3">
          {form.sources.map((source, index) => (
            <div key={source.id} className="rounded-lg border border-border bg-surface-2 p-3.5">
              <div className="mb-3 flex items-center gap-2.5">
                <div className={segmentedWrapClass} role="group" aria-label={`Source ${index + 1} type`}>
                  <button
                    type="button"
                    className={segmentButtonClass(source.type === 'zfs')}
                    onClick={() => updateSource(source.id, { type: 'zfs' })}
                  >
                    ZFS
                  </button>
                  <button
                    type="button"
                    className={segmentButtonClass(source.type === 'k8s')}
                    onClick={() => updateSource(source.id, { type: 'k8s' })}
                  >
                    k8s
                  </button>
                </div>

                <input
                  value={source.label}
                  placeholder="label (optional)"
                  aria-label={`Source ${index + 1} label`}
                  onChange={(event) => updateSource(source.id, { label: event.target.value })}
                  className="min-w-0 flex-1 rounded-lg border border-border-strong bg-surface px-2.5 py-1.5 font-mono text-[12px] text-text outline-none"
                />

                <button
                  type="button"
                  aria-label={`Remove source ${index + 1}`}
                  onClick={() => removeSource(source.id)}
                  disabled={form.sources.length === 1}
                  className="h-7 w-7 flex-none rounded-md border border-border text-text-faint transition-colors hover:border-red-line hover:bg-red-bg hover:text-red disabled:opacity-40"
                >
                  ×
                </button>
              </div>

              {source.type === 'zfs' ? (
                <div>
                  <label className={fieldLabelClass}>ZFS dataset</label>
                  <input
                    value={source.zfsName}
                    placeholder="bulk-pool-01/dataset"
                    onChange={(event) => updateSource(source.id, { zfsName: event.target.value })}
                    className={inputClass}
                  />
                </div>
              ) : (
                <div className="flex flex-col gap-2.5">
                  <div>
                    <label className={fieldLabelClass}>resource kind</label>
                    <div className={segmentedWrapClass} role="group" aria-label={`Source ${index + 1} k8s kind`}>
                      {Object.keys(k8sApiVersions).map((kind) => (
                        <button
                          key={kind}
                          type="button"
                          className={segmentButtonClass(source.k8sKind === kind)}
                          onClick={() =>
                            updateSource(source.id, { k8sKind: kind as SourceFormState['k8sKind'] })
                          }
                        >
                          {kind}
                        </button>
                      ))}
                    </div>
                  </div>

                  <div className={segmentedWrapClass} role="group" aria-label={`Source ${index + 1} selection mode`}>
                    <button
                      type="button"
                      className={segmentButtonClass(source.k8sSelection === 'name')}
                      onClick={() => updateSource(source.id, { k8sSelection: 'name' })}
                    >
                      By name
                    </button>
                    <button
                      type="button"
                      className={segmentButtonClass(source.k8sSelection === 'labelSelector')}
                      onClick={() => updateSource(source.id, { k8sSelection: 'labelSelector' })}
                    >
                      By label selector
                    </button>
                  </div>

                  {source.k8sSelection === 'name' ? (
                    <div className="grid grid-cols-1 gap-2.5 sm:grid-cols-2">
                      <div>
                        <label className={fieldLabelClass}>namespace</label>
                        <input
                          value={source.k8sNamespace}
                          placeholder="media"
                          onChange={(event) => updateSource(source.id, { k8sNamespace: event.target.value })}
                          className={inputClass}
                        />
                      </div>
                      <div>
                        <label className={fieldLabelClass}>name</label>
                        <input
                          value={source.k8sName}
                          placeholder="media-pvc"
                          onChange={(event) => updateSource(source.id, { k8sName: event.target.value })}
                          className={inputClass}
                        />
                      </div>
                    </div>
                  ) : (
                    <div className="grid grid-cols-1 gap-2.5 sm:grid-cols-2">
                      <div>
                        <label className={fieldLabelClass}>
                          namespace <span className="text-text-faint">(optional — all namespaces if empty)</span>
                        </label>
                        <input
                          value={source.k8sNamespace}
                          placeholder="all namespaces"
                          onChange={(event) => updateSource(source.id, { k8sNamespace: event.target.value })}
                          className={inputClass}
                        />
                      </div>
                      <div>
                        <label className={fieldLabelClass}>labelSelector</label>
                        <input
                          value={source.k8sLabelSelector}
                          placeholder="app=media,tier=cold"
                          onChange={(event) =>
                            updateSource(source.id, { k8sLabelSelector: event.target.value })
                          }
                          className={inputClass}
                        />
                      </div>
                    </div>
                  )}
                </div>
              )}

              <div className="mt-3.5 flex items-center justify-between border-t border-border pt-3">
                <div>
                  <div className="text-[12px] text-text">Compress with zstd</div>
                  <div className="mt-0.5 font-mono text-[11px] text-text-faint">
                    shrinks this source before encryption
                  </div>
                </div>
                <label className="cursor-pointer">
                  <input
                    type="checkbox"
                    checked={source.compression}
                    aria-label={`Source ${index + 1} compression`}
                    onChange={(event) => updateSource(source.id, { compression: event.target.checked })}
                    className="h-4 w-4 accent-green"
                  />
                </label>
              </div>
            </div>
          ))}
        </div>

        <button
          type="button"
          onClick={addSource}
          className="mt-3 w-full rounded-lg border border-dashed border-border-strong py-2.5 text-[12px] font-medium text-text-dim transition-colors hover:bg-surface-2 hover:text-text"
        >
          + Add source
        </button>
      </div>

      <div className={cardClass}>
        <div className={sectionTitleClass}>Copies &amp; redundancy</div>
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
          <div>
            <label className={fieldLabelClass} htmlFor="config-copies">
              copies
            </label>
            <input
              id="config-copies"
              type="number"
              min={1}
              value={form.copies}
              onChange={(event) =>
                setForm((previous) => ({ ...previous, copies: Number(event.target.value) || 1 }))
              }
              className={inputClass}
            />
          </div>
          <div>
            <label className={fieldLabelClass} htmlFor="config-slice-size">
              slice size (GiB)
            </label>
            <input
              id="config-slice-size"
              type="number"
              min={0}
              step="0.1"
              value={form.sliceSizeGiB}
              onChange={(event) =>
                setForm((previous) => ({ ...previous, sliceSizeGiB: Number(event.target.value) || 0 }))
              }
              className={inputClass}
            />
          </div>
        </div>

        <div className="mt-3">
          <label className={fieldLabelClass}>PAR2 redundancy</label>
          <div className={segmentedWrapClass} role="group" aria-label="PAR2 redundancy mode">
            <button
              type="button"
              className={segmentButtonClass(form.redundancyMode === 'fixed')}
              onClick={() => setForm((previous) => ({ ...previous, redundancyMode: 'fixed' }))}
            >
              Fixed %
            </button>
            <button
              type="button"
              className={segmentButtonClass(form.redundancyMode === 'fillToCapacity')}
              onClick={() => setForm((previous) => ({ ...previous, redundancyMode: 'fillToCapacity' }))}
            >
              Fill to capacity
            </button>
          </div>

          {form.redundancyMode === 'fixed' ? (
            <div className="mt-2.5 w-40">
              <label className={fieldLabelClass} htmlFor="config-target-percentage">
                target %
              </label>
              <input
                id="config-target-percentage"
                type="number"
                min={1}
                max={100}
                value={form.targetPercentage}
                onChange={(event) =>
                  setForm((previous) => ({ ...previous, targetPercentage: Number(event.target.value) || 1 }))
                }
                className={inputClass}
              />
            </div>
          ) : (
            <div className="mt-2.5 w-40">
              <label className={fieldLabelClass} htmlFor="config-fill-floor">
                floor %
              </label>
              <input
                id="config-fill-floor"
                type="number"
                min={1}
                max={100}
                value={form.fillFloor}
                onChange={(event) =>
                  setForm((previous) => ({ ...previous, fillFloor: Number(event.target.value) || 1 }))
                }
                className={inputClass}
              />
            </div>
          )}
        </div>
      </div>

      <div className={cardClass}>
        <div className={sectionTitleClass}>Library</div>

        <div className="flex flex-col gap-3">
          <DeployField
            label="changer device"
            status={deployStatus}
            values={deployChanger}
            envHint="LIBRARY_CHANGER (or use JSON / paste mode)"
          />

          <DeployField
            label="drive devices"
            status={deployStatus}
            values={deployDrives}
            envHint="LIBRARY_DRIVES (or use JSON / paste mode)"
          />

          <div className="w-fit">
            <label className={fieldLabelClass} htmlFor="config-tape-generation">
              tape capacity
            </label>
            <select
              id="config-tape-generation"
              value={form.tapeGeneration}
              onChange={(event) => setForm((previous) => ({ ...previous, tapeGeneration: event.target.value }))}
              className="rounded-lg border border-border-strong bg-surface-2 px-2.5 py-2 font-mono text-[12px] text-text outline-none"
            >
              {ltoGenerations.map((generation) => (
                <option key={generation.label} value={generation.label}>
                  {generation.label} · {generation.capacityLabel}
                </option>
              ))}
            </select>
          </div>

          <SlotListEditor
            slots={form.blankSlots}
            onChange={(blankSlots) => setForm((previous) => ({ ...previous, blankSlots }))}
          />
        </div>

        <label className="mt-3.5 flex cursor-pointer items-center gap-2 text-[12px] text-text-dim">
          <input
            type="checkbox"
            checked={form.allowNonBlankTapes}
            onChange={(event) =>
              setForm((previous) => ({ ...previous, allowNonBlankTapes: event.target.checked }))
            }
            className="h-3.5 w-3.5 accent-red"
          />
          allow non-blank tapes{' '}
          <span className="font-mono text-[11px] text-red">— irreversible overwrite</span>
        </label>
      </div>

      <div className={cardClass}>
        <div className="mb-3.5 flex items-center gap-2">
          <span className={sectionTitleClass}>Encryption</span>
          <span className="font-mono text-[11px] text-text-faint">age · hybrid ML-KEM-768</span>
        </div>

        <div className="flex flex-col gap-3">
          <StringListEditor
            label="recipients (public)"
            values={form.recipients}
            placeholder="age1pq1…"
            onChange={(recipients) => setForm((previous) => ({ ...previous, recipients }))}
          />

          <div>
            <label className={fieldLabelClass} htmlFor="config-identity">
              identity / private key (escrowed in report)
            </label>
            <textarea
              id="config-identity"
              rows={2}
              value={form.identity}
              placeholder="AGE-SECRET-KEY-PQ-1…"
              onChange={(event) => setForm((previous) => ({ ...previous, identity: event.target.value }))}
              className={`${inputClass} resize-y break-all`}
            />
          </div>
        </div>

        <p className="mt-2.5 font-mono text-[11px] leading-relaxed text-amber">
          The private identity is printed in the report on purpose: anyone holding the report can decrypt the
          tapes. Store the report accordingly.
        </p>

        <div className="mt-3">
          <AgeKeygenPanel
            onGenerated={(keypair: AgeKeypair) =>
              setForm((previous) => ({
                ...previous,
                recipients: [...previous.recipients.filter((recipient) => recipient.trim() !== ''), keypair.recipient],
                identity: keypair.identity,
              }))
            }
          />
        </div>
      </div>

      <div className={cardClass}>
        <div className={sectionTitleClass}>Delivery</div>

        <DeployField
          label="Discord webhook URL"
          status={deployStatus}
          values={deployWebhook}
          envHint="DELIVERY_WEBHOOK_URL (or use JSON / paste mode)"
        />

        <div className="mt-3.5 flex items-center justify-between">
          <div>
            <div className="text-[12.5px] font-medium text-text">Optical recovery discs</div>
            <div className="mt-0.5 font-mono text-[11px] text-text-faint">Burned on the configured burner drives</div>
          </div>
          <label className="cursor-pointer">
            <input
              type="checkbox"
              checked={form.opticalBurnEnabled}
              aria-label="Enable optical recovery discs"
              onChange={(event) =>
                setForm((previous) => ({ ...previous, opticalBurnEnabled: event.target.checked }))
              }
              className="h-4 w-4 accent-green"
            />
          </label>
        </div>

        {form.opticalBurnEnabled ? (
          <div className="mt-3.5 flex flex-col gap-3 border-t border-border pt-3.5">
            <StringListEditor
              label="burner devices"
              values={form.opticalDrives}
              placeholder="/dev/sr0"
              onChange={(opticalDrives) => setForm((previous) => ({ ...previous, opticalDrives }))}
            />

            <div className="w-40">
              <label className={fieldLabelClass} htmlFor="config-optical-copies">
                copies per run
              </label>
              <input
                id="config-optical-copies"
                type="number"
                min={0}
                value={form.opticalCopies}
                onChange={(event) =>
                  setForm((previous) => ({ ...previous, opticalCopies: Number(event.target.value) || 0 }))
                }
                className={inputClass}
              />
            </div>

            <label className="flex cursor-pointer items-center gap-2 text-[12px] text-text-dim">
              <input
                type="checkbox"
                checked={form.allowNonBlankDiscs}
                onChange={(event) =>
                  setForm((previous) => ({ ...previous, allowNonBlankDiscs: event.target.checked }))
                }
                className="h-3.5 w-3.5 accent-red"
              />
              reclaim non-blank rewritable discs{' '}
              <span className="font-mono text-[11px] text-red">— irreversible overwrite</span>
            </label>
          </div>
        ) : null}
      </div>
    </div>
  )
}

export default ConfigForm
