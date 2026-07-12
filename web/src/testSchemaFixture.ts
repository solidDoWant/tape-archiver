// testSchemaFixture.ts is a test-only copy of schemas/run-config.schema.json
// (kept in sync by hand — the same document GET /api/config/schema serves in
// production), used by configSchema.test.ts and any component test that
// needs to stub that endpoint's response without a real backend. Production
// code never imports this file; it always fetches the live endpoint
// (configSchema.ts's fetchConfigSchema), which in turn serves the real
// embedded schemas/run-config.schema.json (pkg/runsapi/configschema.go) — so
// this fixture only needs to stay accurate enough to exercise the
// validator's logic in tests, not to be a build-time source of truth.
import type { JSONSchema } from './configSchema'

export const testRunConfigSchema: JSONSchema = {
  $ref: '#/$defs/Config',
  $defs: {
    Config: {
      type: 'object',
      additionalProperties: false,
      required: ['sources', 'copies', 'library', 'redundancy', 'encryption', 'delivery'],
      properties: {
        sources: { type: 'array', minItems: 1, items: { $ref: '#/$defs/Source' } },
        copies: { type: 'integer', minimum: 1 },
        library: { $ref: '#/$defs/Library' },
        redundancy: { $ref: '#/$defs/Redundancy' },
        encryption: { $ref: '#/$defs/Encryption' },
        delivery: { $ref: '#/$defs/Delivery' },
        feasibilityOverhead: { type: 'number' },
      },
    },
    Delivery: {
      type: 'object',
      additionalProperties: false,
      required: ['webhookUrl'],
      properties: {
        webhookUrl: { type: 'string' },
        opticalBurn: { $ref: '#/$defs/OpticalBurn' },
      },
    },
    Encryption: {
      type: 'object',
      additionalProperties: false,
      required: ['recipients', 'identity'],
      properties: {
        recipients: { type: 'array', minItems: 1, items: { type: 'string' } },
        identity: { type: 'string', minLength: 1 },
      },
    },
    FillConfig: {
      type: 'object',
      additionalProperties: false,
      required: ['floor'],
      properties: {
        floor: { type: 'number', multipleOf: 1, minimum: 1, maximum: 100 },
      },
    },
    K8sRef: {
      type: 'object',
      additionalProperties: false,
      required: ['apiVersion', 'kind'],
      if: { required: ['name'] },
      then: { required: ['namespace'] },
      properties: {
        apiVersion: { type: 'string', minLength: 1 },
        kind: { type: 'string', minLength: 1 },
        namespace: { type: 'string' },
        name: { type: 'string', minLength: 1 },
        labelSelector: { type: 'string', minLength: 1 },
      },
    },
    Library: {
      type: 'object',
      additionalProperties: false,
      required: ['changer', 'drives', 'blankSlots', 'tapeCapacityBytes'],
      properties: {
        changer: { type: 'string', minLength: 1 },
        drives: { type: 'array', minItems: 1, items: { type: 'string' } },
        blankSlots: { type: 'array', minItems: 1, items: { type: 'integer' } },
        tapeCapacityBytes: { type: 'integer', minimum: 1 },
        ioWaitTimeoutSeconds: { type: 'integer' },
        writeFailureWaitTimeoutSeconds: { type: 'integer' },
        allowNonBlankTapes: { type: 'boolean' },
      },
    },
    OpticalBurn: {
      type: 'object',
      additionalProperties: false,
      required: ['drives', 'copies'],
      properties: {
        drives: { type: 'array', items: { type: 'string' } },
        copies: { type: 'integer' },
        allowNonBlankDiscs: { type: 'boolean' },
        burnWaitTimeoutSeconds: { type: 'integer' },
      },
    },
    Redundancy: {
      type: 'object',
      additionalProperties: false,
      required: ['sliceSizeBytes'],
      properties: {
        targetPercentage: { type: 'number', multipleOf: 1, minimum: 1, maximum: 100 },
        fillToCapacity: { $ref: '#/$defs/FillConfig' },
        sliceSizeBytes: { type: 'integer', minimum: 1 },
      },
    },
    Source: {
      type: 'object',
      additionalProperties: false,
      properties: {
        compression: { type: 'boolean' },
        k8s: { $ref: '#/$defs/K8sRef' },
        zfsPath: { $ref: '#/$defs/ZFSPathSource' },
        label: { type: 'string' },
      },
    },
    ZFSPathSource: {
      type: 'object',
      additionalProperties: false,
      required: ['name'],
      properties: {
        name: { type: 'string', minLength: 1 },
      },
    },
  },
}
