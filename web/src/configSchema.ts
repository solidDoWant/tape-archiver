// configSchema.ts fetches the committed run-config JSON Schema (GET
// /api/config/schema — pkg/runsapi/configschema.go, which serves
// schemas/run-config.schema.json byte-for-byte) and validates an arbitrary
// value against it, so the config page's Form and JSON modes can both
// satisfy issue #279's acceptance criteria ("validates against the
// committed schema") without a second, hand-duplicated copy of the schema's
// constraints that could silently drift from it.
//
// This is a small, generic interpreter for exactly the subset of JSON
// Schema (2020-12) draft features the committed schema actually uses —
// object/array/string/integer/number/boolean "type", "properties",
// "required", "additionalProperties: false", "items", "$ref" (to a
// "$defs" sibling), "if"/"then" (used once, by K8sRef), and the numeric
// "multipleOf"/"minimum"/"maximum" keywords — not a general-purpose
// validator. It deliberately does not pull in a full JSON Schema library
// (e.g. ajv): the schema's own feature surface is tiny and stable (config
// regeneration is not expected for this issue), and this codebase already
// prefers a small hand-rolled implementation over a new dependency for a
// narrow, well-understood surface (see router.tsx's doc comment on the
// hand-rolled router, icons.tsx's hand-drawn icons).
//
// One thing this validator deliberately does NOT check: cross-field "exactly
// one of" invariants (Source's zfsPath/k8s, Redundancy's targetPercentage/
// fillToCapacity, K8sRef's name/labelSelector) documented in
// docs/configuration.md. The committed JSON Schema itself does not encode
// them either (only K8sRef's one-directional "if name then namespace" rule
// is expressed, via "if"/"then", and is checked here) — they are
// server-side internal/config.Parse invariants. Form mode's own UI
// structure (a single-choice toggle for each such pair) makes it impossible
// to build a form-constructed config that violates one; a JSON-mode config
// that does is still caught, exactly as before this issue, by the same
// POST /api/runs 400 response ConfigJsonMode.tsx already surfaces.

import { apiFetch } from './api'

export interface JSONSchema {
  $ref?: string
  $defs?: Record<string, JSONSchema>
  type?: string
  properties?: Record<string, JSONSchema>
  required?: string[]
  additionalProperties?: boolean
  items?: JSONSchema
  if?: JSONSchema
  then?: JSONSchema
  multipleOf?: number
  minimum?: number
  maximum?: number
}

export interface ValidationIssue {
  path: string
  message: string
}

// resolve follows a single $ref against defs. The committed schema never
// nests $refs more than one level deep (every $defs entry is a concrete
// object/array/string schema, not itself another bare $ref), so this is not
// recursive.
function resolve(schema: JSONSchema, defs: Record<string, JSONSchema>): JSONSchema {
  if (!schema.$ref) {
    return schema
  }

  const name = schema.$ref.replace('#/$defs/', '')
  const target = defs[name]

  if (!target) {
    throw new Error(`configSchema: unresolved $ref ${schema.$ref}`)
  }

  return target
}

function describePath(path: string, key: string): string {
  return path === '' ? key : `${path}.${key}`
}

