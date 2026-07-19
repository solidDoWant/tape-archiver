import { useBuildInfo } from './buildInfo'

// Footer renders the deploy-time build-version/host line the design shows
// on the login page (Login.dc.html's persistent "tape-archiver v0.4.1"
// footer) — see docs/configuration.md's WEB_FOOTER_HOST. The version
// segment is always the real build version (internal/buildinfo.ToolVersion,
// via GET /api/build-info), never the design's literal "v0.4.1" sample
// string. The host segment (the design's dead, hardcoded
// `hostLine: 'homelab · 10.0.0.4'` — DESIGN_ANALYSIS.md §4/§6 oversight 5)
// only renders when WEB_FOOTER_HOST is configured, and is omitted entirely —
// not a blank "tape-archiver v0.4.1 · " placeholder — when it is not
// (issue #272's acceptance criterion). Renders nothing while the fetch is in
// flight or fails, rather than a flash of placeholder text.
function Footer({ className = '' }: { className?: string }) {
  const state = useBuildInfo()

  if (state.status !== 'loaded') {
    return null
  }

  return (
    <div className={`font-mono text-[9.5px] tracking-wide text-text-faint ${className}`}>
      tape-archiver {state.info.version}
      {state.info.footerHost ? ` · ${state.info.footerHost}` : ''}
    </div>
  )
}

export default Footer
