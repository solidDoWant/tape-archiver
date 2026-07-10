import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { applyTheme, getStoredPreference, resolveInitialTheme, resolveTheme, useTheme } from './theme'

// stubMatchMedia replaces window.matchMedia with one reporting matches for
// good, standing in for a specific OS color-scheme preference.
function stubMatchMedia(matches: boolean) {
  vi.stubGlobal('matchMedia', (query: string) => ({
    matches,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  }))
}

// stubControllableMatchMedia is stubMatchMedia's more capable sibling: it
// returns a `fireChange` function so a test can simulate the OS preference
// actually flipping mid-session (matchMedia's real "change" event), not
// just its value at mount time.
function stubControllableMatchMedia(initialMatches: boolean) {
  let matches = initialMatches
  let changeListener: ((event: MediaQueryListEvent) => void) | null = null

  vi.stubGlobal('matchMedia', (query: string) => ({
    get matches() {
      return matches
    },
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: (_event: string, listener: (event: MediaQueryListEvent) => void) => {
      changeListener = listener
    },
    removeEventListener: () => {
      changeListener = null
    },
    dispatchEvent: () => false,
  }))

  return {
    fireChange(nextMatches: boolean) {
      matches = nextMatches
      changeListener?.({ matches: nextMatches } as MediaQueryListEvent)
    },
  }
}

beforeEach(() => {
  document.documentElement.classList.remove('dark')
  window.localStorage.clear()
})

afterEach(() => {
  vi.unstubAllGlobals()
  document.documentElement.classList.remove('dark')
  window.localStorage.clear()
})

describe('applyTheme', () => {
  it('adds the "dark" class for dark and removes it for light', () => {
    applyTheme('dark')
    expect(document.documentElement.classList.contains('dark')).toBe(true)

    applyTheme('light')
    expect(document.documentElement.classList.contains('dark')).toBe(false)
  })
})

describe('getStoredPreference', () => {
  it('defaults to "auto" when no preference has ever been saved', () => {
    expect(getStoredPreference()).toBe('auto')
  })

  it('returns a previously saved preference', () => {
    window.localStorage.setItem('tape-archiver:theme', 'dark')
    expect(getStoredPreference()).toBe('dark')
  })

  it('falls back to "auto" for a garbage stored value', () => {
    window.localStorage.setItem('tape-archiver:theme', 'not-a-theme')
    expect(getStoredPreference()).toBe('auto')
  })
})

describe('resolveTheme', () => {
  it('follows the OS preference for "auto"', () => {
    stubMatchMedia(true)
    expect(resolveTheme('auto')).toBe('dark')

    stubMatchMedia(false)
    expect(resolveTheme('auto')).toBe('light')
  })

  it('passes an explicit light/dark preference through unchanged, ignoring the OS', () => {
    stubMatchMedia(true)
    expect(resolveTheme('light')).toBe('light')

    stubMatchMedia(false)
    expect(resolveTheme('dark')).toBe('dark')
  })
})

describe('resolveInitialTheme', () => {
  it('follows the OS preference when the stored preference is "auto" (the default)', () => {
    stubMatchMedia(true)
    expect(resolveInitialTheme()).toBe('dark')

    stubMatchMedia(false)
    expect(resolveInitialTheme()).toBe('light')
  })

  it('prefers a stored explicit preference over the OS preference', () => {
    stubMatchMedia(true)
    window.localStorage.setItem('tape-archiver:theme', 'light')

    expect(resolveInitialTheme()).toBe('light')
  })
})

describe('useTheme', () => {
  it('defaults to "auto" and keeps following the OS preference across a live change', () => {
    const { fireChange } = stubControllableMatchMedia(false)
    const { result } = renderHook(() => useTheme())

    expect(result.current[0]).toBe('auto')
    expect(result.current[1]).toBe('light')

    act(() => fireChange(true))
    expect(result.current[1]).toBe('dark')
  })

  it('does not let a later OS preference change override an explicit operator choice', () => {
    const { fireChange } = stubControllableMatchMedia(false)
    const { result } = renderHook(() => useTheme())

    expect(result.current[1]).toBe('light')

    // The operator explicitly picks Dark via the control, after the
    // OS-change listener already subscribed at mount with preference
    // "auto" — the exact ordering that regresses if the "is the preference
    // auto" check is only made once at subscribe time instead of on every
    // OS change event.
    act(() => result.current[2]('dark'))
    expect(result.current[0]).toBe('dark')
    expect(result.current[1]).toBe('dark')
    expect(getStoredPreference()).toBe('dark')

    // The OS preference now changes (e.g. a scheduled day/night switch).
    // The operator's explicit choice must survive it.
    act(() => fireChange(false))
    expect(result.current[1]).toBe('dark')
  })

  it('resumes following the OS preference after switching back to "Auto"', () => {
    const { fireChange } = stubControllableMatchMedia(false)
    const { result } = renderHook(() => useTheme())

    act(() => result.current[2]('dark'))
    expect(result.current[1]).toBe('dark')

    act(() => result.current[2]('auto'))
    expect(result.current[0]).toBe('auto')
    expect(result.current[1]).toBe('light')

    act(() => fireChange(true))
    expect(result.current[1]).toBe('dark')
  })
})
