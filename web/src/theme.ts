import { useCallback, useEffect, useState } from 'react'

// theme.ts implements dark-mode detection/toggle for the app shell
// (App.tsx's header). Tailwind v4 defaults dark: to a media-query-only
// strategy (`prefers-color-scheme`), which already satisfied this issue's
// required acceptance criterion (render in dark/light per the OS
// preference) with zero code — but it cannot support a manual override, the
// AC's documented nice-to-have. index.css switches the dark: variant to a
// class-based selector (`@custom-variant dark (&:where(.dark, .dark *))`)
// so this module can toggle it explicitly; this file is what keeps that
// class in sync with the OS preference by default, and with an explicit
// operator choice once one is made.

export type Theme = 'light' | 'dark'

const STORAGE_KEY = 'tape-archiver:theme'

function isTheme(value: string | null): value is Theme {
  return value === 'light' || value === 'dark'
}

// getStoredTheme returns the operator's explicit override, if they have
// ever used the toggle, or null if the theme has only ever tracked the OS
// preference.
export function getStoredTheme(): Theme | null {
  try {
    const value = window.localStorage.getItem(STORAGE_KEY)

    return isTheme(value) ? value : null
  } catch {
    // Storage can throw in a locked-down/private-browsing context; treat
    // that the same as "no override yet" rather than crashing the app over
    // a purely cosmetic feature.
    return null
  }
}

function setStoredTheme(theme: Theme): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, theme)
  } catch {
    // See getStoredTheme: losing persistence here just means the toggle
    // resets on reload, not worth failing over.
  }
}

function systemPrefersDark(): boolean {
  return window.matchMedia('(prefers-color-scheme: dark)').matches
}

// resolveInitialTheme is the OS-preference-driven acceptance criterion:
// an explicit stored override wins if one exists, otherwise the OS
// preference decides.
export function resolveInitialTheme(): Theme {
  return getStoredTheme() ?? (systemPrefersDark() ? 'dark' : 'light')
}

// applyTheme sets/clears the "dark" class index.css's custom dark: variant
// selects on. Called synchronously in main.tsx before the first render (not
// from inside a React effect) so a dark-mode operator never sees a flash of
// the light theme while React mounts.
export function applyTheme(theme: Theme): void {
  document.documentElement.classList.toggle('dark', theme === 'dark')
}

// useTheme exposes the current theme and a setter that both applies it and
// persists it as an explicit override. While no override has been saved
// yet, it also tracks live OS preference changes, so an operator who never
// touches the toggle keeps following their OS setting even if it changes
// mid-session.
export function useTheme(): [Theme, (theme: Theme) => void] {
  const [theme, setThemeState] = useState<Theme>(() => resolveInitialTheme())

  useEffect(() => {
    applyTheme(theme)
  }, [theme])

  useEffect(() => {
    if (getStoredTheme() !== null) {
      // An explicit override already exists; do not let further OS changes
      // override the operator's own choice.
      return
    }

    const media = window.matchMedia('(prefers-color-scheme: dark)')
    const onChange = (event: MediaQueryListEvent) => setThemeState(event.matches ? 'dark' : 'light')

    media.addEventListener('change', onChange)

    return () => media.removeEventListener('change', onChange)
  }, [])

  const setTheme = useCallback((next: Theme) => {
    setStoredTheme(next)
    setThemeState(next)
  }, [])

  return [theme, setTheme]
}
