// configModel.ts is the run-config page's data model (issue #279): TypeScript
// types mirroring schemas/run-config.schema.json's shape (RunConfig and its
// nested types — kept in sync by hand, since the schema itself is not
// expected to change for this issue and is validated at runtime by
// configSchema.ts, not by these types), the guided Form mode's own editable
// FormState, and the two directions of conversion between them.
//
// Some schema fields have no Form-mode UI (library.ioWaitTimeoutSeconds,
// library.writeFailureWaitTimeoutSeconds, delivery.opticalBurn.
// burnWaitTimeoutSeconds, feasibilityOverhead) — the design mock does not
// expose them either, and they are advanced/operational tuning knobs an
// operator building a guided config rarely needs on day one. Form mode
// simply omits them (the run then gets internal/config's own documented
// defaults); an operator who needs one uses JSON mode instead, which can
// express the full schema.

import { defaultLtoGeneration, ltoGenerationForCapacity, ltoGenerations } from './ltoGenerations'

export interface ZFSPathSource {
  name: string
}

export interface K8sRef {
  apiVersion: string
  kind: string
  namespace?: string
  name?: string
  labelSelector?: string
}

export interface Source {
  compression?: boolean
  k8s?: K8sRef
  zfsPath?: ZFSPathSource
  label?: string
}

export interface FillConfig {
  floor: number
}

export interface Redundancy {
  targetPercentage?: number
  fillToCapacity?: FillConfig
  sliceSizeBytes: number
}

export interface Library {
  changer: string
  drives: string[]
  blankSlots: number[]
  tapeCapacityBytes: number
  ioWaitTimeoutSeconds?: number
  writeFailureWaitTimeoutSeconds?: number
  allowNonBlankTapes?: boolean
}

export interface Encryption {
  recipients: string[]
  identity: string
}

export interface OpticalBurn {
  drives: string[]
  copies: number
  allowNonBlankDiscs?: boolean
  burnWaitTimeoutSeconds?: number
}

export interface Delivery {
  webhookUrl: string
  opticalBurn?: OpticalBurn
}

export interface RunConfig {
  sources: Source[]
  copies: number
  library: Library
  redundancy: Redundancy
  encryption: Encryption
  delivery: Delivery
  feasibilityOverhead?: number
}

// k8sApiVersions maps the two k8s resource kinds the Form mode's source
// editor offers to their standard apiVersion (docs/configuration.md's own
// K8sRef examples), so the operator only ever picks a kind, never has to
// know or type the matching API group/version.
export const k8sApiVersions: Record<string, string> = {
  VolumeSnapshot: 'snapshot.storage.k8s.io/v1',
  VolumeGroupSnapshot: 'groupsnapshot.storage.k8s.io/v1alpha1',
}

const bytesPerGiB = 1024 * 1024 * 1024

let nextSourceID = 0

// newSourceID returns a fresh, stable-for-the-session React key for a new
// Form-mode source row — never part of the built config, purely a UI list
// key so add/remove/reorder does not misattribute input focus/state between
// rows (the same reason any dynamic list needs stable keys beyond array
// index).
export function newSourceID(): string {
  nextSourceID += 1

  return `source-${nextSourceID}`
}

export interface SourceFormState {
  id: string
  type: 'zfs' | 'k8s'
  label: string
  zfsName: string
  k8sKind: 'VolumeSnapshot' | 'VolumeGroupSnapshot'
  k8sSelection: 'name' | 'labelSelector'
  k8sNamespace: string
  k8sName: string
  k8sLabelSelector: string
  compression: boolean
}

export function newSourceFormState(): SourceFormState {
  return {
    id: newSourceID(),
    type: 'zfs',
    label: '',
    zfsName: '',
    k8sKind: 'VolumeSnapshot',
    k8sSelection: 'name',
    k8sNamespace: '',
    k8sName: '',
    k8sLabelSelector: '',
    compression: true,
  }
}

export interface FormState {
  sources: SourceFormState[]
  copies: number
  redundancyMode: 'fixed' | 'fillToCapacity'
  targetPercentage: number
  fillFloor: number
  sliceSizeGiB: number
  changer: string
  drives: string[]
  blankSlots: number[]
  tapeGeneration: string
  allowNonBlankTapes: boolean
  recipients: string[]
  identity: string
  webhookUrl: string
  opticalBurnEnabled: boolean
  opticalDrives: string[]
  opticalCopies: number
  allowNonBlankDiscs: boolean
}

