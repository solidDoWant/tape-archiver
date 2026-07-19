import { useRef } from 'react'
import AgeKeygenPanel, { type AgeKeypair } from './AgeKeygenPanel'
import {
  blankSlotsCopiesIssue,
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
  // deploy carries the deploy-owned config the form still needs: the physical
  // library topology (issue #305) that bounds the blank-slot grid picker.
  // deployStatus drives the picker's loading/unavailable/unconfigured messaging.
  // Sourced by ConfigPage from GET /api/config/ui (uiConfig.ts). The library
  // changer/drive devices and Discord webhook (issue #304) are deploy-owned too
  // but are NOT shown in the form at all — they are filled into the submitted
  // config by buildConfig and enforced server-side (pkg/runsapi applyDeployConfig),
  // so there is no per-run form control for them.
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

// SlotGridEditor is the Library section's blank/write-target slot picker: a grid
// of the deployment's real storage slots (issue #305), bounded by the library
// topology from deploy config (GET /api/config/ui) rather than the design mock's
// hardcoded 44-slot layout (DESIGN_ANALYSIS.md §6 flags a fixed count as wrong to
// bake into the frontend). Storage slots are numbered 1..slotCount; the cleaning
// and I/O-station slot numbers are rendered non-selectable, so an operator can
// only choose real, loadable storage slots. Selecting a slot toggles its
// membership in the per-run library.blankSlots array — the topology only bounds
// the choice; which blanks a given run uses is still a per-run decision.
//
// The topology is deploy-owned (like the changer/drive devices, which the form
// no longer shows at all — see ConfigFormProps), so the picker handles the same
// config-fetch states: while the fetch is in flight it shows a loading line, on
// failure an unavailable notice, and when the deployment declared no topology
// (slotCount 0) an actionable notice naming the env var to set — with JSON /
// paste mode as the escape hatch for setting blank slots without a declared
// topology. Unlike the devices/webhook, the blank-slot *selection* is a per-run
// choice, so this stays a form control rather than being dropped.
function SlotGridEditor({
  status,
  slotCount,
  cleaningSlots,
  ioStationSlots,
  selected,
  copies,
  onChange,
}: {
  status: UiConfigState['status']
  slotCount: number
  cleaningSlots: number[]
  ioStationSlots: number[]
  selected: number[]
  copies: number
  onChange: (slots: number[]) => void
}) {
  const label = (
    <label className={fieldLabelClass}>
      blank storage slots <span className="text-text-faint">— slots holding blank tapes for this run</span>
    </label>
  )

  if (status === 'loading') {
    return (
      <div>
        {label}
        <p role="status" className="font-mono text-[11px] text-text-faint">
          Loading deploy config…
        </p>
      </div>
    )
  }

  if (status === 'error') {
    return (
      <div>
        {label}
        <p className="font-mono text-[11px] text-amber">Deploy config unavailable — could not load the library topology.</p>
      </div>
    )
  }

  if (slotCount <= 0) {
    return (
      <div>
        {label}
        <p className="font-mono text-[11px] text-amber">
          Library topology not configured — set LIBRARY_SLOT_COUNT (or use JSON / paste mode).
        </p>
      </div>
    )
  }

  const cleaning = new Set(cleaningSlots)
  const ioStation = new Set(ioStationSlots)
  const selectedSet = new Set(selected)

  const toggle = (slot: number) => {
    if (selectedSet.has(slot)) {
      onChange(selected.filter((existing) => existing !== slot))
    } else {
      onChange([...selected, slot].sort((a, b) => a - b))
    }
  }

  // Only real storage slots are selectable, and duplicates are collapsed, so
  // this count matches what buildConfig actually submits — it dedups and
  // topology-filters blankSlots (configModel.ts). Counting the raw length
  // instead double-counts a slot that a JSON-mode edit repeated (e.g. [1,1,2]),
  // producing an inline count and copies-multiple warning that disagree with
  // the deduped config the Review step and server both see.
  const selectableCount = new Set(
    selected.filter(
      (slot) => slot >= 1 && slot <= slotCount && !cleaning.has(slot) && !ioStation.has(slot),
    ),
  ).size

  // Warn inline when the chosen blanks can't form whole logical-tape copy sets
  // (count not a positive multiple of copies) — the same cross-field gate the
  // server enforces (internal/config/validate.go) and Review re-checks, surfaced
  // here at the point of selection so the operator sees it as they pick slots.
  const copiesIssue = blankSlotsCopiesIssue(copies, selectableCount)

  return (
    <div>
      {label}
      <div className="flex flex-wrap gap-1.5" role="group" aria-label="Blank storage slots">
        {Array.from({ length: slotCount }, (_, index) => index + 1).map((slot) => {
          const reservedFor = cleaning.has(slot) ? 'cleaning' : ioStation.has(slot) ? 'I/O-station' : null

          if (reservedFor !== null) {
            return (
              <span
                key={slot}
                aria-label={`Slot ${slot} — reserved for ${reservedFor}`}
                title={`Reserved for ${reservedFor}`}
                className="flex h-8 w-8 items-center justify-center rounded-md border border-dashed border-border bg-inset font-mono text-[11px] text-text-faint"
              >
                {slot}
              </span>
            )
          }

          const isSelected = selectedSet.has(slot)

          return (
            <button
              key={slot}
              type="button"
              aria-label={`Slot ${slot}`}
              aria-pressed={isSelected}
              onClick={() => toggle(slot)}
              className={
                isSelected
                  ? 'flex h-8 w-8 cursor-pointer items-center justify-center rounded-md border border-green-line bg-green-bg font-mono text-[11px] font-semibold text-green'
                  : 'flex h-8 w-8 cursor-pointer items-center justify-center rounded-md border border-border-strong bg-surface-2 font-mono text-[11px] text-text transition-colors hover:border-green-line hover:text-green'
              }
            >
              {slot}
            </button>
          )
        })}
      </div>
      <div className="mt-1.5 font-mono text-[11px] text-text-faint">{selectableCount} blank slot(s) selected</div>
      {copiesIssue !== null ? (
        <p role="alert" className="mt-1 font-mono text-[11px] text-amber">
          {copiesIssue}
        </p>
      ) : null}
    </div>
  )
}

// NumberField is an uncontrolled numeric input so a value can actually be
// cleared and retyped (and a decimal typed through). Binding a number straight
// to the input and coercing every keystroke with `Number(value) || fallback`
// snapped the field back to the fallback the moment it was emptied, and
// destroyed a partial decimal like "0.". Leaving the input uncontrolled lets the
// browser hold the in-progress text; onValue commits only a complete, parseable
// number, so an empty or mid-edit entry leaves the last committed value in place.
// Decimal fields use a text input (type=number reports an empty string for
// "0.", which would lose the fraction). The field is remounted (a fresh
// defaultValue) whenever it needs to reflect an externally-set value — a restart
// preload remounts the whole form, and the redundancy/optical toggles mount a
// fresh input — so it never needs to be controlled to stay in sync.
function NumberField({
  id,
  value,
  onValue,
  decimal = false,
  min,
  max,
  className,
}: {
  id: string
  value: number
  onValue: (value: number) => void
  decimal?: boolean
  min?: number
  max?: number
  className?: string
}) {
  return (
    <input
      id={id}
      type={decimal ? 'text' : 'number'}
      inputMode={decimal ? 'decimal' : 'numeric'}
      min={min}
      max={max}
      step={decimal ? '0.1' : undefined}
      defaultValue={String(value)}
      onChange={(event) => {
        const raw = event.target.value

        const parsed = Number(raw)
        if (raw.trim() !== '' && !Number.isNaN(parsed)) {
          onValue(parsed)
        }
      }}
      // On blur, snap an emptied or half-typed field's display back to the last
      // committed value so it never lingers out of sync with the state behind
      // it. The input is uncontrolled, so this writes the DOM value directly.
      onBlur={(event) => {
        const raw = event.target.value
        if (raw.trim() === '' || Number.isNaN(Number(raw))) {
          event.target.value = String(value)
        }
      }}
      className={className}
    />
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
  // The recipient produced by the most recent in-session keygen, so a second
  // "Generate new age keypair" replaces it rather than leaving it behind as a
  // recipient nobody holds a private key for (only the newest identity is
  // escrowed). Manually-entered recipients are untouched.
  const lastGeneratedRecipient = useRef<string | null>(null)

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
                  className="h-7 w-7 flex-none rounded-md border border-border text-text-faint transition-colors hover:border-red-line hover:bg-red-bg hover:text-red disabled:opacity-50"
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
            <NumberField
              id="config-copies"
              min={1}
              value={form.copies}
              onValue={(copies) => setForm((previous) => ({ ...previous, copies }))}
              className={inputClass}
            />
          </div>
          <div>
            <label className={fieldLabelClass} htmlFor="config-slice-size">
              slice size (GiB)
            </label>
            <NumberField
              id="config-slice-size"
              decimal
              min={0}
              value={form.sliceSizeGiB}
              onValue={(sliceSizeGiB) => setForm((previous) => ({ ...previous, sliceSizeGiB }))}
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
              <NumberField
                id="config-target-percentage"
                min={1}
                max={100}
                value={form.targetPercentage}
                onValue={(targetPercentage) => setForm((previous) => ({ ...previous, targetPercentage }))}
                className={inputClass}
              />
            </div>
          ) : (
            <div className="mt-2.5 w-40">
              <label className={fieldLabelClass} htmlFor="config-fill-floor">
                floor %
              </label>
              <NumberField
                id="config-fill-floor"
                min={1}
                max={100}
                value={form.fillFloor}
                onValue={(fillFloor) => setForm((previous) => ({ ...previous, fillFloor }))}
                className={inputClass}
              />
            </div>
          )}
        </div>
      </div>

      <div className={cardClass}>
        <div className={sectionTitleClass}>Library</div>

        <div className="flex flex-col gap-3">
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

          <SlotGridEditor
            status={deployStatus}
            slotCount={deploy.slotCount}
            cleaningSlots={deploy.cleaningSlots}
            ioStationSlots={deploy.ioStationSlots}
            selected={form.blankSlots}
            copies={form.copies}
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
            onGenerated={(keypair: AgeKeypair) => {
              const previousGenerated = lastGeneratedRecipient.current
              lastGeneratedRecipient.current = keypair.recipient

              setForm((previous) => ({
                ...previous,
                // Drop empties and the previously-generated recipient (now
                // orphaned — its identity is being replaced), keep everything
                // else, then add the new recipient.
                recipients: [
                  ...previous.recipients.filter(
                    (recipient) => recipient.trim() !== '' && recipient !== previousGenerated,
                  ),
                  keypair.recipient,
                ],
                identity: keypair.identity,
              }))
            }}
          />
        </div>
      </div>

      <div className={cardClass}>
        <div className={sectionTitleClass}>Delivery</div>

        <div className="flex items-center justify-between">
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
                setForm((previous) => ({
                  ...previous,
                  opticalBurnEnabled: event.target.checked,
                  // Enabling burn guarantees at least one copy: a config loaded
                  // with copies=0 (burn off) would otherwise turn on with the
                  // hidden 0 still in state and build a no-op burn block.
                  opticalCopies: event.target.checked && previous.opticalCopies < 1 ? 1 : previous.opticalCopies,
                }))
              }
              className="h-4 w-4 accent-green"
            />
          </label>
        </div>

        {form.opticalBurnEnabled ? (
          <div className="mt-3.5 flex flex-col gap-3 border-t border-border pt-3.5">
            <div className="w-40">
              <label className={fieldLabelClass} htmlFor="config-optical-copies">
                copies per run
              </label>
              <NumberField
                id="config-optical-copies"
                // min 1: this input is only shown when optical burn is enabled,
                // and copies=0 means "disabled" server-side — so a 0 here builds
                // an opticalBurn block that silently burns nothing yet reads
                // back as OFF on a Form round-trip (opticalBurnEnabled is
                // copies>0). Clearing the field commits nothing (the last value
                // stands); the min attribute nudges toward a positive count.
                min={1}
                value={form.opticalCopies}
                onValue={(opticalCopies) => setForm((previous) => ({ ...previous, opticalCopies }))}
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
