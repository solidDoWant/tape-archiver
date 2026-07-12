import { useEffect, useState } from 'react'
import { apiFetch } from './api'

// UiConfig mirrors pkg/runsapi's GET /api/config/ui JSON shape: server-provided
// deploy-time config the SPA needs to build outbound links. temporalUiBaseUrl
// is the browsable Temporal Web UI base (cmd/web's TEMPORAL_UI_URL); it is empty
// when the operator has not configured one, in which case the run overview's
// Temporal-workflow link is simply not shown.
export interface UiConfig {
  temporalUiBaseUrl: string
  temporalNamespace: string
}

export type UiConfigState =
  | { status: 'loading' }
  | { status: 'loaded'; config: UiConfig }
  | { status: 'error' }

// useUiConfig fetches GET /api/config/ui once on mount. Same one-shot,
// error-swallowing pattern as useBuildInfo (buildInfo.ts): a failure just means
// the outbound links it feeds are omitted, never a broken page.
export function useUiConfig(): UiConfigState {
  const [state, setState] = useState<UiConfigState>({ status: 'loading' })

  useEffect(() => {
    let cancelled = false

    apiFetch<UiConfig>('/api/config/ui')
      .then((config) => {
        if (!cancelled) {
          setState({ status: 'loaded', config })
        }
      })
      .catch(() => {
        if (!cancelled) {
          setState({ status: 'error' })
        }
      })

    return () => {
      cancelled = true
    }
  }, [])

  return state
}

// temporalWorkflowUrl builds the Temporal Web UI deep-link for one workflow
// execution, or null when no UI base URL is configured (so the caller renders
// no link). The path is Temporal's standard
// {base}/namespaces/{ns}/workflows/{workflowId}/{runId}/history. workflowId is
// the run's own Temporal WorkflowID (backup.WorkflowID = "backup" for every
// run), passed in from the run detail rather than hardcoded here.
export function temporalWorkflowUrl(
  config: UiConfig | undefined,
  workflowId: string,
  runId: string,
): string | null {
  if (!config || !config.temporalUiBaseUrl) {
    return null
  }

  const base = config.temporalUiBaseUrl.replace(/\/+$/, '')
  const namespace = encodeURIComponent(config.temporalNamespace || 'default')

  return `${base}/namespaces/${namespace}/workflows/${encodeURIComponent(workflowId)}/${encodeURIComponent(runId)}/history`
}
