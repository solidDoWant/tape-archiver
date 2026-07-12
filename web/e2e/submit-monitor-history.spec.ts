import { test, expect, type Locator, type Page } from '@playwright/test'

// submit-monitor-history.spec.ts is the whole-stack e2e test for issue #260,
// repaired and extended for the redesigned UI by issue #281: a real headless
// browser, driven through the actual UI (never the API directly), exercising
// the full sign-in -> submit (dry-run, JSON mode) -> live monitor (phase
// rail + log/metric panels) -> dashboard-history flow against a real cmd/web
// deployment, real dev Temporal, and real mhvtl. e2e/web_test.go
// (TestWebUIEndToEnd) is what stands up that whole topology (kind cluster,
// the web Helm chart + image, a fake in-process OIDC provider, a blanked
// mhvtl tape, and the ZFS test snapshot) and sets the two environment
// variables this file depends on before invoking `npx playwright test`; it
// can equally be run directly against a `make web-dev` stack by setting
// WEB_UI_BASE_URL/RUN_CONFIG_PATH by hand (issue #281's live-verification
// pass did exactly that).
//
// This file previously referenced markup from before the run-detail
// phase-rail redesign (issue #277) — a "Status"/"Last completed phase"
// `<dt>/<dd>` pair and a "Run finished." string that no longer exist
// anywhere in the redesigned RunOverview.tsx (that page shows a status hero
// heading like "Backup completed" and a "Last completed phase: X"
// paragraph instead). Since the suite had never actually been run against a
// live stack until issue #281, that mismatch was only caught here, running
// it for the first time — fixed below to match RunOverview.tsx's real
// markup, plus phase-rail/log-panel coverage AC4 also calls for.

const configPath = process.env.RUN_CONFIG_PATH
if (!configPath) {
  throw new Error(
    'RUN_CONFIG_PATH must be set to a run-config JSON file (see e2e/web_test.go, which sets this before invoking Playwright)',
  )
}

// lastCompletedPhaseText locates RunOverview.tsx's "Last completed phase: X"
// paragraph (rendered as a single text node, not a dt/dd pair).
function lastCompletedPhaseText(scope: Page | Locator): Locator {
  return scope.getByText(/Last completed phase:/)
}

// statusHeading locates RunOverview.tsx's hero <h2> ("Backup in progress",
// "Backup completed", ...) — the redesigned page's equivalent of the old
// "Status" field.
function statusHeading(scope: Page | Locator): Locator {
  return scope.getByRole('heading', { level: 2 })
}

