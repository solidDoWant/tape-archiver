// icons.tsx: a small set of inline SVGs standing in for the design's bare
// Unicode glyphs (◱ ▥ ＋ ☀ ☾ ◐ ⚠ etc.) — DESIGN_ANALYSIS.md §3/§6
// (oversight/iconography note) flagged those as inconsistent across
// browser/OS font stacks and recommended a real icon set. Rather than add a
// whole icon-library dependency for the handful of glyphs this foundational
// issue (#272) needs, these are hand-drawn, self-contained (no external
// asset, no extra npm dependency) and sized via currentColor/className like
// a typical icon component; later redesign issues (dashboard, run detail,
// tapes, config) can grow this file or introduce a library once the fuller
// icon surface is known.

export interface IconProps {
  className?: string
}

export function IconDashboard({ className }: IconProps) {
  return (
    <svg
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      className={className}
      aria-hidden="true"
    >
      <rect x="1.5" y="1.5" width="5.5" height="5.5" rx="1" />
      <rect x="9" y="1.5" width="5.5" height="5.5" rx="1" />
      <rect x="1.5" y="9" width="5.5" height="5.5" rx="1" />
      <rect x="9" y="9" width="5.5" height="5.5" rx="1" />
    </svg>
  )
}

export function IconPlus({ className }: IconProps) {
  return (
    <svg
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      className={className}
      aria-hidden="true"
    >
      <path d="M8 2.5v11M2.5 8h11" />
    </svg>
  )
}

export function IconTapes({ className }: IconProps) {
  return (
    <svg
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      className={className}
      aria-hidden="true"
    >
      <rect x="1.5" y="3" width="13" height="10" rx="1.5" />
      <circle cx="5.5" cy="8" r="1.6" />
      <circle cx="10.5" cy="8" r="1.6" />
    </svg>
  )
}

export function IconSun({ className }: IconProps) {
  return (
    <svg
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      className={className}
      aria-hidden="true"
    >
      <circle cx="8" cy="8" r="3" />
      <path d="M8 1.5v1.4M8 13.1v1.4M1.5 8h1.4M13.1 8h1.4M3.4 3.4l1 1M11.6 11.6l1 1M3.4 12.6l1-1M11.6 4.4l1-1" />
    </svg>
  )
}

export function IconMoon({ className }: IconProps) {
  return (
    <svg viewBox="0 0 16 16" fill="currentColor" className={className} aria-hidden="true">
      <path d="M13.5 9.7A6 6 0 0 1 6.3 2.5a6 6 0 1 0 7.2 7.2Z" />
    </svg>
  )
}

export function IconAuto({ className }: IconProps) {
  return (
    <svg
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.3"
      className={className}
      aria-hidden="true"
    >
      <circle cx="8" cy="8" r="6" />
      <path d="M8 2v12a6 6 0 0 0 0-12Z" fill="currentColor" stroke="none" />
    </svg>
  )
}

export function IconWarning({ className }: IconProps) {
  return (
    <svg
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden="true"
    >
      <path d="M8 1.6 14.8 13.4a1 1 0 0 1-.87 1.5H2.07a1 1 0 0 1-.87-1.5L8 1.6Z" />
      <path d="M8 6.2v3.2" />
      <circle cx="8" cy="11.6" r="0.2" fill="currentColor" stroke="none" />
    </svg>
  )
}

export function IconSpinner({ className }: IconProps) {
  return (
    <svg viewBox="0 0 16 16" fill="none" className={className} aria-hidden="true">
      <circle cx="8" cy="8" r="6.5" stroke="currentColor" strokeOpacity="0.25" strokeWidth="2" />
      <path d="M14.5 8A6.5 6.5 0 0 0 8 1.5" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
    </svg>
  )
}

export function IconLock({ className }: IconProps) {
  return (
    <svg
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden="true"
    >
      <rect x="3" y="7" width="10" height="7" rx="1.5" />
      <path d="M5.5 7V5a2.5 2.5 0 0 1 5 0v2" />
    </svg>
  )
}

export function IconBook({ className }: IconProps) {
  return (
    <svg
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden="true"
    >
      <path d="M8 3.5C6.8 2.7 5.3 2.3 3.5 2.3a1 1 0 0 0-1 1v8.4a1 1 0 0 0 1 1c1.8 0 3.3.4 4.5 1.2 1.2-.8 2.7-1.2 4.5-1.2a1 1 0 0 0 1-1V3.3a1 1 0 0 0-1-1c-1.8 0-3.3.4-4.5 1.2Z" />
      <path d="M8 3.5v10" />
    </svg>
  )
}
