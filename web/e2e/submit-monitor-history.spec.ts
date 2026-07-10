import { test, expect, type Locator, type Page } from '@playwright/test'

// submit-monitor-history.spec.ts is the whole-stack e2e test for issue #260:
// a real headless browser, driven through the actual UI (never the API
// directly), exercising the full submit (dry-run) -> live monitor -> history
// flow against a real cmd/web deployment, real dev Temporal, and real mhvtl.
// e2e/web_test.go (TestWebUIEndToEnd) is what stands up that whole topology
// (kind cluster, the web Helm chart + image, a fake in-process OIDC
// provider, a blanked mhvtl tape, and the ZFS test snapshot) and sets the
// two environment variables this file depends on before invoking
// `npx playwright test`.

const configPath = process.env.RUN_CONFIG_PATH
if (!configPath) {
  throw new Error(
    'RUN_CONFIG_PATH must be set to a run-config JSON file (see e2e/web_test.go, which sets this before invoking Playwright)',
  )
}

// lastCompletedPhaseCell locates the "Last completed phase" value cell within
// scope (either the whole page's RunDetail <dl>, or one RunHistory row's own
// <dl>) — both render the same "<dt>Last completed phase</dt><dd>...</dd>"
// shape (RunDetail.tsx, RunHistory.tsx). XPath's following-sibling axis is
// used rather than a CSS adjacent-sibling selector so this reads unambiguously
// regardless of exactly which Playwright CSS extensions are in play.
function lastCompletedPhaseCell(scope: Page | Locator): Locator {
  return scope.locator('xpath=//dt[contains(., "Last completed phase")]/following-sibling::dd[1]')
}

test('submit a dry-run, watch it progress live, then see it in history', async ({ page, context }) => {
  // Navigating to "/" while unauthenticated now lands on the SPA's own
  // styled login page (issue #272 — the server serves the SPA rather than
  // 302-ing straight to the IdP). Activating the sign-in control starts
  // the full OIDC authorization-code flow — pkg/webauth's /auth/login
  // redirects to the fake IdP's /authorize, which immediately redirects
  // back to /auth/callback with a code, which is exchanged and lands back
  // on "/" — before the submit form ever renders. Completing that
  // round-trip into the form is this test's proof that the login leg of
  // AC1 actually works, not a separate assertion.
  await page.goto('/')

  await page.getByRole('button', { name: /continue with sso/i }).click()

  const configTextarea = page.getByLabel('Run config (JSON)')
  await expect(configTextarea).toBeVisible({ timeout: 30_000 })

  await page.getByLabel('Upload run config file').setInputFiles(configPath)
  await expect(configTextarea).toHaveValue(/zfsPath/)

  await page.getByLabel(/Dry-run/).check()
  await page.getByRole('button', { name: 'Submit run' }).click()

  const successPanel = page.getByRole('status').filter({ hasText: 'Run submitted.' })
  await expect(successPanel).toBeVisible({ timeout: 30_000 })

  const runId = (await successPanel.locator('code').first().innerText()).trim()
  expect(runId, 'the success panel must show a non-empty run ID').not.toBe('')

  await successPanel.getByRole('button', { name: 'View run' }).click()
  await expect(page).toHaveURL(new RegExp(`/runs/${runId}$`))

  // AC1: live phase updates on the run-detail view, with no manual reload —
  // this whole test never calls page.reload(). The first non-placeholder
  // value proves an SSE update landed at all; requiring a SECOND, different
  // value proves it is genuinely live (more than one push happened), not a
  // single static render that happens to already show a phase.
  const phaseCell = lastCompletedPhaseCell(page)
  await expect(phaseCell).not.toHaveText('—', { timeout: 3 * 60_000 })
  const firstPhase = await phaseCell.innerText()

  await expect
    .poll(async () => phaseCell.innerText(), { timeout: 4 * 60_000, intervals: [1000] })
    .not.toBe(firstPhase)

  // AC2 (mid-run half): the run-history view also surfaces phase-reached
  // information for the still-Running row — RunHistory.tsx enriches exactly
  // one row (the singleton Running run, SPEC §4.2) with a live per-row fetch
  // of the same last-completed-phase data the SSE stream above carries.
  // Opened as a second page in the same (already-authenticated) browser
  // context so the live monitoring page above keeps running unaffected.
  const historyPage = await context.newPage()

  try {
    await historyPage.goto('/history')

    const runningRow = historyPage.locator('li').filter({ hasText: runId }).first()
    await expect(runningRow).toBeVisible({ timeout: 30_000 })
    await expect(runningRow.getByText('Running', { exact: true })).toBeVisible()
    await expect(lastCompletedPhaseCell(runningRow)).not.toHaveText('—', { timeout: 60_000 })
  } finally {
    await historyPage.close()
  }

  // Back on the live view: wait for the run to actually finish.
  await expect(page.getByText('Run finished.')).toBeVisible({ timeout: 10 * 60_000 })

  const statusCell = page.locator('xpath=//dt[contains(., "Status")]/following-sibling::dd[1]').first()
  await expect(statusCell).toHaveText('Completed', { timeout: 30_000 })

  // AC2 (post-completion half): status + timing are listed in the history
  // view once the run has closed. RunHistory.tsx only re-enriches Running
  // rows with a live phase fetch — a deliberate, already-reviewed
  // (sub-issue 7) scope decision, not a bug — so a closed row's own
  // "Last completed phase" cell shows "—" rather than being re-queried; the
  // mid-run assertion above already proved that data is real and genuinely
  // surfaced through this same view while the row was still Running.
  // "Dashboard" is the sidebar's name for the run-history view since the
  // issue #272 shell redesign (the /history route is unchanged).
  await page.getByRole('link', { name: 'Dashboard' }).click()
  await expect(page).toHaveURL(/\/history\/?$/)

  const finishedRow = page.locator('li').filter({ hasText: runId }).first()
  await expect(finishedRow).toBeVisible({ timeout: 30_000 })
  await expect(finishedRow.getByText('Completed', { exact: true })).toBeVisible()

  const startedCell = finishedRow.locator('xpath=.//dt[contains(., "Started")]/following-sibling::dd[1]')
  const closedCell = finishedRow.locator('xpath=.//dt[contains(., "Closed")]/following-sibling::dd[1]')
  await expect(startedCell).not.toHaveText('—')
  await expect(closedCell).not.toHaveText('—')
})
