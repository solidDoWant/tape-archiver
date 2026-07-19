import { useEffect, useState } from 'react'
import { apiFetch } from './api'

// BuildInfo mirrors cmd/web's GET /api/build-info JSON shape. Unlike every
// other /api/* route, this one is intentionally ungated (pkg/webauth.Wrap) —
// the footer it feeds renders on the (unauthenticated) login page too, and
// a build version/deploy label is not sensitive.
export interface BuildInfo {
  version: string
  footerHost?: string
}

export type BuildInfoState =
  | { status: 'loading' }
  | { status: 'loaded'; info: BuildInfo }
  | { status: 'error' }

// useBuildInfo fetches GET /api/build-info once on mount, for Footer.tsx.
export function useBuildInfo(): BuildInfoState {
  const [state, setState] = useState<BuildInfoState>({ status: 'loading' })

  useEffect(() => {
    let cancelled = false

    apiFetch<BuildInfo>('/api/build-info')
      .then((info) => {
        if (!cancelled) {
          setState({ status: 'loaded', info })
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