export function defaultFormState(): FormState {
  return {
    sources: [newSourceFormState()],
    copies: 2,
    redundancyMode: 'fixed',
    targetPercentage: 10,
    fillFloor: 5,
    sliceSizeGiB: 4,
    changer: '/dev/sch0',
    drives: ['/dev/nst0', '/dev/nst1'],
    blankSlots: [],
    tapeGeneration: 'LTO-6',
    allowNonBlankTapes: false,
    recipients: [''],
    identity: '',
    webhookUrl: '',
    opticalBurnEnabled: false,
    opticalDrives: ['/dev/sr0'],
    opticalCopies: 2,
    allowNonBlankDiscs: false,
  }
}

// buildSource converts one Form-mode source row into a schema-shaped Source.
// The ZFS/k8s toggle and (within k8s) the by-name/by-label-selector toggle
// are each a single-choice UI control, so the built object always carries
// exactly one of zfsPath/k8s, and a k8s ref always carries exactly one of
// name/labelSelector, by construction — the same invariant
// docs/configuration.md documents (Source/K8sRef "exactly one of ... must be
// set") without configSchema.ts's generic validator needing to re-check it
// (the committed JSON Schema itself does not encode that invariant either;
// see configSchema.ts's doc comment).
function buildSource(source: SourceFormState): Source {
  const built: Source = { compression: source.compression }

  if (source.label.trim() !== '') {
    built.label = source.label.trim()
  }

  if (source.type === 'zfs') {
    built.zfsPath = { name: source.zfsName.trim() }

    return built
  }

  const k8s: K8sRef = {
    apiVersion: k8sApiVersions[source.k8sKind],
    kind: source.k8sKind,
  }

  if (source.k8sNamespace.trim() !== '') {
    k8s.namespace = source.k8sNamespace.trim()
  }

  if (source.k8sSelection === 'name') {
    k8s.name = source.k8sName.trim()
  } else {
    k8s.labelSelector = source.k8sLabelSelector.trim()
  }

  built.k8s = k8s

  return built
}

// buildConfig assembles a schema-shaped RunConfig from the guided Form
// mode's current state. It is deliberately a pure, total function — it never
// throws and never drops data, even for a still-incomplete form (e.g. a
// blank ZFS dataset name) — so it can back both the live JSON preview and
// the Review step; configSchema.ts's validator is what tells the operator
// what, if anything, is still wrong with the result.
export function buildConfig(form: FormState): RunConfig {
  const tapeCapacityBytes =
    ltoGenerations.find((generation) => generation.label === form.tapeGeneration)?.capacityBytes ??
    defaultLtoGeneration.capacityBytes

  const redundancy: Redundancy =
    form.redundancyMode === 'fixed'
      ? { targetPercentage: form.targetPercentage, sliceSizeBytes: Math.round(form.sliceSizeGiB * bytesPerGiB) }
      : { fillToCapacity: { floor: form.fillFloor }, sliceSizeBytes: Math.round(form.sliceSizeGiB * bytesPerGiB) }

  const library: Library = {
    changer: form.changer.trim(),
    drives: form.drives.map((drive) => drive.trim()).filter((drive) => drive !== ''),
    blankSlots: form.blankSlots,
    tapeCapacityBytes,
    allowNonBlankTapes: form.allowNonBlankTapes,
  }

  const delivery: Delivery = { webhookUrl: form.webhookUrl.trim() }

  if (form.opticalBurnEnabled) {
    delivery.opticalBurn = {
      drives: form.opticalDrives.map((drive) => drive.trim()).filter((drive) => drive !== ''),
      copies: form.opticalCopies,
      allowNonBlankDiscs: form.allowNonBlankDiscs,
    }
  }

  return {
    sources: form.sources.map(buildSource),
    copies: form.copies,
    library,
    redundancy,
    encryption: {
      recipients: form.recipients.map((recipient) => recipient.trim()).filter((recipient) => recipient !== ''),
      identity: form.identity.trim(),
    },
    delivery,
  }
}

