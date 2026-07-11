import { useEffect, useState } from 'react'
import { apiFetch, ApiError, describeNetworkError } from './api'

// Config mirrors internal/config.Config's JSON shape as returned by GET
// /api/runs/{runID}/config (pkg/runsapi/config.go) — secrets
// (Encryption.Identity, Delivery.WebhookURL) already redacted server-side.
// Only the fields this summary actually renders are declared; the rest pass
// through untyped into the raw-JSON view below, so this stays in sync with
// server-side additions without needing a matching client-side field for
// every one of them.
export interface RunConfigSource {
  compression?: boolean
  label?: string
  zfsPath?: { name: string }
  k8s?: { kind: string; namespace: string; name?: string; labelSelector?: string }
}

export interface RunConfig {
  sources: RunConfigSource[]
  copies: number
  redundancy: { targetPercentage?: number }
  [key: string]: unknown
}

interface RunConfigResponse {
  runId: string
  config: RunConfig
}

type ConfigState =
  | { status: 'loading' }
  | { status: 'unavailable' }
  | { status: 'error'; message: string }
  | { status: 'ready'; config: RunConfig }

// sourceLabel renders one config source's identity the way DESIGN_ANALYSIS.md
// §2.B's Sources list does: the source's own label when set, else a
// derived name from whichever of ZFSPath/K8s it carries (internal/config.Source:
// exactly one of the two is ever set).
function sourceLabel(source: RunConfigSource): string {
  if (source.label) {
    return source.label
  }

  if (source.zfsPath) {
    return source.zfsPath.name
  }

  if (source.k8s) {
    const selector = source.k8s.name ?? source.k8s.labelSelector ?? ''

    return `k8s · ${source.k8s.kind}/${selector} (${source.k8s.namespace})`
  }

  return 'source'
}

// compressionLabel mirrors internal/config.Source.Compression's documented
// default: unset (undefined) means compression is ON, so only an explicit
// `false` reads as "raw".
function compressionLabel(source: RunConfigSource): string {
  return source.compression === false ? 'raw' : 'zstd'
}

export interface ConfigSummaryProps {
  runId: string
  // logicalTapes/copies are the Pack phase's own observed facts (GET
  // /api/runs/{runID}/phases' Pack entry, "Logical tapes"/"Copies" —
  // facts.go's packFacts), passed down from RunOverview rather than
  // re-derived here: the *submitted* config's copies count and the *packed*
  // plan's copies are the same value by construction, but the Pack phase is
  // the actual observed result (SPEC §4.2 — never persisted state), so it is
  // preferred over the submitted config whenever it is available. Undefined
  // before Pack completes.
  logicalTapes?: number
  copies?: number
}

