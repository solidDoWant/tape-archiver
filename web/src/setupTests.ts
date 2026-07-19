// Extends vitest's `expect` with jest-dom matchers (toBeInTheDocument, etc.)
// for every test file, per web/vite.config.ts's test.setupFiles.
import '@testing-library/jest-dom/vitest'

// jsdom does not implement matchMedia at all, but theme.ts's OS dark-mode
// detection (resolveInitialTheme, useTheme's live-preference listener)
// calls it unconditionally — every test that renders App/uses theme.ts
// needs at least a no-op implementation to exist, even before a specific
// test stubs a specific answer via vi.stubGlobal('matchMedia', ...).
if (!window.matchMedia) {
  window.matchMedia = ((query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia
}
