import { afterEach, describe, expect, it, vi } from 'vitest'
import { fetchConfigSchema, resetConfigSchemaCache, validateAgainstSchema } from './configSchema'
import { testRunConfigSchema } from './testSchemaFixture'

const validConfig = {
  sources: [{ zfsPath: { name: 'bulk-pool-01/archive@snap' } }],
  copies: 2,
  library: {
    changer: '/dev/sch0',
    drives: ['/dev/nst0', '/dev/nst1'],
    blankSlots: [1, 2],
    tapeCapacityBytes: 2500000000000,
  },
  redundancy: { targetPercentage: 10, sliceSizeBytes: 1073741824 },
  encryption: {
    recipients: ['age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4'],
    identity: 'AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL',
  },
  delivery: { webhookUrl: 'https://discord.com/api/webhooks/123/abc' },
}

describe('validateAgainstSchema', () => {
  it('accepts a complete, valid config', () => {
    expect(validateAgainstSchema(testRunConfigSchema, validConfig)).toEqual([])
  })

  it('reports every missing required top-level field', () => {
    const issues = validateAgainstSchema(testRunConfigSchema, {})
    const paths = issues.map((issue) => issue.path).sort()

    expect(paths).toEqual(['copies', 'delivery', 'encryption', 'library', 'redundancy', 'sources'])
  })

  it('reports an unrecognized field (additionalProperties: false)', () => {
    const issues = validateAgainstSchema(testRunConfigSchema, { ...validConfig, bogus: true })

    expect(issues).toContainEqual({ path: 'bogus', message: 'is not a recognized field' })
  })

  it('reports a wrong-typed field with a nested path', () => {
    const issues = validateAgainstSchema(testRunConfigSchema, { ...validConfig, copies: 'two' })

    expect(issues).toContainEqual({ path: 'copies', message: 'must be a number' })
  })

  it('reports an out-of-range numeric constraint', () => {
    const issues = validateAgainstSchema(testRunConfigSchema, {
      ...validConfig,
      redundancy: { targetPercentage: 150, sliceSizeBytes: 1 },
    })

    expect(issues).toContainEqual({ path: 'redundancy.targetPercentage', message: 'must be at most 100' })
  })

  it('reports a non-integer value against a multipleOf: 1 constraint', () => {
    const issues = validateAgainstSchema(testRunConfigSchema, {
      ...validConfig,
      redundancy: { targetPercentage: 10.5, sliceSizeBytes: 1 },
    })

    expect(issues).toContainEqual({ path: 'redundancy.targetPercentage', message: 'must be a multiple of 1' })
  })

  it('reports each array item error at its own indexed path', () => {
    const issues = validateAgainstSchema(testRunConfigSchema, {
      ...validConfig,
      sources: [{ zfsPath: { name: 'ok' } }, { zfsPath: { name: 123 } }],
    })

    expect(issues).toContainEqual({ path: 'sources[1].zfsPath.name', message: 'must be a string' })
  })

  it("applies K8sRef's if/then: name without namespace is reported", () => {
    const issues = validateAgainstSchema(testRunConfigSchema, {
      ...validConfig,
      sources: [{ k8s: { apiVersion: 'snapshot.storage.k8s.io/v1', kind: 'VolumeSnapshot', name: 'x' } }],
    })

    expect(issues).toContainEqual({ path: 'sources[0].k8s.namespace', message: 'is required' })
  })

  it("K8sRef's if/then does not fire when name is absent (labelSelector-only ref)", () => {
    const issues = validateAgainstSchema(testRunConfigSchema, {
      ...validConfig,
      sources: [
        { k8s: { apiVersion: 'snapshot.storage.k8s.io/v1', kind: 'VolumeSnapshot', labelSelector: 'app=x' } },
      ],
    })

    expect(issues).toEqual([])
  })
})

describe('fetchConfigSchema', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    resetConfigSchemaCache()
  })

  it('fetches GET /api/config/schema and caches the result across calls', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => testRunConfigSchema,
    })
    vi.stubGlobal('fetch', fetchMock)

    const first = await fetchConfigSchema()
    const second = await fetchConfigSchema()

    expect(first).toEqual(testRunConfigSchema)
    expect(second).toBe(first)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock).toHaveBeenCalledWith('/api/config/schema')
  })

  it('does not poison the cache on a failed fetch', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: false, status: 500, json: async () => ({}) })
      .mockResolvedValueOnce({ ok: true, json: async () => testRunConfigSchema })
    vi.stubGlobal('fetch', fetchMock)

    await expect(fetchConfigSchema()).rejects.toThrow()

    const second = await fetchConfigSchema()
    expect(second).toEqual(testRunConfigSchema)
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })
})
