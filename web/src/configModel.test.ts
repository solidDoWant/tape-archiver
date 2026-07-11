import { describe, expect, it } from 'vitest'
import { buildConfig, configToFormState, defaultFormState, newSourceFormState, type RunConfig } from './configModel'

describe('buildConfig', () => {
  it('builds a schema-shaped config from the default form state', () => {
    const config = buildConfig(defaultFormState())

    expect(config.copies).toBe(2)
    expect(config.library.tapeCapacityBytes).toBe(2_500_000_000_000)
    expect(config.redundancy).toEqual({ targetPercentage: 10, sliceSizeBytes: 4 * 1024 ** 3 })
    expect(config.delivery.opticalBurn).toBeUndefined()
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

    const config = buildConfig(form)

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

    const config = buildConfig(form)

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

    const config = buildConfig(form)

    expect(config.redundancy).toEqual({ fillToCapacity: { floor: 7 }, sliceSizeBytes: 4 * 1024 ** 3 })
    expect(config.redundancy.targetPercentage).toBeUndefined()
  })

  it('includes opticalBurn only when enabled', () => {
    const form = defaultFormState()
    form.opticalBurnEnabled = true
    form.opticalDrives = ['/dev/sr0']
    form.opticalCopies = 2

    const config = buildConfig(form)

    expect(config.delivery.opticalBurn).toEqual({
      drives: ['/dev/sr0'],
      copies: 2,
      allowNonBlankDiscs: false,
    })
  })

  it('filters blank recipient/drive entries out of the built config', () => {
    const form = defaultFormState()
    form.recipients = ['age1pq1abc', '', '  ']
    form.drives = ['/dev/nst0', '']

    const config = buildConfig(form)

    expect(config.encryption.recipients).toEqual(['age1pq1abc'])
    expect(config.library.drives).toEqual(['/dev/nst0'])
  })
})

describe('configToFormState', () => {
  it('round-trips a full config built by buildConfig back into an equivalent form state', () => {
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
    form.webhookUrl = 'https://discord.com/api/webhooks/1/a'
    form.opticalBurnEnabled = true
    form.opticalDrives = ['/dev/sr0']
    form.opticalCopies = 3

    const roundTripped = configToFormState(buildConfig(form))

    expect(roundTripped.sources).toHaveLength(2)
    expect(roundTripped.sources[0].type).toBe('zfs')
    expect(roundTripped.sources[0].zfsName).toBe('pool/data')
    expect(roundTripped.sources[0].label).toBe('data')
    expect(roundTripped.sources[1].type).toBe('k8s')
    expect(roundTripped.sources[1].k8sNamespace).toBe('media')
    expect(roundTripped.sources[1].k8sName).toBe('media-pvc')
    expect(roundTripped.recipients).toEqual(['age1pq1abc'])
    expect(roundTripped.identity).toBe('AGE-SECRET-KEY-PQ-1abc')
    expect(roundTripped.webhookUrl).toBe('https://discord.com/api/webhooks/1/a')
    expect(roundTripped.opticalBurnEnabled).toBe(true)
    expect(roundTripped.opticalDrives).toEqual(['/dev/sr0'])
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
})
