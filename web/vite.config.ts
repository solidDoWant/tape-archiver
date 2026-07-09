import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const dirname = path.dirname(fileURLToPath(import.meta.url))

// `build.outDir` points at cmd/web/dist rather than the default web/dist: a
// Go `go:embed` directive can only reach the embedding file's own directory
// subtree, so cmd/web (which embeds this output — see cmd/web/assets.go) needs
// it inside cmd/web/. emptyOutDir is explicit (true) since the target is
// outside Vite's project root, where Vite would otherwise refuse to empty it
// for safety.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: path.resolve(dirname, '../cmd/web/dist'),
    emptyOutDir: true,
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/setupTests.ts'],
    globals: true,
    // Scoped to src/: vitest's own default include glob
    // (**/*.{test,spec}.*) would otherwise also pick up web/e2e/*.spec.ts,
    // the real-browser Playwright suite (issue #260) — those import
    // @playwright/test's own test()/expect (not vitest's) and need a live
    // web UI + baseURL, so running them under vitest would just fail. They
    // are run via `npm run test:e2e` (playwright.config.ts), not `npm test`.
    include: ['src/**/*.{test,spec}.{ts,tsx}'],
  },
})