// sourceFormStateFromSource is configToFormState's per-source reverse
// mapping. A source this best-effort mapper cannot represent faithfully
// (neither zfsPath nor k8s set, or a k8s ref missing both name and
// labelSelector — configs JSON mode alone can express, e.g. mid-edit) still
// produces a row rather than throwing: it defaults to an empty ZFS row, so
// switching into Form mode never crashes on an unusual-but-schema-legal
// document; the operator sees an empty/incomplete row to fill in instead.
function sourceFormStateFromSource(source: Source): SourceFormState {
  const base = newSourceFormState()
  base.label = source.label ?? ''
  base.compression = source.compression ?? true

  if (source.zfsPath) {
    base.type = 'zfs'
    base.zfsName = source.zfsPath.name

    return base
  }

  if (source.k8s) {
    base.type = 'k8s'
    base.k8sKind = source.k8s.kind === 'VolumeGroupSnapshot' ? 'VolumeGroupSnapshot' : 'VolumeSnapshot'
    base.k8sNamespace = source.k8s.namespace ?? ''

    if (source.k8s.labelSelector) {
      base.k8sSelection = 'labelSelector'
      base.k8sLabelSelector = source.k8s.labelSelector
    } else {
      base.k8sSelection = 'name'
      base.k8sName = source.k8s.name ?? ''
    }
  }

  return base
}

// unmodeledFields lists (as dotted paths) the fields present in config that
// Form mode has no controls for and configToFormState therefore drops —
// the advanced tuning knobs this file's doc comment names. ConfigPage's
// JSON → Form switch calls this to warn the operator exactly what a
// continued Form-mode edit would silently lose (the values survive only for
// as long as the JSON text itself is untouched); kept adjacent to
// configToFormState so a future Form-mode control for one of these fields
// is naturally removed from both places together.
export function unmodeledFields(config: RunConfig): string[] {
  const dropped: string[] = []

  if (config.feasibilityOverhead !== undefined) {
    dropped.push('feasibilityOverhead')
  }

  if (config.library?.ioWaitTimeoutSeconds !== undefined) {
    dropped.push('library.ioWaitTimeoutSeconds')
  }

  if (config.library?.writeFailureWaitTimeoutSeconds !== undefined) {
    dropped.push('library.writeFailureWaitTimeoutSeconds')
  }

  if (config.delivery?.opticalBurn?.burnWaitTimeoutSeconds !== undefined) {
    dropped.push('delivery.opticalBurn.burnWaitTimeoutSeconds')
  }

  return dropped
}

// configToFormState is buildConfig's inverse: a best-effort reconstruction
// of Form-mode state from an arbitrary schema-shaped config (e.g. one
// parsed from JSON-mode text), used when the operator switches from JSON
// mode into Form mode (ConfigPage.tsx's mode toggle — see its doc comment
// for the switch's documented semantics). Fields Form mode has no controls
// for are dropped (unmodeledFields above enumerates them; ConfigPage warns
// the operator at switch time). Every field has a safe fallback,
// so this never throws: a tapeCapacityBytes with no exact match in
// ltoGenerations falls back to defaultLtoGeneration (Form mode's <select>
// can only ever choose one of that fixed table's values — an operator
// needing an unusual capacity keeps using JSON mode), an
// empty/malformed redundancy falls back to a 10% fixed target, and so on.
export function configToFormState(config: RunConfig): FormState {
  const form = defaultFormState()

  form.sources = config.sources.length > 0 ? config.sources.map(sourceFormStateFromSource) : form.sources
  form.copies = config.copies
  form.sliceSizeGiB = config.redundancy.sliceSizeBytes / bytesPerGiB

  if (config.redundancy.fillToCapacity) {
    form.redundancyMode = 'fillToCapacity'
    form.fillFloor = config.redundancy.fillToCapacity.floor
  } else if (typeof config.redundancy.targetPercentage === 'number') {
    form.redundancyMode = 'fixed'
    form.targetPercentage = config.redundancy.targetPercentage
  }

  form.changer = config.library.changer
  form.drives = config.library.drives.length > 0 ? config.library.drives : form.drives
  form.blankSlots = config.library.blankSlots
  form.tapeGeneration = (ltoGenerationForCapacity(config.library.tapeCapacityBytes) ?? defaultLtoGeneration).label
  form.allowNonBlankTapes = config.library.allowNonBlankTapes ?? false

  form.recipients = config.encryption.recipients.length > 0 ? config.encryption.recipients : form.recipients
  form.identity = config.encryption.identity

  form.webhookUrl = config.delivery.webhookUrl
  form.opticalBurnEnabled = Boolean(config.delivery.opticalBurn && config.delivery.opticalBurn.drives.length > 0)

  if (config.delivery.opticalBurn) {
    form.opticalDrives =
      config.delivery.opticalBurn.drives.length > 0 ? config.delivery.opticalBurn.drives : form.opticalDrives
    form.opticalCopies = config.delivery.opticalBurn.copies
    form.allowNonBlankDiscs = config.delivery.opticalBurn.allowNonBlankDiscs ?? false
  }

  return form
}
