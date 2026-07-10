import { Link } from './router'
import { useRoute } from './route'
import { useActiveRun } from './activeRun'
import type { Identity } from './identity'
import type { ThemePreference } from './theme'
import Footer from './Footer'
import { IconAuto, IconDashboard, IconLock, IconMoon, IconPlus, IconSun, IconTapes } from './icons'

interface SidebarProps {
  identity: Identity
  preference: ThemePreference
  onPreferenceChange: (preference: ThemePreference) => void
}

function navItemClasses(active: boolean): string {
  const base = 'flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-[13px] transition-colors'

  return active ? `${base} bg-nav-active text-text` : `${base} text-text hover:bg-nav-hover`
}

// initials renders a short avatar label from whatever identity info is
// available — a name's first two initials when present, otherwise the
// first two characters of the subject (always present, per
// pkg/webauth.Identity).
function initials({ name, subject }: Identity): string {
  const source = name?.trim() || subject

  const parts = source.split(/\s+/).filter(Boolean)
  if (parts.length >= 2) {
    return (parts[0][0] + parts[1][0]).toUpperCase()
  }

  return source.slice(0, 2).toUpperCase()
}

const themeOptions: { value: ThemePreference; label: string; Icon: typeof IconSun }[] = [
  { value: 'light', label: 'Light', Icon: IconSun },
  { value: 'dark', label: 'Dark', Icon: IconMoon },
  { value: 'auto', label: 'Auto', Icon: IconAuto },
]

// Sidebar is the app shell's persistent left navigation (Tape
// Archiver.dc.html — issue #272): brand mark, Dashboard/"Start new
// run"/Tapes nav items, the Light/Dark/Auto theme control, the signed-in
// operator's identity chip, and the build-version footer.
//
// Nav item routing note: the design's Dashboard and Tapes pages, and a
// dedicated "run submission" flow distinct from today's JSON-only form, are
// separate, later issues (DESIGN_ANALYSIS.md §7's implementation-shape
// split; this issue's non-goals). Until they land: "Dashboard" routes to
// the existing run-history page (today's closest functional equivalent —
// a paginated run list), "Start new run" routes to the existing
// SubmitRunForm at "/", and "Tapes" routes to a new minimal placeholder
// (TapesPage.tsx) so the nav item has somewhere real to go.
function Sidebar({ identity, preference, onPreferenceChange }: SidebarProps) {
  const route = useRoute()
  const activeRunState = useActiveRun()

  const runActive = activeRunState.status === 'loaded' && activeRunState.activeRun !== null

  // Narrow viewports (below md) stack the sidebar as a full-width block
  // above the content instead of a fixed 232px column — the design itself
  // is desktop-only (a fixed 1180px canvas, no media queries), and the
  // owner decision (docs/web-ui-design.md §9, 2026-07-10) is to adapt it by
  // stacking rather than design a separate mobile UI.
  return (
    <aside className="flex w-full flex-none flex-col border-b border-border bg-surface md:sticky md:top-0 md:h-screen md:w-[232px] md:border-r md:border-b-0">
      <div className="flex items-center gap-[11px] px-5 pt-5 pb-[18px]">
        <div className="flex h-[30px] w-[30px] flex-none items-center justify-center rounded-lg bg-text">
          <div className="h-[13px] w-[13px] rounded-full border-[2.5px] border-surface" />
        </div>
        <div className="text-[14.5px] font-bold tracking-tight">tape-archiver</div>
      </div>

      <nav aria-label="Main" className="flex flex-col gap-0.5 px-3 pt-1.5 pb-1">
        <Link to="/history" className={navItemClasses(route.name === 'history')}>
          <IconDashboard className="h-4 w-4 text-text-dim" />
          Dashboard
        </Link>

        {runActive ? (
          <span
            tabIndex={0}
            aria-disabled="true"
            aria-describedby="start-new-run-disabled-reason"
            className="group relative flex cursor-not-allowed items-center gap-2.5 rounded-lg px-2.5 py-2 text-[13px] text-text-faint opacity-70 outline-none"
          >
            <IconLock className="h-4 w-4" />
            Start new run
            <span
              id="start-new-run-disabled-reason"
              role="tooltip"
              className="pointer-events-none absolute top-full left-0 z-10 mt-1 w-56 rounded-md border border-border bg-surface p-2 text-[11px] text-text-dim opacity-0 shadow-card transition-opacity group-hover:opacity-100 group-focus:opacity-100"
            >
              A run is already in progress — finish or abort it before starting another.
            </span>
          </span>
        ) : (
          <Link to="/" className={navItemClasses(route.name === 'submit')}>
            <IconPlus className="h-4 w-4 text-text-dim" />
            Start new run
          </Link>
        )}

        <Link to="/tapes" className={navItemClasses(route.name === 'tapes')}>
          <IconTapes className="h-4 w-4 text-text-dim" />
          Tapes
        </Link>
      </nav>

      <div className="flex-1" />

      <div className="border-t border-border px-4 py-3.5">
        <div
          role="group"
          aria-label="Theme"
          className="flex h-[34px] items-center gap-0.5 rounded-[7px] border border-border bg-inset p-0.5"
        >
          {themeOptions.map(({ value, label, Icon }) => {
            const active = preference === value

            return (
              <button
                key={value}
                type="button"
                aria-pressed={active}
                onClick={() => onPreferenceChange(value)}
                className={
                  active
                    ? 'flex h-full flex-1 items-center justify-center gap-1.5 rounded-md border border-border-strong bg-surface text-[12px] font-medium text-text'
                    : 'flex h-full flex-1 items-center justify-center gap-1.5 rounded-md border border-transparent text-[12px] font-medium text-text-faint transition-colors hover:text-text'
                }
              >
                <Icon className="h-3 w-3" />
                {label}
              </button>
            )
          })}
        </div>

        <div className="mt-3 flex items-center gap-2 px-0.5">
          <div className="flex h-[26px] w-[26px] flex-none items-center justify-center rounded-full border border-border bg-inset text-[11px] text-text-dim">
            {initials(identity)}
          </div>
          <div className="min-w-0 leading-tight">
            <div className="truncate text-[11.5px] font-medium">{identity.name || identity.subject}</div>
            {identity.email && (
              <div className="truncate font-mono text-[11px] text-text-faint">{identity.email}</div>
            )}
          </div>
        </div>

        <Footer className="mt-3" />
      </div>
    </aside>
  )
}

export default Sidebar
