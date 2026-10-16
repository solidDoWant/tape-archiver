import { test, expect } from '@playwright/test'

// presets.spec.ts covers issue #417 end to end: run presets set in the web
// chart (e2e/web_test.go) are offered on the config page, an invalid one is
// not, and loading one fills the form. It never submits, so it cannot race the
// singleton backup workflow the other specs drive.
const validPreset = process.env.VALID_PRESET
const invalidPreset = process.env.INVALID_PRESET
const zfsSource = process.env.FORM_ZFS_SOURCE
if (!validPreset || !invalidPreset || !zfsSource) {
  throw new Error('VALID_PRESET, INVALID_PRESET and FORM_ZFS_SOURCE must be set (see e2e/web_test.go)')
}

test('offers the deployment run presets and loads one into the form', async ({ page }) => {
  await page.goto('/')
  await page.getByRole('button', { name: /continue with sso/i }).click()
  await expect(page.getByRole('link', { name: 'Dashboard' })).toBeVisible({ timeout: 30_000 })

  await page.getByRole('link', { name: 'Start new run' }).click()

  const presets = page.getByRole('region', { name: 'Presets' })
  await expect(presets).toBeVisible({ timeout: 15_000 })
  await expect(presets.getByText(validPreset, { exact: true })).toBeVisible()
  await expect(presets.getByText('E2E preset')).toBeVisible()
  await expect(presets.getByText(invalidPreset, { exact: true })).toHaveCount(0)

  await presets.getByRole('button', { name: `Load preset ${validPreset}` }).click()

  await expect(page.getByPlaceholder('bulk-pool-01/dataset')).toHaveValue(zfsSource)
  await expect(page.locator('#config-copies')).toHaveValue('1')

  // Loaded values stay editable.
  await page.getByPlaceholder('bulk-pool-01/dataset').fill('bulk-pool-01/edited')
  await expect(page.getByPlaceholder('bulk-pool-01/dataset')).toHaveValue('bulk-pool-01/edited')
})
