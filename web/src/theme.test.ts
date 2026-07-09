import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { applyTheme, getStoredTheme, resolveInitialTheme, useTheme } from './theme'

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

describe('getStoredTheme', () => {
  it('returns null when no override has ever been saved', () => {
    expect(getStoredTheme()).toBeNull()
  })

  it('returns a previously saved override', () => {
    window.localStorage.setItem('tape-archiver:theme', 'dark')
    expect(getStoredTheme()).toBe('dark')
  })

  it('ignores a garbage stored value', () => {
    window.localStorage.setItem('tape-archiver:theme', 'not-a-theme')
    expect(getStoredTheme()).toBeNull()
  })
})

describe('resolveInitialTheme', () => {
  it('follows the OS preference when no override is stored', () => {
    stubMatchMedia(true)
    expect(resolveInitialTheme()).toBe('dark')

    stubMatchMedia(false)
    expect(resolveInitialTheme()).toBe('light')
  })

  it('prefers a stored override over the OS preference', () => {
    stubMatchMedia(true)
    window.localStorage.setItem('tape-archiver:theme', 'light')

    expect(resolveInitialTheme()).toBe('light')
  })
})

describe('useTheme', () => {
  it('keeps following the OS preference across a live change when no override is stored', () => {
    const { fireChange } = stubControllableMatchMedia(false)
    const { result } = renderHook(() => useTheme())

    expect(result.current[0]).toBe('light')

    act(() => fireChange(true))
    expect(result.current[0]).toBe('dark')
  })

  it('does not let a later OS preference change override an explicit operator choice', () => {
    const { fireChange } = stubControllableMatchMedia(false)
    const { result } = renderHook(() => useTheme())

    expect(result.current[0]).toBe('light')

    // The operator explicitly picks dark via the toggle (setTheme), after
    // the OS-change listener already subscribed at mount with no stored
    // override — the exact ordering that regresses if the "an override
    // exists" check is only made once at subscribe time instead of on
    // every OS change event.
    act(() => result.current[1]('dark'))
    expect(result.current[0]).toBe('dark')
    expect(getStoredTheme()).toBe('dark')

    // The OS preference now changes (e.g. a scheduled day/night switch).
    // The operator's explicit choice must survive it.
    act(() => fireChange(false))
    expect(result.current[0]).toBe('dark')
  })
})