test('submit a dry-run (JSON mode), watch it progress live with the phase rail, then see it in history', async ({
  page,
  context,
}) => {
  // Navigating to "/" while unauthenticated lands on the SPA's own styled
  // login page (issue #272 — the server serves the SPA rather than 302-ing
  // straight to the IdP). Activating the sign-in control starts the full
  // OIDC authorization-code flow — pkg/webauth's /auth/login redirects to
  // the fake IdP's /authorize, which immediately redirects back to
  // /auth/callback with a code, which is exchanged and lands back on "/" —
  // the dashboard, since issue #276. Completing that round-trip, then
  // reaching the config page (issue #279 — "Start new run", /submit) and
  // its JSON mode ("Paste / upload" — the page opens in Form mode), is this
  // test's proof that the login leg of AC1 actually works, not a separate
  // assertion.
  await page.goto('/')

  await page.getByRole('button', { name: /continue with sso/i }).click()

  // Dashboard: current-run card (idle at this point) + runs table shell —
  // both present as soon as the shell mounts post-login (AC4's "dashboard
  // (current-run card + runs table)").
  await expect(page.getByRole('link', { name: 'Dashboard' })).toBeVisible({ timeout: 30_000 })
  await expect(page.getByText('CURRENT RUN')).toBeVisible()
  await expect(page.getByText('Runs', { exact: true })).toBeVisible()

  await page.getByRole('link', { name: 'Start new run' }).click({ timeout: 30_000 })
  await page.getByRole('button', { name: 'Paste / upload' }).click()

  const configTextarea = page.getByLabel('Run config (JSON)')
  await expect(configTextarea).toBeVisible({ timeout: 30_000 })

  await page.getByLabel('Upload run config file').setInputFiles(configPath)
  await expect(configTextarea).toHaveValue(/zfsPath/)

  await page.getByLabel(/Dry-run/).check()
  await page.getByRole('button', { name: 'Submit run' }).click()

  // Submitting redirects straight to the new run's detail page (no intermediate
  // confirmation panel) — capture the run ID from the URL it lands on.
  await expect(page).toHaveURL(/\/runs\/[^/?#]+$/, { timeout: 30_000 })
  const runId = new URL(page.url()).pathname.split('/').pop() ?? ''
  expect(runId, 'the redirect URL must carry a non-empty run ID').not.toBe('')

  // Phase rail: all 11 pipeline phases listed, in order, "Run overview"
  // selected by default (issue #277).
  const rail = page.getByRole('navigation', { name: 'Run phases' })
  await expect(rail).toBeVisible({ timeout: 30_000 })
  for (const label of [
    'Resolve',
    'Prepare',
    'Pack',
    'PAR2',
    'Verify',
    'Load',
    'Write',
    'Eject',
    'Report',
    'Burn',
    'Deliver',
  ]) {
    await expect(rail.getByRole('button', { name: new RegExp(`^${label}`) })).toBeVisible()
  }

  // AC1: live phase updates on the run-detail view, with no manual reload —
  // this whole test never calls page.reload(). The first non-placeholder
  // value proves an SSE update landed at all; requiring a SECOND, different
  // value proves it is genuinely live (more than one push happened), not a
  // single static render that happens to already show a phase.
  const phaseText = lastCompletedPhaseText(page)
  await expect(phaseText).not.toHaveText('Last completed phase: —', { timeout: 3 * 60_000 })
  const firstPhase = await phaseText.innerText()

  await expect
    .poll(async () => phaseText.innerText(), { timeout: 4 * 60_000, intervals: [1000] })
    .not.toBe(firstPhase)

  // Phase rail navigation (AC4's "run detail page including its log and
  // metric panels"): selecting the Write phase shows its own facts/log
  // panel, and (uniquely among phases) live drive write-rate metrics,
  // without leaving the page. This may run before, during, or after the
  // Write phase actually executes — the view must render either way (a
  // "not started yet" placeholder before, live data during/after).
  await rail.getByRole('button', { name: /^Write/ }).click()
  await expect(page.getByRole('heading', { name: 'Write', exact: true })).toBeVisible({ timeout: 15_000 })

  // Wait until the log panel reaches its "ready" state (a role="log"
  // region) rather than staying stuck loading or reporting VictoriaLogs
  // unavailable — this dev stack always has VictoriaLogs configured
  // (docs/web-ui.md's "Local development" section), so real matched lines
  // (or an honest "no lines yet" empty state) must appear, never a stuck
  // spinner.
  await expect(page.getByRole('log')).toBeVisible({ timeout: 3 * 60_000 })

  // The drive-metrics panel (DriveMetricsPanel — the Write phase's other
  // AC4 panel) must be present as its own labeled region alongside the log
  // panel, and must settle into one of its meaningful states rather than a
  // stuck loading spinner: a real MB/s reading (live from VictoriaMetrics,
  // or a terminal run's final recorded write health), an honest "no
  // measurement" note (write health is measured only after a tape's write
  // window closes — DriveGauge.tsx), an empty "no tapes" note, or the
  // panel's styled unavailable state. Which one depends on where the run
  // has progressed by the time this executes — all are correct; a
  // perpetual "Loading" is the only wrong answer.
  const metricsPanel = page.getByRole('region', { name: 'Drive write health' })
  await expect(metricsPanel).toBeVisible()
  await expect(
    metricsPanel.getByText(
      /MB\/s|No measurement yet|No measurement was taken|No tapes were written|Metrics unavailable|Write-health unavailable/,
    ),
  ).toBeVisible({ timeout: 60_000 })

  // Back to the overview for the remaining assertions.
  await rail.getByRole('button', { name: 'Run overview' }).click()
  await expect(statusHeading(page)).toBeVisible()

  // AC2 (mid-run half): the run-history table (embedded in the dashboard
  // since issue #276 — /history redirects there) also surfaces phase-reached
  // information for the still-Running row, fed by the dashboard's own live
  // SSE subscription to the singleton Running run (SPEC §4.2); the
  // dashboard's current-run card also reflects the same active run (AC4's
  // dashboard coverage). Opened as a second page in the same
  // (already-authenticated) browser context so the live monitoring page
  // above keeps running unaffected.
  const historyPage = await context.newPage()

  try {
    await historyPage.goto('/')

    // The dashboard must reflect this run. Which state it reflects is a
    // race this test cannot control: a dry-run against mhvtl can finish in
    // well under a minute, so by the time this second page loads, the run
    // is either still Running (current-run card shows its active state with
    // an "Open run ->" link — distinct from the idle state's "Start a
    // run ->") or already Completed (the card is back to idle and the run
    // sits in the embedded history table). Both are correct dashboard
    // behavior for AC2/AC4; assert the run's row either way, and only
    // assert the active-state card when the run is genuinely still open.
    const runRow = historyPage.getByRole('link', { name: runId, exact: true })
    await expect(runRow).toBeVisible({ timeout: 30_000 })

    const stillRunning = await runRow
      .getByText('Running', { exact: true })
      .first()
      .isVisible()
      .catch(() => false)
    if (stillRunning) {
      await expect(historyPage.getByRole('link', { name: 'Open run →' })).toBeVisible({ timeout: 30_000 })
      // The row's LAST PHASE cell (grid column 5) must show a real phase.
      await expect(runRow.locator(':scope > span').nth(4)).not.toHaveText('—', { timeout: 60_000 })
    } else {
      // Already terminal: the row must carry a terminal status badge and a
      // real final phase, proving the dashboard tracked it to completion.
      await expect(runRow.getByText(/Completed|Failed/).first()).toBeVisible()
      await expect(runRow.locator(':scope > span').nth(4)).not.toHaveText('—')
    }
  } finally {
    await historyPage.close()
  }

  // Back on the live view: wait for the run to actually finish — the hero
  // heading (RunOverview.tsx's heroCopy) reads "Backup completed" once the
  // workflow closes as Completed, and the terminal READ-ONLY banner appears
  // (RunDetail.tsx's RunDetailLive).
  await expect(statusHeading(page)).toHaveText('Backup completed', { timeout: 10 * 60_000 })
  await expect(page.getByText('READ-ONLY')).toBeVisible()

  // AC2 (post-completion half): status + timing are listed in run history
  // once the run has closed. Run history lives in the dashboard's embedded
  // runs table since issue #276 (RunsTable.tsx — each row is one link whose
  // accessible name is the run ID); a closed row's "last phase" cell shows
  // "—" by design (only a live workflow query can answer it), and the
  // mid-run assertion above already proved that data is real and genuinely
  // surfaced while the run was still Running.
  await page.getByRole('link', { name: 'Dashboard' }).click()
  await expect(page).toHaveURL(/\/$/)

  const finishedRow = page.getByRole('link', { name: runId, exact: true })
  await expect(finishedRow).toBeVisible({ timeout: 30_000 })
  await expect(finishedRow.getByText('Completed', { exact: true })).toBeVisible()

  // The STARTED and DURATION cells (grid columns 2 and 3) must both be
  // real values, not the em-dash placeholder.
  const cells = finishedRow.locator(':scope > span')
  await expect(cells.nth(1)).not.toHaveText('—')
  await expect(cells.nth(2)).not.toHaveText('—')
})