// ConfigSummary is the run detail page's run-config viewer (issue #277): the
// exact submitted configuration (GET /api/runs/{runID}/config,
// pkg/runsapi/config.go — secrets already redacted server-side), rendered as
// a physical-tapes/redundancy stat pair, a human-readable sources list
// (DESIGN_ANALYSIS.md §2.B's "Sources" card), and the full config as
// formatted JSON behind a disclosure — the JSON view exists so every
// submitted field (library device paths, encryption recipients, delivery
// settings, etc.) stays inspectable without this component growing a
// bespoke renderer for each one.
function ConfigSummary({ runId, logicalTapes, copies }: ConfigSummaryProps) {
  const [state, setState] = useState<ConfigState>({ status: 'loading' })

  useEffect(() => {
    let cancelled = false

    apiFetch<RunConfigResponse>(`/api/runs/${encodeURIComponent(runId)}/config`)
      .then((response) => {
        if (!cancelled) {
          setState({ status: 'ready', config: response.config })
        }
      })
      .catch((error: unknown) => {
        if (cancelled) {
          return
        }

        // 410 and 404 deliberately share the "no longer available" copy
        // here, unlike RunDetail.tsx's page-level not-found/aged-out
        // taxonomy: by the time this panel renders, the page has already
        // confirmed the run exists, so either status can only mean the
        // history became unreconstructable mid-view (aged out, or fell out
        // of visibility entirely) — the distinction was already drawn where
        // it matters, and neither is operator-actionable from this panel.
        if (error instanceof ApiError && (error.status === 410 || error.status === 404)) {
          setState({ status: 'unavailable' })

          return
        }

        const message = error instanceof ApiError ? error.message : describeNetworkError(error)
        setState({ status: 'error', message })
      })

    return () => {
      cancelled = true
    }
  }, [runId])

  if (state.status === 'loading') {
    return <p className="text-[12px] text-text-faint">Loading configuration…</p>
  }

  if (state.status === 'unavailable') {
    return (
      <p className="rounded-xl border border-dashed border-border-strong bg-surface-2 p-3 text-[12px] text-text-dim">
        The submitted configuration is no longer available for this run.
      </p>
    )
  }

  if (state.status === 'error') {
    return (
      <p role="alert" className="rounded-xl border border-red-line bg-red-bg p-3 text-[12px] text-red">
        {state.message}
      </p>
    )
  }

  const { config } = state
  const physicalTapes = typeof logicalTapes === 'number' && typeof copies === 'number' ? logicalTapes * copies : undefined
  const redundancyPercent = config.redundancy?.targetPercentage

  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-2 gap-3">
        <div className="rounded-xl border border-border bg-surface p-4 shadow-card">
          <div className="font-mono text-[11px] tracking-[0.05em] text-text-faint">PHYSICAL TAPES</div>
          <div className="mt-1.5 text-2xl font-bold tracking-tight">{physicalTapes ?? '—'}</div>
          <div className="mt-0.5 text-[11px] text-text-dim">
            {typeof logicalTapes === 'number' && typeof copies === 'number'
              ? `${logicalTapes} logical × ${copies} ${copies === 1 ? 'copy' : 'copies'}`
              : 'not yet packed'}
          </div>
        </div>
        <div className="rounded-xl border border-border bg-surface p-4 shadow-card">
          <div className="font-mono text-[11px] tracking-[0.05em] text-text-faint">REDUNDANCY</div>
          <div className="mt-1.5 text-2xl font-bold tracking-tight">
            {typeof redundancyPercent === 'number' ? (
              <>
                {redundancyPercent}
                <span className="text-sm font-semibold text-text-dim">%</span>
              </>
            ) : (
              '—'
            )}
          </div>
          <div className="mt-0.5 text-[11px] text-text-dim">PAR2 · post-quantum age</div>
        </div>
      </div>

      <div className="rounded-xl border border-border bg-surface p-4 shadow-card">
        <div className="mb-2.5 font-mono text-[11px] tracking-[0.05em] text-text-faint">SOURCES · {config.sources.length}</div>
        {config.sources.length === 0 ? (
          <p className="text-[12px] text-text-faint">No sources.</p>
        ) : (
          <ul className="flex flex-col divide-y divide-border">
            {config.sources.map((source, index) => (
              <li key={index} className="flex items-center gap-3 py-2 text-[12px]">
                <span className="min-w-0 flex-1 truncate font-mono">{sourceLabel(source)}</span>
                <span
                  className={`shrink-0 rounded px-1.5 py-0.5 text-[11px] ${
                    compressionLabel(source) === 'zstd' ? 'bg-green-bg text-green' : 'bg-inset text-text-faint'
                  }`}
                >
                  {compressionLabel(source)}
                </span>
              </li>
            ))}
          </ul>
        )}
      </div>

      <details className="rounded-xl border border-border bg-surface p-4 shadow-card">
        <summary className="cursor-pointer text-[12px] font-medium text-text-dim">View submitted configuration (JSON)</summary>
        <pre className="mt-3 max-w-full overflow-x-auto rounded-lg border border-console-border bg-console-bg p-3 font-mono text-[11px] text-console-text">
          {JSON.stringify(config, null, 2)}
        </pre>
      </details>
    </div>
  )
}

export default ConfigSummary
