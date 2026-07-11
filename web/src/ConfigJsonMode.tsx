import { useEffect, useState, type ChangeEvent } from 'react'
import { fetchConfigSchema, validateAgainstSchema, type JSONSchema, type ValidationIssue } from './configSchema'

export interface ConfigJsonModeProps {
  text: string
  onTextChange: (text: string) => void
}

type LiveCheck =
  | { status: 'empty' }
  | { status: 'parse-error'; message: string }
  | { status: 'checking' }
  | { status: 'valid'; sourceCount: number; copies: number }
  | { status: 'invalid'; issues: ValidationIssue[] }

// checkText parses text as JSON and, when schema is available, validates it
// against the committed run-config schema — the same validator Form mode's
// Review step uses (configSchema.ts), so the two modes can never disagree
// about what "valid" means.
function checkText(text: string, schema: JSONSchema | null): LiveCheck {
  if (text.trim() === '') {
    return { status: 'empty' }
  }

  let parsed: unknown

  try {
    parsed = JSON.parse(text)
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error)

    return { status: 'parse-error', message }
  }

  if (!schema) {
    return { status: 'checking' }
  }

  const issues = validateAgainstSchema(schema, parsed)

  if (issues.length > 0) {
    return { status: 'invalid', issues }
  }

  const config = parsed as { sources?: unknown[]; copies?: number }

  return { status: 'valid', sourceCount: config.sources?.length ?? 0, copies: config.copies ?? 0 }
}

// ConfigJsonMode is the config page's JSON mode: paste/upload a run-config
// JSON document, with a live valid/invalid indicator — functionally the
// same paste/upload flow the former SubmitRunForm.tsx offered (issue #279's
// acceptance criterion: "submission behaves as it does today"), now with a
// schema-validated live indicator (DESIGN_ANALYSIS.md §2 "D. Config" JSON
// mode). The dry-run toggle and Submit control live in ConfigPage's shared
// sticky action bar, not here, since Form mode's Review step uses the same
// bar — see ConfigPage.tsx.
function ConfigJsonMode({ text, onTextChange }: ConfigJsonModeProps) {
  const [schema, setSchema] = useState<JSONSchema | null>(null)

  useEffect(() => {
    let cancelled = false

    fetchConfigSchema()
      .then((fetched) => {
        if (!cancelled) {
          setSchema(fetched)
        }
      })
      .catch(() => {
        // Schema fetch failure degrades the live indicator to "checking"
        // forever rather than blocking the textarea itself — submission
        // still goes through the server's own config.Parse validation
        // regardless of whether this client-side indicator could load.
      })

    return () => {
      cancelled = true
    }
  }, [])

  const check = checkText(text, schema)

  const handleFileChange = (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]

    event.target.value = ''

    if (!file) {
      return
    }

    file
      .text()
      .then((fileText) => onTextChange(fileText))
      .catch(() => {
        // Read failures are rare (a file picker only ever hands back a
        // readable File); leaving the textarea untouched keeps whatever was
        // typed there rather than clobbering it with nothing.
      })
  }

  return (
    <div className="overflow-hidden rounded-xl border border-border bg-surface shadow-card">
      <div className="flex items-center gap-2.5 border-b border-border bg-surface-2 px-4 py-3">
        <label htmlFor="run-config" className="font-mono text-[11px] text-text-dim">
          Run config (JSON)
        </label>
        <span className="flex-1" />
        <input
          type="file"
          accept="application/json,.json"
          onChange={handleFileChange}
          aria-label="Upload run config file"
          className="text-[11px] text-text-dim file:mr-2 file:rounded-md file:border file:border-border-strong file:bg-surface file:px-2.5 file:py-1 file:text-[11px] file:text-text"
        />
      </div>

      <textarea
        id="run-config"
        value={text}
        onChange={(event) => onTextChange(event.target.value)}
        rows={16}
        spellCheck={false}
        placeholder="Paste run-config JSON here, or upload a file above."
        className="w-full resize-y border-none bg-console-bg p-4 font-mono text-[12px] leading-relaxed text-console-text outline-none"
      />

      <div className="flex items-center gap-2 border-t border-border px-4 py-2.5">
        {check.status === 'empty' ? <span className="font-mono text-[11px] text-text-faint">no config yet</span> : null}

        {check.status === 'checking' ? (
          <span className="font-mono text-[11px] text-text-faint">checking…</span>
        ) : null}

        {check.status === 'parse-error' ? (
          <>
            <span className="h-2 w-2 flex-none rounded-full bg-red" aria-hidden="true" />
            <span role="alert" className="font-mono text-[11px] text-red">
              invalid JSON: {check.message}
            </span>
          </>
        ) : null}

        {check.status === 'invalid' ? (
          <>
            <span className="h-2 w-2 flex-none rounded-full bg-red" aria-hidden="true" />
            <span role="alert" className="font-mono text-[11px] text-red">
              invalid · {check.issues[0].path || '(root)'} {check.issues[0].message}
              {check.issues.length > 1 ? ` (+${check.issues.length - 1} more)` : ''}
            </span>
          </>
        ) : null}

        {check.status === 'valid' ? (
          <>
            <span className="h-2 w-2 flex-none rounded-full bg-green" aria-hidden="true" />
            <span className="font-mono text-[11px] text-green">
              valid · {check.sourceCount} source(s) · {check.copies} cop{check.copies === 1 ? 'y' : 'ies'} ·
              schema draft 2020-12
            </span>
          </>
        ) : null}
      </div>
    </div>
  )
}

export default ConfigJsonMode
