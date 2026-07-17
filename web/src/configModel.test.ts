import { describe, expect, it } from 'vitest'
import {
  blankSlotsCopiesIssue,
  buildConfig,
  configToFormState,
  defaultFormState,
  deployOwnedFields,
  newSourceFormState,
  unmodeledFields,
  type DeployConfig,
  type RunConfig,
} from './configModel'

// testDeploy is the deploy-owned config (issue #304) buildConfig fills into the
// submitted run config: the library changer/drive devices and Discord webhook
// the guided form no longer edits per run. Tests pass this explicitly so the
// injection is exercised rather than assumed.
const testDeploy: DeployConfig = {
  changer: '/dev/sch0',
  drives: ['/dev/nst0', '/dev/nst1'],
  webhookUrl: 'https://discord.com/api/webhooks/1/a',
  opticalBurnDrives: ['/dev/sr0'],
  slotCount: 47,
  cleaningSlots: [45],
  ioStationSlots: [46, 47],
}

describe('buildConfig', () => {
  it('builds a schema-shaped config from the default form state', () => {
    const config = buildConfig(defaultFormState(), testDeploy)

    expect(config.copies).toBe(2)
    expect(config.library.tapeCapacityBytes).toBe(2_500_000_000_000)
    expect(config.redundancy).toEqual({ targetPercentage: 10, sliceSizeBytes: 4 * 1024 ** 3 })
    expect(config.delivery.opticalBurn).toBeUndefined()
  })

  it('fills the deploy-owned library devices and webhook from deploy config, not the form', () => {
    const config = buildConfig(defaultFormState(), testDeploy)

    expect(config.library.changer).toBe('/dev/sch0')
    expect(config.library.drives).toEqual(['/dev/nst0', '/dev/nst1'])
    expect(config.delivery.webhookUrl).toBe('https://discord.com/api/webhooks/1/a')
  })

  it('builds exactly one of zfsPath/k8s per source, never both', () => {
    const form = defaultFormState()
    form.sources = [
      { ...newSourceFormState(), type: 'zfs', zfsName: 'pool/data' },
      {
        ...newSourceFormState(),
        type: 'k8s',
        k8sKind: 'VolumeSnapshot',
        k8sSelection: 'name',
        k8sNamespace: 'media',
        k8sName: 'media-pvc',
      },
    ]

    const config = buildConfig(form, testDeploy)

    expect(config.sources[0].zfsPath).toEqual({ name: 'pool/data' })
    expect(config.sources[0].k8s).toBeUndefined()

    expect(config.sources[1].k8s).toEqual({
      apiVersion: 'snapshot.storage.k8s.io/v1',
      kind: 'VolumeSnapshot',
      namespace: 'media',
      name: 'media-pvc',
    })
    expect(config.sources[1].zfsPath).toBeUndefined()
  })

  it('builds a k8s source by label selector with exactly one of name/labelSelector', () => {
    const form = defaultFormState()
    form.sources = [
      {
        ...newSourceFormState(),
        type: 'k8s',
        k8sKind: 'VolumeGroupSnapshot',
        k8sSelection: 'labelSelector',
        k8sLabelSelector: 'app=plex',
      },
    ]

    const config = buildConfig(form, testDeploy)

    expect(config.sources[0].k8s).toEqual({
      apiVersion: 'groupsnapshot.storage.k8s.io/v1alpha1',
      kind: 'VolumeGroupSnapshot',
      labelSelector: 'app=plex',
    })
  })

  it('builds fillToCapacity redundancy instead of targetPercentage when that mode is selected', () => {
    const form = defaultFormState()
    form.redundancyMode = 'fillToCapacity'
    form.fillFloor = 7

    const config = buildConfig(form, testDeploy)

    expect(config.redundancy).toEqual({ fillToCapacity: { floor: 7 }, sliceSizeBytes: 4 * 1024 ** 3 })
    expect(config.redundancy.targetPercentage).toBeUndefined()
  })

  it('includes opticalBurn only when enabled, sourcing the burner drives from deploy config', () => {
    const form = defaultFormState()
    form.opticalBurnEnabled = true
    form.opticalCopies = 2

    const config = buildConfig(form, testDeploy)

    // drives come from deploy config (issue #317), not the form; copies and the
    // reclaim opt-out stay per-run.
    expect(config.delivery.opticalBurn).toEqual({
      drives: ['/dev/sr0'],
      copies: 2,
      allowNonBlankDiscs: false,
    })
  })

  it('filters blank deploy burner-drive entries out of the built optical-burn block', () => {
    const form = defaultFormState()
    form.opticalBurnEnabled = true
    form.opticalCopies = 2

    const config = buildConfig(form, { ...testDeploy, opticalBurnDrives: ['/dev/sr0', '', '  ', '/dev/sr1'] })

    expect(config.delivery.opticalBurn?.drives).toEqual(['/dev/sr0', '/dev/sr1'])
  })

  it('filters blank recipient entries out of the built config', () => {
    const form = defaultFormState()
    form.recipients = ['age1pq1abc', '', '  ']

    const config = buildConfig(form, testDeploy)

    expect(config.encryption.recipients).toEqual(['age1pq1abc'])
  })

  it('filters blank deploy drive entries out of the built config', () => {
    const config = buildConfig(defaultFormState(), {
      changer: '/dev/sch0',
      drives: ['/dev/nst0', '', '  '],
      webhookUrl: '',
      opticalBurnDrives: [],
      slotCount: 0,
      cleaningSlots: [],
      ioStationSlots: [],
    })

    expect(config.library.drives).toEqual(['/dev/nst0'])
  })

  it('drops blank slots outside the deployment topology so an out-of-range slot is never submitted', () => {
    const form = defaultFormState()
    // 45 is a cleaning slot, 46/47 the I/O-station, 99 out of range, and a dup 2.
    form.blankSlots = [1, 2, 45, 46, 47, 99, 2]

    const config = buildConfig(form, testDeploy)

    expect(config.library.blankSlots).toEqual([1, 2])
  })

  it('only dedups blank slots when the topology is unknown', () => {
    const form = defaultFormState()
    form.blankSlots = [1, 99, 1]

    const config = buildConfig(form, { ...testDeploy, slotCount: 0, cleaningSlots: [], ioStationSlots: [] })

    expect(config.library.blankSlots).toEqual([1, 99])
  })

  it('leaves the library changer empty and drives empty when the deployment is unconfigured', () => {
    // An unconfigured deployment yields empty deploy values; buildConfig stays
    // total (never throws) and the empty changer/drives then fail the schema
    // at the Review step rather than the SPA inventing a default.
    const config = buildConfig(defaultFormState(), {
      changer: '',
      drives: [],
      webhookUrl: '',
      opticalBurnDrives: [],
      slotCount: 0,
      cleaningSlots: [],
      ioStationSlots: [],
    })

    expect(config.library.changer).toBe('')
    expect(config.library.drives).toEqual([])
    expect(config.delivery.webhookUrl).toBe('')
  })
})

