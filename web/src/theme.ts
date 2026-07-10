import { useCallback, useEffect, useRef, useState } from 'react'

// theme.ts implements the app's Light/Dark/Auto theme control (the design's
// sidebar segmented control — issue #272). Tailwind v4 defaults dark: to a
// media-query-only strategy (`prefers-color-scheme`), which alone would
// satisfy "render in dark/light per the OS preference" with zero code — but
// it cannot support a manual override, or a way to tell "explicitly light"
// apart from "explicitly dark" apart from "following the OS". index.css
// switches the dark: variant to a class-based selector
// (`@custom-variant dark (&:where(.dark, .dark *))`) so this module can
// toggle it explicitly; this file keeps that class in sync with either the
// OS preference (while the operator has never chosen, or has chosen "Auto")
// or an explicit Light/Dark choice.

// ThemePreference is what the operator picked (or the default, "auto").
export type ThemePreference = 'light' | 'dark' | 'auto'

// Theme is the resolved value actually applied to the page — "auto" is
// never a Theme, only a ThemePreference; resolveTheme turns one into the
// other.
export type Theme = 'light' | 'dark'

const STORAGE_KEY = 'tape-archiver:theme'

function isPreference(value: string | null): value is ThemePreference {
  return value === 'light' || value === 'dark' || value === 'auto'
}

// getStoredPreference returns the operator's saved preference, or "auto"
// when none has ever been saved (or storage is unavailable) — "auto" is
// this module's default, not a sentinel meaning "unset".
export function getStoredPreference(): ThemePreference {
  try {
    const value = window.localStorage.getItem(STORAGE_KEY)

    return isPreference(value) ? value : 'auto'
  } catch {
    // Storage can throw in a locked-down/private-browsing context; treat
    // that the same as "auto" rather than crashing the app over a purely
    // cosmetic feature.
    return 'auto'
  }
}

function setStoredPreference(preference: ThemePreference): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, preference)
  } catch {
    // See getStoredPreference: losing persistence here just means the
    // control resets to Auto on reload, not worth failing over.
  }
}

function systemPrefersDark(): boolean {
  return window.matchMedia('(prefers-color-scheme: dark)').matches
}

// resolveTheme turns a preference into the Theme actually applied: "auto"
// follows the live OS preference, "light"/"dark" are already resolved.
export function resolveTheme(preference: ThemePreference): Theme {
  return preference === 'auto' ? (systemPrefersDark() ? 'dark' : 'light') : preference
}

// resolveInitialTheme is called synchronously before the first render (see
// main.tsx) so a dark-mode/Auto operator never sees a flash of the light
// theme while React mounts.
export function resolveInitialTheme(): Theme {
  return resolveTheme(getStoredPreference())
}

// applyTheme sets/clears the "dark" class index.css's custom dark: variant
// selects on, and that the design tokens' `.dark { ... }` block also keys
// off (web/src/design/tokens.css).
export function applyTheme(theme: Theme): void {
  document.documentElement.classList.toggle('dark', theme === 'dark')
}

// useTheme exposes the operator's preference, the resolved theme currently
// applied, and a setter for the preference (used by the sidebar's
// Light/Dark/Auto control). While the preference is "auto", it also tracks
// live OS preference changes, so an operator who never touches the control
// keeps following their OS setting even if it changes mid-session; picking
// "Auto" explicitly (after having picked Light/Dark before) resumes that
// tracking too.
export function useTheme(): [ThemePreference, Theme, (preference: ThemePreference) => void] {
  const [preference, setPreferenceState] = useState<ThemePreference>(() => getStoredPreference())
  const [resolved, setResolved] = useState<Theme>(() => resolveTheme(preference))

  // Mirrors `preference` for the OS-change listener below, which subscribes
  // once (empty deps) for the component's whole lifetime — without this ref,
  // that handler would only ever see the preference value from the render it
  // subscribed in, and an operator picking "Auto" after an earlier explicit
  // choice would not resume following OS changes.
  const preferenceRef = useRef(preference)

  useEffect(() => {
    preferenceRef.current = preference
    setResolved(resolveTheme(preference))
  }, [preference])

  useEffect(() => {
    applyTheme(resolved)
  }, [resolved])

  useEffect(() => {
    const media = window.matchMedia('(prefers-color-scheme: dark)')
    const onChange = (event: MediaQueryListEvent) => {
      if (preferenceRef.current !== 'auto') {
        return
      }

      setResolved(event.matches ? 'dark' : 'light')
    }

    media.addEventListener('change', onChange)

    return () => media.removeEventListener('change', onChange)
  }, [])

  const setPreference = useCallback((next: ThemePreference) => {
    setStoredPreference(next)
    setPreferenceState(next)
  }, [])

  return [preference, resolved, setPreference]
}
