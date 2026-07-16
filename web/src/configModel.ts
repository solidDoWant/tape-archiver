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

// DeployConfig is the deploy-owned subset of a run config the guided Form mode
// does NOT let the operator edit per run (issues #304 and #317): the library
// changer/drive device paths, the Discord webhook URL, and the optical burner
// device paths are properties of the deployment/host supplied by GET
// /api/config/ui (uiConfig.ts's deployConfigFrom), not typed into the form.
// buildConfig fills these into the submitted config so it still carries them
// (SPEC §4.2 — the run config stays the single source of truth); they are simply
// not FormState fields. This is also enforced server-side: where a deployment
// configures one of these, cmd/web overwrites it onto every submitted config
// regardless of mode (pkg/runsapi applyDeployConfig), so JSON / paste mode cannot
// override a deploy-owned device/webhook either — only a field the deployment
// left unset can be supplied per run. (The burner drives are applied server-side
// only when the run actually enables optical burn.)
//
// slotCount/cleaningSlots/ioStationSlots are the physical library topology
// (issue #305): the storage slot count and the reserved cleaning / I/O-station
// slot numbers, from which ConfigForm's slot-grid picker draws a grid bounded to
// the real library. Unlike the devices above, the per-run *selection* of blank
// slots (library.blankSlots) is still an operator choice — the topology only
// bounds it — so blankSlots stays a FormState field; the topology does not.
export interface DeployConfig {
  changer: string
  drives: string[]
  webhookUrl: string
  // opticalBurnDrives is the deploy-owned optical burner device paths (issue
  // #317), the delivery analogue of drives above: the operator toggles optical
  // burn on/off and sets the copy count per run, but the burner devices come
  // from deploy config. [] when the deployment configured none.
  opticalBurnDrives: string[]
  slotCount: number
  cleaningSlots: number[]
  ioStationSlots: number[]
}

export interface FormState {
  sources: SourceFormState[]
  copies: number
  redundancyMode: 'fixed' | 'fillToCapacity'
  targetPercentage: number
  fillFloor: number
  sliceSizeGiB: number
  blankSlots: number[]
  tapeGeneration: string
  allowNonBlankTapes: boolean
  recipients: string[]
  identity: string
  opticalBurnEnabled: boolean
  // The burner device paths are deploy-owned (issue #317) and sourced from
  // DeployConfig.opticalBurnDrives at buildConfig time, so — like the library
  // changer/drives (issue #304) — they are not a FormState field. The operator
  // still controls the on/off toggle and the copy count per run.
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
    blankSlots: [],
    tapeGeneration: 'LTO-6',
    allowNonBlankTapes: false,
    recipients: [''],
    identity: '',
    opticalBurnEnabled: false,
    opticalCopies: 2,
    allowNonBlankDiscs: false,
  }
}