describe('configToFormState', () => {
  it('round-trips form-owned fields built by buildConfig back into an equivalent form state', () => {
    const form = defaultFormState()
    form.sources = [
      { ...newSourceFormState(), type: 'zfs', zfsName: 'pool/data', label: 'data' },
      {
        ...newSourceFormState(),
        type: 'k8s',
        k8sKind: 'VolumeSnapshot',
        k8sSelection: 'name',
        k8sNamespace: 'media',
        k8sName: 'media-pvc',
      },
    ]
    form.recipients = ['age1pq1abc']
    form.identity = 'AGE-SECRET-KEY-PQ-1abc'
    form.opticalBurnEnabled = true
    form.opticalCopies = 3

    const roundTripped = configToFormState(buildConfig(form, testDeploy))

    expect(roundTripped.sources).toHaveLength(2)
    expect(roundTripped.sources[0].type).toBe('zfs')
    expect(roundTripped.sources[0].zfsName).toBe('pool/data')
    expect(roundTripped.sources[0].label).toBe('data')
    expect(roundTripped.sources[1].type).toBe('k8s')
    expect(roundTripped.sources[1].k8sNamespace).toBe('media')
    expect(roundTripped.sources[1].k8sName).toBe('media-pvc')
    expect(roundTripped.recipients).toEqual(['age1pq1abc'])
    expect(roundTripped.identity).toBe('AGE-SECRET-KEY-PQ-1abc')
    expect(roundTripped.opticalBurnEnabled).toBe(true)
    expect(roundTripped.opticalCopies).toBe(3)
    expect(roundTripped.tapeGeneration).toBe('LTO-6')
  })

  it('falls back to the default LTO generation for a capacity with no exact match in the table', () => {
    const config: RunConfig = {
      sources: [],
      copies: 1,
      library: { changer: '/dev/sch0', drives: ['/dev/nst0'], blankSlots: [], tapeCapacityBytes: 999 },
      redundancy: { sliceSizeBytes: 1024 },
      encryption: { recipients: [], identity: '' },
      delivery: { webhookUrl: '' },
    }

    const form = configToFormState(config)

    expect(form.tapeGeneration).toBe('LTO-6')
  })

  it('never throws on an empty sources array or a config with no optical burn', () => {
    const config: RunConfig = {
      sources: [],
      copies: 1,
      library: { changer: '', drives: [], blankSlots: [], tapeCapacityBytes: 2_500_000_000_000 },
      redundancy: { sliceSizeBytes: 1 },
      encryption: { recipients: [], identity: '' },
      delivery: { webhookUrl: '' },
    }

    expect(() => configToFormState(config)).not.toThrow()
    expect(configToFormState(config).opticalBurnEnabled).toBe(false)
  })

  it('never throws on wrong-typed fields from a schema-invalid JSON document', () => {
    // A syntactically valid but schema-invalid document whose fields carry the
    // wrong runtime type (blankSlots a number, recipients a string, floor/copies
    // wrong-typed). configToFormState must degrade to safe values, and the built
    // config must not crash the page later (new Set(5) / "x".trim()).
    const bad = {
      sources: 'nope',
      copies: 'two',
      library: { blankSlots: 5, tapeCapacityBytes: 'big' },
      redundancy: { fillToCapacity: { floor: '3' }, sliceSizeBytes: 'x' },
      encryption: { recipients: 'age1abc', identity: 42 },
      delivery: { opticalBurn: { drives: [], copies: 'many' } },
    } as unknown as RunConfig

    let form!: ReturnType<typeof configToFormState>
    expect(() => {
      form = configToFormState(bad)
    }).not.toThrow()

    expect(Array.isArray(form.blankSlots)).toBe(true)
    expect(Array.isArray(form.recipients)).toBe(true)
    expect(form.opticalBurnEnabled).toBe(false)
    expect(() => buildConfig(form, testDeploy)).not.toThrow()
  })
})