function validateValue(
  schema: JSONSchema,
  defs: Record<string, JSONSchema>,
  value: unknown,
  path: string,
  issues: ValidationIssue[],
): void {
  const resolved = resolve(schema, defs)

  switch (resolved.type) {
    case 'object': {
      if (typeof value !== 'object' || value === null || Array.isArray(value)) {
        issues.push({ path, message: 'must be an object' })

        return
      }

      const object = value as Record<string, unknown>

      for (const key of resolved.required ?? []) {
        if (object[key] === undefined) {
          issues.push({ path: describePath(path, key), message: 'is required' })
        }
      }

      if (resolved.additionalProperties === false) {
        const allowed = new Set(Object.keys(resolved.properties ?? {}))

        for (const key of Object.keys(object)) {
          if (!allowed.has(key)) {
            issues.push({ path: describePath(path, key), message: 'is not a recognized field' })
          }
        }
      }

      for (const [key, propertySchema] of Object.entries(resolved.properties ?? {})) {
        if (object[key] !== undefined) {
          validateValue(propertySchema, defs, object[key], describePath(path, key), issues)
        }
      }

      if (resolved.if && resolved.then) {
        const ifResolved = resolve(resolved.if, defs)
        const conditionMet = (ifResolved.required ?? []).every((key) => object[key] !== undefined)

        if (conditionMet) {
          // "then" here (K8sRef's only use of if/then in the committed
          // schema) carries just a bare "required" list, no "type" — so
          // this checks it directly rather than recursing through
          // validateValue's type switch, which would otherwise treat a
          // type-less schema as unconstrained (the "default" case below)
          // and silently skip it.
          const thenResolved = resolve(resolved.then, defs)

          for (const key of thenResolved.required ?? []) {
            if (object[key] === undefined) {
              issues.push({ path: describePath(path, key), message: 'is required' })
            }
          }
        }
      }

      return
    }

    case 'array': {
      if (!Array.isArray(value)) {
        issues.push({ path, message: 'must be an array' })

        return
      }

      if (resolved.items) {
        value.forEach((item, index) => {
          validateValue(resolved.items as JSONSchema, defs, item, `${path}[${index}]`, issues)
        })
      }

      return
    }

    case 'string': {
      if (typeof value !== 'string') {
        issues.push({ path, message: 'must be a string' })
      }

      return
    }

    case 'boolean': {
      if (typeof value !== 'boolean') {
        issues.push({ path, message: 'must be true or false' })
      }

      return
    }

    case 'integer':
    case 'number': {
      if (typeof value !== 'number' || Number.isNaN(value)) {
        issues.push({ path, message: 'must be a number' })

        return
      }

      if (resolved.type === 'integer' && !Number.isInteger(value)) {
        issues.push({ path, message: 'must be a whole number' })
      }

      if (resolved.multipleOf !== undefined && value % resolved.multipleOf !== 0) {
        issues.push({ path, message: `must be a multiple of ${resolved.multipleOf}` })
      }

      if (resolved.minimum !== undefined && value < resolved.minimum) {
        issues.push({ path, message: `must be at least ${resolved.minimum}` })
      }

      if (resolved.maximum !== undefined && value > resolved.maximum) {
        issues.push({ path, message: `must be at most ${resolved.maximum}` })
      }

      return
    }

    default:
      // No "type" (or an unrecognized one): the committed schema never
      // omits "type" on a schema this validator would reach, so this is
      // unreachable in practice; treat as unconstrained rather than
      // throwing, so a future schema addition degrades to "unchecked" for
      // that one node instead of breaking validation entirely.
      return
  }
}

// validateAgainstSchema validates value against root (a full JSON Schema
// document with a top-level "$ref" into "$defs", exactly
// run-config.schema.json's own shape) and returns every issue found, empty
// when value is valid. Never throws for a well-formed schema document.
export function validateAgainstSchema(root: JSONSchema, value: unknown): ValidationIssue[] {
  const issues: ValidationIssue[] = []
  const defs = root.$defs ?? {}

  validateValue(root, defs, value, '', issues)

  return issues
}

let cachedSchema: Promise<JSONSchema> | null = null

// fetchConfigSchema fetches GET /api/config/schema once per page load and
// caches the in-flight/resolved promise, so every caller (Form mode's
// Review-step validation, JSON mode's live valid/invalid indicator) shares
// one network request rather than each re-fetching the same static
// document.
//
// It goes through apiFetch (not raw fetch) deliberately: the endpoint is
// session-gated like every other /api/* route, so a 401 here — an operator
// who sat on the config page past session expiry and then clicked Review,
// or whose JSON mode mounted after expiry — must trigger apiFetch's
// onSessionExpired routing back to the login page (issue #285) the same as
// any other data fetch, not dead-end in a component-level error.
export function fetchConfigSchema(): Promise<JSONSchema> {
  if (!cachedSchema) {
    cachedSchema = apiFetch<JSONSchema>('/api/config/schema')

    cachedSchema.catch(() => {
      // A failed fetch must not permanently poison the cache — the next
      // caller (e.g. a retry) gets a fresh fetch attempt instead of the
      // same rejected promise forever.
      cachedSchema = null
    })
  }

  return cachedSchema
}

// resetConfigSchemaCache clears fetchConfigSchema's cache. Test-only: each
// test that stubs fetch needs its own fresh fetch call rather than reusing
// whatever a previous test's stub already cached.
export function resetConfigSchemaCache(): void {
  cachedSchema = null
}
