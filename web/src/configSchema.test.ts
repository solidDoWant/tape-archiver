import { afterEach, describe, expect, it, vi } from 'vitest'
import { onSessionExpired } from './api'
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

  it('reports a present-but-empty required string (minLength=1) — the core issue #321 gap', () => {
    // library.changer, encryption.identity, and a source's zfsPath.name are all
    // required AND minLength=1: a present-but-empty "" passes "required" (the key
    // exists) but must be caught by minLength.
    const issues = validateAgainstSchema(testRunConfigSchema, {
      ...validConfig,
      library: { ...validConfig.library, changer: '' },
      encryption: { ...validConfig.encryption, identity: '' },
      sources: [{ zfsPath: { name: '' } }],
    })

    expect(issues).toContainEqual({ path: 'library.changer', message: 'must not be empty' })
    expect(issues).toContainEqual({ path: 'encryption.identity', message: 'must not be empty' })
    expect(issues).toContainEqual({ path: 'sources[0].zfsPath.name', message: 'must not be empty' })
  })

  it('reports a present-but-empty required array (minItems=1)', () => {
    // drives / blankSlots / recipients / sources are required AND minItems=1: an
    // empty [] passes "required" but must be caught by minItems.
    const issues = validateAgainstSchema(testRunConfigSchema, {
      ...validConfig,
      library: { ...validConfig.library, drives: [], blankSlots: [] },
      encryption: { ...validConfig.encryption, recipients: [] },
      sources: [],
    })

    expect(issues).toContainEqual({ path: 'library.drives', message: 'must have at least one item' })
    expect(issues).toContainEqual({ path: 'library.blankSlots', message: 'must have at least one item' })
    expect(issues).toContainEqual({ path: 'encryption.recipients', message: 'must have at least one item' })
    expect(issues).toContainEqual({ path: 'sources', message: 'must have at least one item' })
  })

  it('reports copies below its minimum (0 is no longer accepted)', () => {
    const issues = validateAgainstSchema(testRunConfigSchema, { ...validConfig, copies: 0 })

    expect(issues).toContainEqual({ path: 'copies', message: 'must be at least 1' })
  })

  it("applies K8sRef's if/then: name without namespace is reported", () => {
    const issues = validateAgainstSchema(testRunConfigSchema, {
      ...validConfig,
      sources: [{ k8s: { apiVersion: 'snapshot.storage.k8s.io/v1', kind: 'VolumeSnapshot', name: 'x' } }],
    })

    expect(issues).toContainEqual({ path: 'sources[0].k8s.namespace', message: 'is required' })
  })

  it('reports a present-but-empty k8s name (minLength=1) — not just the namespace', () => {
    // The guided form's by-name selection emits a present-but-empty name:"" for
    // an unfilled name; without minLength on name, only the if/then namespace
    // requirement fired, so the empty name itself went unvalidated.
    const issues = validateAgainstSchema(testRunConfigSchema, {
      ...validConfig,
      sources: [{ k8s: { apiVersion: 'snapshot.storage.k8s.io/v1', kind: 'VolumeSnapshot', namespace: 'ns', name: '' } }],
    })

    expect(issues).toContainEqual({ path: 'sources[0].k8s.name', message: 'must not be empty' })
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
    expect(fetchMock).toHaveBeenCalledWith('/api/config/schema', undefined)
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

  // issue #285 (PR #297 review): /api/config/schema is session-gated like
  // every other /api/* route, and this was the one call site not going
  // through apiFetch — a schema fetch after mid-session expiry must trigger
  // the same session-loss notification as any other data fetch, not
  // dead-end in a component-level validation error or a silently-stuck
  // JSON-mode validity indicator.
  it('notifies onSessionExpired subscribers when the schema fetch 401s', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ ok: false, status: 401, json: async () => ({ error: 'unauthorized' }) }),
    )

    const listener = vi.fn()
    const unsubscribe = onSessionExpired(listener)

    await expect(fetchConfigSchema()).rejects.toMatchObject({ status: 401 })

    expect(listener).toHaveBeenCalledTimes(1)
    unsubscribe()
  })
})