// blankSlotsCopiesIssue mirrors internal/config/validate.go's cross-field gate:
// every logical tape needs one blank per copy (physical tapes = logical tapes ×
// copies, SPEC §4.3), so the selected blank slots only form whole logical-tape
// sets when their count is a positive multiple of copies. Returns a human
// message when the selection violates that (a leftover count that can never
// complete another copy set), else null. The empty-selection and copies < 1
// cases are left to the schema validator's own minItems / minimum gates, so this
// stays a single-purpose cross-field check and does not double-report them.
export function blankSlotsCopiesIssue(copies: number, blankSlotCount: number): string | null {
  if (copies < 1 || blankSlotCount === 0 || blankSlotCount % copies === 0) {
    return null
  }

  return `${blankSlotCount} blank slot(s) selected is not a multiple of ${copies} copies — each logical tape needs ${copies} tape(s), so select a multiple of ${copies}.`
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
// mode's current state and the deploy-owned config (issue #304): the library
// changer/drive device paths and the Discord webhook URL come from deploy
// (GET /api/config/ui), not FormState, since they are deployment/host
// properties the operator does not re-type per run — see DeployConfig. It is
// deliberately a pure, total function — it never throws and never drops data,
// even for a still-incomplete form (e.g. a blank ZFS dataset name) or an
// unconfigured deployment (empty deploy values) — so it can back both the live
// JSON preview and the Review step; configSchema.ts's validator is what tells
// the operator what, if anything, is still wrong with the result.
export function buildConfig(form: FormState, deploy: DeployConfig): RunConfig {
  const tapeCapacityBytes =
    ltoGenerations.find((generation) => generation.label === form.tapeGeneration)?.capacityBytes ??
    defaultLtoGeneration.capacityBytes

  const redundancy: Redundancy =
    form.redundancyMode === 'fixed'
      ? { targetPercentage: form.targetPercentage, sliceSizeBytes: Math.round(form.sliceSizeGiB * bytesPerGiB) }
      : { fillToCapacity: { floor: form.fillFloor }, sliceSizeBytes: Math.round(form.sliceSizeGiB * bytesPerGiB) }

  const library: Library = {
    changer: deploy.changer.trim(),
    drives: deploy.drives.map((drive) => drive.trim()).filter((drive) => drive !== ''),
    // Dedup, preserving order: a slot is either blank or not, so a duplicate is
    // meaningless — but internal/config rejects duplicate slot addresses, so a
    // form built from a JSON/restart config that carried [1, 1, 2] (the grid
    // itself can't produce dupes) would 400 at submit. The client schema
    // interpreter has no uniqueItems, so collapse them here.
    blankSlots: [...new Set(form.blankSlots)],
    tapeCapacityBytes,
    allowNonBlankTapes: form.allowNonBlankTapes,
  }

  const delivery: Delivery = { webhookUrl: deploy.webhookUrl.trim() }

  if (form.opticalBurnEnabled) {
    // The burner drives are deploy-owned (issue #317): sourced from deploy
    // config, not the form, mirroring library.changer/drives above. The server
    // re-applies them over the submitted config (applyDeployConfig), so this is
    // only where the operator's config first picks them up.
    delivery.opticalBurn = {
      drives: deploy.opticalBurnDrives.map((drive) => drive.trim()).filter((drive) => drive !== ''),
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

// deployOwnedFields lists (as dotted paths) the deploy-owned fields (issues
// #304 and #317) a JSON config sets to a non-empty value that a JSON → Form
// switch will replace with the deployment's own config: Form mode sources
// library.changer/drives, delivery.webhookUrl, and delivery.opticalBurn.drives
// from deploy config (buildConfig's deploy argument), not FormState, so a custom
// value typed into JSON mode does not survive the switch. ConfigPage warns the operator by name
// at switch time (its mode-switch notice), rather than swapping them silently —
// unlike unmodeledFields, these are not lost but *replaced by* the deploy
// values, which the read-only Library/Delivery displays then show.
export function deployOwnedFields(config: RunConfig): string[] {
  const overridden: string[] = []

  if (config.library?.changer) {
    overridden.push('library.changer')
  }

  if (config.library?.drives?.length) {
    overridden.push('library.drives')
  }

  if (config.delivery?.webhookUrl) {
    overridden.push('delivery.webhookUrl')
  }

  // Burner drives are deploy-owned too (issue #317): a JSON → Form switch
  // replaces any drives typed in JSON mode with the deployment's own list.
  if (config.delivery?.opticalBurn?.drives?.length) {
    overridden.push('delivery.opticalBurn.drives')
  }

  return overridden
}

// configToFormState is buildConfig's inverse: a best-effort reconstruction
// of Form-mode state from an arbitrary schema-shaped config (e.g. one
// parsed from JSON-mode text), used when the operator switches from JSON
// mode into Form mode (ConfigPage.tsx's mode toggle — see its doc comment
// for the switch's documented semantics). Fields Form mode has no controls
// for are dropped (unmodeledFields above enumerates them; ConfigPage warns
// the operator at switch time), and the deploy-owned device/webhook fields
// (deployOwnedFields above) are not mapped out of the JSON at all — Form mode
// sources them from deploy config, so they are left to buildConfig's deploy
// argument rather than reconstructed into FormState. Every field has a safe
// fallback,
// so this never throws: a tapeCapacityBytes with no exact match in
// ltoGenerations falls back to defaultLtoGeneration (Form mode's <select>
// can only ever choose one of that fixed table's values — an operator
// needing an unusual capacity keeps using JSON mode), an
// empty/malformed redundancy falls back to a 10% fixed target, and so on.
export function configToFormState(config: RunConfig): FormState {
  // ConfigPage casts arbitrary parsed JSON to RunConfig before calling this
  // (only its object-ness is checked), so at runtime any section — or field —
  // may be absent. Read every section through a Partial view so a partial but
  // valid-JSON config loads what it can into the form instead of throwing a
  // TypeError, which ConfigPage would otherwise misreport as "not valid JSON".
  // This is what the doc comment above means by "every field has a safe
  // fallback, so this never throws".
  const partial = config as Partial<RunConfig>
  const form = defaultFormState()

  const sources = partial.sources ?? []
  form.sources = sources.length > 0 ? sources.map(sourceFormStateFromSource) : form.sources

  if (typeof partial.copies === 'number') {
    form.copies = partial.copies
  }

  const redundancy = partial.redundancy as Partial<Redundancy> | undefined
  if (redundancy) {
    if (typeof redundancy.sliceSizeBytes === 'number') {
      form.sliceSizeGiB = redundancy.sliceSizeBytes / bytesPerGiB
    }

    if (redundancy.fillToCapacity) {
      form.redundancyMode = 'fillToCapacity'
      form.fillFloor = redundancy.fillToCapacity.floor
    } else if (typeof redundancy.targetPercentage === 'number') {
      form.redundancyMode = 'fixed'
      form.targetPercentage = redundancy.targetPercentage
    }
  }

  // library.changer/drives and delivery.webhookUrl are deploy-owned (issue
  // #304): Form mode has no controls for them and sources them from deploy
  // config at buildConfig time, so a JSON → Form switch deliberately does not
  // map them out of the JSON into FormState — see configToFormState's doc
  // comment and ConfigPage's mode-switch notice.
  const library = partial.library as Partial<Library> | undefined
  if (library) {
    form.blankSlots = library.blankSlots ?? form.blankSlots

    const generation =
      typeof library.tapeCapacityBytes === 'number' ? ltoGenerationForCapacity(library.tapeCapacityBytes) : undefined
    form.tapeGeneration = (generation ?? defaultLtoGeneration).label

    form.allowNonBlankTapes = library.allowNonBlankTapes ?? false
  }

  const encryption = partial.encryption as Partial<Encryption> | undefined
  if (encryption) {
    form.recipients = encryption.recipients && encryption.recipients.length > 0 ? encryption.recipients : form.recipients
    form.identity = encryption.identity ?? form.identity
  }

  // The burner drives are deploy-owned (issue #317) and sourced from deploy
  // config at buildConfig time, so — like library.changer/drives — they are not
  // mapped out of the JSON. Optical burn is "enabled" when the block is present
  // with a positive copy count; the drives no longer gate it (a Form-built
  // config carries deploy drives, and a JSON config may legitimately leave them
  // empty for the server to fill).
  const opticalBurn = (partial.delivery as Partial<Delivery> | undefined)?.opticalBurn
  form.opticalBurnEnabled = Boolean(opticalBurn && opticalBurn.copies > 0)

  if (opticalBurn) {
    form.opticalCopies = opticalBurn.copies
    form.allowNonBlankDiscs = opticalBurn.allowNonBlankDiscs ?? false
  }

  return form
}
