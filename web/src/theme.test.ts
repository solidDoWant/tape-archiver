import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { applyTheme, getStoredTheme, resolveInitialTheme } from './theme'

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
