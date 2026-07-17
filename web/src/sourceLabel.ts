// LabeledSource is the minimal shape sourceLabel needs — the subset shared by
// configModel.Source (the config page) and ConfigSummary's RunConfigSource (the
// run-detail config viewer), so both surfaces derive a source's display
// identity from one place instead of two near-copies that can drift.
// internal/config.Source sets exactly one of zfsPath/k8s.
export interface LabeledSource {
  label?: string
  zfsPath?: { name: string }
  k8s?: { kind: string; namespace?: string; name?: string; labelSelector?: string }
}

// sourceLabel renders one config source's display identity: its own label when
// set, else a name derived from whichever of zfsPath/k8s it carries, else
// "(unlabeled)".
//
// `||`, not `??`, at every step: a present-but-empty field (e.g. a half-filled
// k8s ref's name) must fall through to the next candidate and ultimately
// "(unlabeled)", never render as a blank identity.
//
// `detail` selects the k8s rendering. The run-detail Sources list passes it to
// show the full `k8s · Kind/name (namespace)` form (matching the design); the
// config Review step's dense inline summary omits it to keep each entry to a
// bare name/selector so the comma-joined line stays compact.
export function sourceLabel(source: LabeledSource, opts?: { detail?: boolean }): string {
  // A JSON / paste-mode config can carry a malformed sources array whose elements
  // are null or non-objects (the mode does not validate shape — the server does,
  // on submit). Render such an element as the same "(unlabeled)" fallback a
  // fieldless source gets, rather than dereferencing it and crashing the caller's
  // render (ConfigReview) or summary.
  if (typeof source !== 'object' || source === null) {
    return '(unlabeled)'
  }

  if (source.label) {
    return source.label
  }

  if (source.zfsPath?.name) {
    return source.zfsPath.name
  }

  const k8sName = source.k8s?.name || source.k8s?.labelSelector || ''

  if (opts?.detail && source.k8s) {
    const namespace = source.k8s.namespace ? ` (${source.k8s.namespace})` : ''

    return `k8s · ${source.k8s.kind}/${k8sName}${namespace}`
  }

  return k8sName || '(unlabeled)'
}
