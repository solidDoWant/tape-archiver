import { useEffect, useState } from 'react'
import { apiFetch } from './api'

// RunDeliveryResponse mirrors pkg/runsapi's GET /api/runs/{runID}/delivery JSON
// shape (delivery.go): the Discord jump-to-message deep-link for this run's
// posted report (https://discord.com/channels/{guild}/{channel}/{message}),
// reconstructed from the Deliver activity's result in workflow history. messageUrl
// is "" when there is nothing to link to — delivery disabled, not yet reached,
// failed, or an identity that could not be fully resolved.
interface RunDeliveryResponse {
  runId: string
  messageUrl: string
}

// useReportMessageUrl fetches the run's Discord report deep-link from GET
// /api/runs/{runID}/delivery, returning the message URL or null. Same
// self-contained, error-swallowing pattern as the run overview's other panels
// (TapesSection, useUiConfig): a failure — or a run that delivered no report —
// simply omits the "Discord report" link, never breaks the view. It refetches
// when `terminal` flips true so the link appears once the just-completed run's
// Deliver phase (the final phase) is recorded in history; a still-running run has
// no posted report yet, so it resolves to null until then.
export function useReportMessageUrl(runId: string, terminal: boolean): string | null {
  const [url, setUrl] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false

    apiFetch<RunDeliveryResponse>(`/api/runs/${encodeURIComponent(runId)}/delivery`)
      .then((response) => {
        if (!cancelled) {
          setUrl(response.messageUrl || null)
        }
      })
      .catch(() => {
        if (!cancelled) {
          setUrl(null)
        }
      })

    return () => {
      cancelled = true
    }
  }, [runId, terminal])

  return url
}
