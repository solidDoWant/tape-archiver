import { test, expect } from '@playwright/test'

// config-form-and-tapes.spec.ts is issue #281's second whole-stack e2e
// spec, covering the two AC4 surfaces submit-monitor-history.spec.ts does
// not: submitting a run via the guided Form-mode config builder (issue
// #279's ConfigForm/ConfigReview, as opposed to that file's JSON-paste
// mode) and the Tapes page (issue #278). Runs against the same real cmd/web
// deployment, dev Temporal, and mhvtl as submit-monitor-history.spec.ts
// (playwright.config.ts's workers: 1 / fullyParallel: false keeps the two
// files from racing the singleton `backup` workflow — SPEC §4.2 — against
// each other).
//
// FORM_ZFS_SOURCE/FORM_BLANK_SLOT let the caller point this at whatever ZFS
// snapshot and mhvtl blank storage slot are available (e2e/web_test.go's
// kind harness sets both, mirroring RUN_CONFIG_PATH's existing pattern; a
// `make web-dev` run can export them by hand — see docs/web-ui.md's
// "Local development" section for the always-available sample dataset).
const zfsSource = process.env.FORM_ZFS_SOURCE
if (!zfsSource) {
  throw new Error('FORM_ZFS_SOURCE must be set to a "dataset@snapshot" name (see e2e/web_test.go)')
}

const blankSlotRaw = process.env.FORM_BLANK_SLOT
if (!blankSlotRaw) {
  throw new Error('FORM_BLANK_SLOT must be set to a blank mhvtl storage slot number (see e2e/web_test.go)')
}

test('submit a dry-run via the Form-mode config builder, then find its tapes on the Tapes page', async ({
  page,
}) => {
  await page.goto('/')
  await page.getByRole('button', { name: /continue with sso/i }).click()
  await expect(page.getByRole('link', { name: 'Dashboard' })).toBeVisible({ timeout: 30_000 })

  await page.getByRole('link', { name: 'Start new run' }).click()

  // Form mode is the config page's default (issue #279 — ConfigPage.tsx's
  // initial mode state), so no toggle click is needed; the Sources card
  // being visible without first clicking "Form" proves it, since "Paste /
  // upload" mode shows a JSON textarea instead.
  await expect(page.getByPlaceholder('bulk-pool-01/dataset')).toBeVisible({ timeout: 15_000 })

  await page.getByPlaceholder('bulk-pool-01/dataset').fill(zfsSource)

  // defaultFormState defaults to 2 copies, which needs (at least) 2 blank
  // slots — this test only has one (FORM_BLANK_SLOT), so drop to 1 copy
  // rather than provisioning a second slot the harness would also need to
  // keep track of.
  await page.locator('#config-copies').fill('1')

  // Library section: the changer/drive devices are deploy-owned and shown
  // read-only from GET /api/config/ui (issue #304 — the deployed cmd/web sets
  // them via the web chart's config.web.library.*, see e2e/web_test.go), so
  // there is nothing to type here; the Dry-run toggle below overrides them
  // server-side with real mhvtl paths regardless (pkg/runsubmit.ApplyDryRun).
  // This test only needs to select a blank slot.
  //
  // Blank slots are picked from the topology-bounded slot grid (issue
  // #305/#310's SlotGridEditor — the free-text "add a slot number" control was
  // removed): the deploy advertises its storage-slot count via GET
  // /api/config/ui (config.web.library.topology.slotCount in e2e/web_test.go),
  // and each real storage slot renders as a "Slot N" toggle button. Clicking
  // blankSlotRaw's toggle adds it to library.blankSlots; exact:true so "Slot 3"
  // never matches "Slot 31", etc.
  await page.getByRole('button', { name: `Slot ${blankSlotRaw}`, exact: true }).click()
  // SlotGridEditor's counter reads "{selectableCount} blank slot(s) selected" —
  // a COUNT of selected slots, not the slot number just clicked (this test only
  // ever selects one, so the count is always "1").
  await expect(page.getByText('1 blank slot(s) selected')).toBeVisible()

  // Encryption: exercise the real server-side age keygen endpoint
  // (POST /api/age/keygen, issue #279) rather than a hardcoded key — this
  // is itself part of the Form-mode surface AC4 asks this test to cover.
  await page.getByRole('button', { name: 'Generate new age keypair' }).click()
  // A successful keygen reveals the identity/recipient pair exactly once, marked
  // by the "NEW KEYPAIR GENERATED" status box (AgeKeygenPanel.tsx). The panel
  // deliberately carries no "store this now or lose it forever" warning
  // (AgeKeygenPanel.tsx's doc comment), so wait on that generated-state marker
  // rather than the old wording.
  await expect(page.getByText(/new keypair generated/i)).toBeVisible({ timeout: 30_000 })

  await page.getByLabel(/^Dry-run/).check()
  await page.getByRole('button', { name: /Review/ }).click()

  // Review step: the assembled config is shown as JSON before submission
  // (issue #279's Review step), including the source and slot just entered.
  // The source name legitimately appears twice (ConfigReview.tsx's summary
  // <dd> AND the raw-JSON <pre id="config-review-json">) — check the raw
  // JSON specifically, since that is what docs/web-ui.md documents as "the
  // final run-config JSON exactly as it will be submitted".
  await expect(page.getByText('STEP 2 · REVIEW')).toBeVisible({ timeout: 15_000 })
  await expect(page.locator('#config-review-json')).toContainText(zfsSource)

  await page.getByRole('button', { name: 'Submit run' }).click()

  // Submitting redirects straight to the new run's detail page (no intermediate
  // confirmation panel) — capture the run ID from the URL it lands on.
  await expect(page).toHaveURL(/\/runs\/[^/?#]+$/, { timeout: 30_000 })
  const runId = new URL(page.url()).pathname.split('/').pop() ?? ''
  expect(runId, 'the redirect URL must carry a non-empty run ID').not.toBe('')

  // Wait for the run to reach a terminal state (the hero heading reads
  // "Backup completed" on success — RunOverview.tsx's heroCopy) before
  // checking the Tapes page: that page reconstructs tapes from each run's
  // own Temporal history on every request (issue #278, GET /api/tapes), so
  // there is nothing to find there before at least the Load phase has run.
  await expect(page.getByRole('heading', { level: 2 })).toHaveText('Backup completed', { timeout: 10 * 60_000 })

  // Tapes page (issue #278): the tape this run just wrote must appear,
  // linked back to this run, with a real measured write-health summary —
  // not the aggregate endpoint's "not measured" fallback, since the run has
  // already closed successfully.
  await page.getByRole('link', { name: 'Tapes' }).click()
  await expect(page).toHaveURL(/\/tapes$/)
  await expect(page.getByText('The archiver keeps no persistent tape catalog')).toBeVisible({ timeout: 30_000 })

  const tapeRow = page.getByRole('row').filter({ has: page.getByRole('link', { name: runId, exact: true }) })
  await expect(tapeRow).toBeVisible({ timeout: 30_000 })
  await expect(tapeRow.getByText('written', { exact: true })).toBeVisible()
  await expect(tapeRow.getByText('not measured')).toHaveCount(0)
})
