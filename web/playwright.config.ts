import { defineConfig, devices } from '@playwright/test'

// playwright.config.ts drives web/e2e/, the real end-to-end suite for issue
// #260: a real (headless) browser against a real cmd/web deployment (its
// Helm chart + image, docs/web-ui-design.md §6) behind a `kubectl
// port-forward`, real dev Temporal, real mhvtl, and an in-process fake OIDC
// provider bound to the kind bridge gateway IP. e2e/web_test.go
// (TestWebUIEndToEnd) builds that whole topology and then shells out to
// `npx playwright test` from web/, passing WEB_UI_BASE_URL and
// RUN_CONFIG_PATH as environment variables — this file is never invoked
// directly against a bare `npm run dev` server.
//
// Chromium comes from nixpkgs (pkgs.playwright-driver.browsers, wired into
// flake.nix's devShell via PLAYWRIGHT_BROWSERS_PATH +
// PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1), not Playwright's own
// `npx playwright install` browser-download step: that download produces a
// binary that cannot run in this repo's NixOS-style sandbox (missing system
// libs the dynamic linker can't resolve outside a nixpkgs-patched RPATH) —
// the same constraint sub-issue 7's ad hoc browser verification hit first.
// @playwright/test's pinned version (web/package.json) is kept in lockstep
// with the nixpkgs revision's playwright-driver version so the driver's
// bundled Chromium matches the protocol version this package expects.
export default defineConfig({
  testDir: './e2e',
  // Generous: the test drives a REAL backup workflow through mhvtl (Load,
  // Write, Eject) end to end via the browser, not a mock — comparable
  // real-run budgets elsewhere in this repo's e2e suite (e2e/*_test.go) run
  // up to 20 minutes. The suite's test snapshot is tiny (a few MB), so this
  // ceiling is expected to be hit only on a genuinely stuck run, not typical
  // timing.
  timeout: 18 * 60 * 1000,
  expect: { timeout: 15 * 1000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [['line']],
  use: {
    baseURL: process.env.WEB_UI_BASE_URL,
    trace: 'retain-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        // e2e/web_test.go's harness (and thus this whole test run, via
        // `make test-e2e`) runs under sudo — the same real-tape-device
        // access the rest of the e2e suite needs (CLAUDE.md's Hardware and
        // Safety section) — so Chromium is launched as root. Chromium
        // refuses to start its own setuid sandbox as root; --no-sandbox
        // disables it rather than failing to launch. This is a throwaway,
        // single-purpose browser session against a topology this same test
        // run just stood up and tears down afterward, so the sandbox's
        // process isolation is not protecting against anything here.
        launchOptions: { args: ['--no-sandbox'] },
      },
    },
  ],
})