describe('unmodeledFields', () => {
  const baseConfig: RunConfig = {
    sources: [{ zfsPath: { name: 'pool/data' } }],
    copies: 2,
    library: { changer: '/dev/sch0', drives: ['/dev/nst0'], blankSlots: [], tapeCapacityBytes: 2_500_000_000_000 },
    redundancy: { targetPercentage: 10, sliceSizeBytes: 1024 },
    encryption: { recipients: ['age1pq1abc'], identity: 'AGE-SECRET-KEY-PQ-1x' },
    delivery: { webhookUrl: 'https://discord.com/api/webhooks/1/a' },
  }

  it('returns nothing for a config with only form-modeled fields', () => {
    expect(unmodeledFields(baseConfig)).toEqual([])
  })

  it('names every advanced-only field present, as a dotted path', () => {
    const config: RunConfig = {
      ...baseConfig,
      feasibilityOverhead: 1.1,
      library: { ...baseConfig.library, ioWaitTimeoutSeconds: 3600, writeFailureWaitTimeoutSeconds: 7200 },
      delivery: {
        ...baseConfig.delivery,
        opticalBurn: { drives: ['/dev/sr0'], copies: 2, burnWaitTimeoutSeconds: 1800 },
      },
    }

    expect(unmodeledFields(config)).toEqual([
      'feasibilityOverhead',
      'library.ioWaitTimeoutSeconds',
      'library.writeFailureWaitTimeoutSeconds',
      'delivery.opticalBurn.burnWaitTimeoutSeconds',
    ])
  })

  it('re-building from the mapped form restores every other field when deploy config mirrors the deploy-owned values', () => {
    // The invariant the ConfigPage notice relies on: for a config with none of
    // the unmodeled fields, JSON -> Form -> JSON round-trips losslessly (modulo
    // defaulted booleans buildConfig always emits explicitly) — provided the
    // deploy config supplies the same deploy-owned device/webhook values the
    // JSON carried (issue #304), since Form mode now sources those from deploy
    // config rather than the JSON.
    const deploy: DeployConfig = {
      changer: baseConfig.library.changer,
      drives: baseConfig.library.drives,
      webhookUrl: baseConfig.delivery.webhookUrl,
      opticalBurnDrives: [],
      slotCount: 0,
      cleaningSlots: [],
      ioStationSlots: [],
    }

    const roundTripped = buildConfig(configToFormState(baseConfig), deploy)

    expect(roundTripped.feasibilityOverhead).toBeUndefined()
    expect(roundTripped.sources[0].zfsPath).toEqual({ name: 'pool/data' })
    expect(roundTripped.copies).toBe(2)
    expect(roundTripped.library.tapeCapacityBytes).toBe(2_500_000_000_000)
    expect(roundTripped.library.changer).toBe(baseConfig.library.changer)
    expect(roundTripped.library.drives).toEqual(baseConfig.library.drives)
    expect(roundTripped.encryption).toEqual(baseConfig.encryption)
    expect(roundTripped.delivery.webhookUrl).toBe(baseConfig.delivery.webhookUrl)
  })
})

