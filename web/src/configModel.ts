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
// complete another copy set), else null. The empty-selection, copies < 1, and
// non-integer copies cases are left to the schema validator's own
// minItems / minimum / "must be a whole number" gates, so this stays a
// single-purpose cross-field check and does not double-report them — and never
// emits a nonsensical "not a multiple of 2.5 copies" message for a fractional
// copies value a JSON/paste-mode config might carry.
export function blankSlotsCopiesIssue(copies: number, blankSlotCount: number): string | null {
  if (copies < 1 || !Number.isInteger(copies) || blankSlotCount === 0 || blankSlotCount % copies === 0) {
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

  // Dedup, preserving order (a slot is either blank or not, so a duplicate is
  // meaningless — but internal/config rejects duplicate slot addresses, so a
  // form built from a JSON/restart config that carried [1, 1, 2] would 400 at
  // submit; the client schema interpreter has no uniqueItems), then drop any
  // slot outside the deployment's real storage topology. The grid picker can
  // only produce in-range, non-reserved slots, but a JSON/restart load can
  // leave out-of-range or reserved ones in form.blankSlots that the grid never
  // renders (so the operator cannot deselect them) — the same filter the
  // inline slot count applies (ConfigForm's selectableCount) and the server
  // enforces (validateBlankSlotsAgainstTopology), applied here so the built
  // config, the Review-step count, and the inline count all agree and an
  // out-of-range slot is never submitted. When the topology is unknown
  // (slotCount <= 0 — deploy config missing), only dedup, matching the
  // server's own no-op in that case.
  const reservedSlots = new Set([...deploy.cleaningSlots, ...deploy.ioStationSlots])
  const blankSlots = [...new Set(form.blankSlots)].filter(
    (slot) => deploy.slotCount <= 0 || (slot >= 1 && slot <= deploy.slotCount && !reservedSlots.has(slot)),
  )

  const library: Library = {
    changer: deploy.changer.trim(),
    drives: deploy.drives.map((drive) => drive.trim()).filter((drive) => drive !== ''),
    blankSlots,
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
  // A JSON / paste-mode document can carry a null or non-object element in the
  // sources array (`{"sources": [null]}` — valid JSON, invalid shape). Treat it
  // as an empty source so it loads a blank row rather than dereferencing null
  // below and throwing a TypeError — configToFormState's doc promises it "never
  // throws", and ConfigPage would otherwise catch the throw and misreport it as
  // "not a config object".
  const safe: Partial<Source> = typeof source === 'object' && source !== null ? source : {}

  // asText coerces a possibly-wrong-typed leaf (a JSON-mode document can carry
  // e.g. zfsPath.name as a number) to the string the form fields and
  // buildConfig's `.trim()` require, so an unusual-but-parseable document
  // populates a row rather than crashing later — see configToFormState.
  const asText = (value: unknown): string => (typeof value === 'string' ? value : value == null ? '' : String(value))

  const base = newSourceFormState()
  base.label = asText(safe.label)
  base.compression = safe.compression ?? true

  if (safe.zfsPath) {
    base.type = 'zfs'
    base.zfsName = asText(safe.zfsPath.name)

    return base
  }

  if (safe.k8s) {
    base.type = 'k8s'
    base.k8sKind = safe.k8s.kind === 'VolumeGroupSnapshot' ? 'VolumeGroupSnapshot' : 'VolumeSnapshot'
    base.k8sNamespace = asText(safe.k8s.namespace)

    if (safe.k8s.labelSelector) {
      base.k8sSelection = 'labelSelector'
      base.k8sLabelSelector = asText(safe.k8s.labelSelector)
    } else {
      base.k8sSelection = 'name'
      base.k8sName = asText(safe.k8s.name)
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

// unmatchedTapeCapacity reports config's library.tapeCapacityBytes when it is a
// number that matches no LTO generation in ltoGenerations, else null. Form
// mode's capacity <select> can only choose one of that fixed table's values, so
// a JSON → Form switch of a config carrying a custom capacity silently falls
// back to defaultLtoGeneration. ConfigPage names this in its mode-switch notice
// so the operator knows the value was replaced (unlike unmodeledFields, which
// are simply dropped, this one is reset to a different value) rather than
// discovering it only after submit.
export function unmatchedTapeCapacity(config: RunConfig): number | null {
  const capacity = (config.library as Partial<Library> | undefined)?.tapeCapacityBytes

  if (typeof capacity !== 'number') {
    return null
  }

  return ltoGenerationForCapacity(capacity) ? null : capacity
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

  // Every field below is read through a runtime type check, not just an
  // optional-chaining `?.`: ConfigPage casts arbitrary parsed JSON to
  // RunConfig (only its object-ness is checked), so a schema-invalid but
  // syntactically valid document can carry a field of the wrong type
  // (blankSlots: 5, recipients: "x", floor: "3", ...). A `?? fallback` still
  // lets a wrong-typed value through, which then crashes later at render
  // (`new Set(5)`) or in buildConfig (`"x".trim()`); guarding the type here is
  // what makes the doc comment's "this never throws" actually hold.
  const sources = Array.isArray(partial.sources) ? partial.sources : []
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
      if (typeof redundancy.fillToCapacity.floor === 'number') {
        form.fillFloor = redundancy.fillToCapacity.floor
      }
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
    form.blankSlots = Array.isArray(library.blankSlots)
      ? library.blankSlots.filter((slot): slot is number => typeof slot === 'number')
      : form.blankSlots

    const generation =
      typeof library.tapeCapacityBytes === 'number' ? ltoGenerationForCapacity(library.tapeCapacityBytes) : undefined
    form.tapeGeneration = (generation ?? defaultLtoGeneration).label

    form.allowNonBlankTapes = library.allowNonBlankTapes ?? false
  }

  const encryption = partial.encryption as Partial<Encryption> | undefined
  if (encryption) {
    form.recipients =
      Array.isArray(encryption.recipients) && encryption.recipients.length > 0
        ? encryption.recipients.map((recipient) => String(recipient))
        : form.recipients
    form.identity = typeof encryption.identity === 'string' ? encryption.identity : form.identity
  }

  // The burner drives are deploy-owned (issue #317) and sourced from deploy
  // config at buildConfig time, so — like library.changer/drives — they are not
  // mapped out of the JSON. Optical burn is "enabled" when the block is present
  // with a positive copy count; the drives no longer gate it (a Form-built
  // config carries deploy drives, and a JSON config may legitimately leave them
  // empty for the server to fill).
  const opticalBurn = (partial.delivery as Partial<Delivery> | undefined)?.opticalBurn
  form.opticalBurnEnabled = Boolean(opticalBurn && typeof opticalBurn.copies === 'number' && opticalBurn.copies > 0)

  if (opticalBurn) {
    if (typeof opticalBurn.copies === 'number') {
      form.opticalCopies = opticalBurn.copies
    }
    form.allowNonBlankDiscs = opticalBurn.allowNonBlankDiscs ?? false
  }

  return form
}