describe('deployOwnedFields', () => {
  it('names the deploy-owned device/webhook/burner fields a JSON config sets, as dotted paths', () => {
    const config: RunConfig = {
      sources: [],
      copies: 1,
      library: { changer: '/dev/sch0', drives: ['/dev/nst0'], blankSlots: [], tapeCapacityBytes: 2_500_000_000_000 },
      redundancy: { sliceSizeBytes: 1 },
      encryption: { recipients: [], identity: '' },
      delivery: {
        webhookUrl: 'https://discord.com/api/webhooks/1/a',
        opticalBurn: { drives: ['/dev/sr0'], copies: 2 },
      },
    }

    expect(deployOwnedFields(config)).toEqual([
      'library.changer',
      'library.drives',
      'delivery.webhookUrl',
      'delivery.opticalBurn.drives',
    ])
  })

  it('returns nothing when the JSON leaves the deploy-owned fields empty', () => {
    const config: RunConfig = {
      sources: [],
      copies: 1,
      library: { changer: '', drives: [], blankSlots: [], tapeCapacityBytes: 2_500_000_000_000 },
      redundancy: { sliceSizeBytes: 1 },
      encryption: { recipients: [], identity: '' },
      delivery: { webhookUrl: '' },
    }

    expect(deployOwnedFields(config)).toEqual([])
  })
})

describe('blankSlotsCopiesIssue', () => {
  it('returns null when the blank-slot count is a positive multiple of copies', () => {
    expect(blankSlotsCopiesIssue(1, 1)).toBeNull()
    expect(blankSlotsCopiesIssue(2, 2)).toBeNull()
    expect(blankSlotsCopiesIssue(2, 4)).toBeNull()
    expect(blankSlotsCopiesIssue(3, 6)).toBeNull()
  })

  it('returns a message naming both counts when the blanks are not a multiple of copies', () => {
    const message = blankSlotsCopiesIssue(3, 5)

    expect(message).toContain('5')
    expect(message).toContain('3')
    expect(message).toMatch(/multiple of 3/i)
  })

  it('defers the empty-selection and copies < 1 cases to the schema validator, returning null', () => {
    // blankSlotCount 0 is covered by minItems=1; copies < 1 by minimum=1.
    expect(blankSlotsCopiesIssue(2, 0)).toBeNull()
    expect(blankSlotsCopiesIssue(0, 3)).toBeNull()
  })
})
