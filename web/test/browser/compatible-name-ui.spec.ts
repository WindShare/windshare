import { expect, test, type Page } from '@playwright/test'

type RepairMode = 'receiving' | 'stopped' | 'pending' | 'completed'

const HARNESS_PATH = '/test/browser/compatible-name-ui-harness.tsx'
const FULL_COMMAND = 'powershell.exe -NoProfile -ExecutionPolicy Bypass -File ".\\restore.windshare-abc234.ps1"'

test('restoration copy waits for clipboard success and exposes selectable command on failure', async ({ page }) => {
  await page.goto('/')
  await mount(page, 'completed')
  await expect(page.getByRole('button', { name: 'Copy restoration command' })).toBeVisible()
  await expect(page.getByText(FULL_COMMAND, { exact: true })).not.toBeVisible()
  await page.getByRole('button', { name: 'Copy restoration command' }).click()
  await expect(page.getByRole('button', { name: 'Copying…' })).toBeDisabled()
  await expect(page.getByText('Restoration command copied.')).toHaveCount(0)
  expect(await finishCopy(page, true)).toBe(FULL_COMMAND)
  await expect(page.getByText('Restoration command copied.')).toBeVisible()
  await page.getByRole('button', { name: 'Copy restoration command' }).click()
  await finishCopy(page, false)
  await expect(page.getByText('Restoration command copied.')).toHaveCount(0)
  await expect(page.getByText('Could not copy.', { exact: false })).toBeVisible()
  await expect(page.getByText(FULL_COMMAND, { exact: true })).toBeVisible()
  await page.getByText('Unable to run?', { exact: true }).click()
  await expect(page.getByText('No permanent execution-policy change is needed.', { exact: false })).toBeVisible()
  await expect(page.getByText('.\\restore.windshare-abc234.ps1', { exact: true })).toBeVisible()
})

test('receiving and pending checkpoints never expose commands, while stopped restoration stays secondary', async ({ page }) => {
  await page.goto('/')
  await mount(page, 'receiving')
  await expect(page.getByText('Compatible names are in use')).toBeVisible()
  await expect(page.locator('details')).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Copy restoration command' })).toHaveCount(0)
  await expect(page.getByText(FULL_COMMAND, { exact: true })).toHaveCount(0)

  await mount(page, 'pending')
  await page.getByText('Restore names after stopping', { exact: true }).click()
  await expect(page.getByRole('button', { name: 'Finish local restoration catch-up' })).toBeVisible()
  await page.getByText('Affected names and restoration files', { exact: true }).click()
  await expect(page.getByText(FULL_COMMAND, { exact: true })).toHaveCount(0)
  await expect(page.getByText('Unable to run?', { exact: true })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Copy restoration command' })).toHaveCount(0)

  await mount(page, 'stopped')
  await expect(page.getByRole('button', { name: 'Copy restoration command' })).not.toBeVisible()
  await page.locator('summary').filter({ hasText: /^Restore names after stopping$/u }).click()
  await expect(page.getByRole('button', { name: 'Copy restoration command' })).toBeVisible()
  await expect(page.getByText('After restoring original names, this output cannot resume in the browser.', { exact: false })).toBeVisible()
})

async function mount(page: Page, mode: RepairMode): Promise<void> {
  await page.evaluate(async ({ path, mode }) => {
    const harness = await import(path) as {
      mountRepair(mode: RepairMode): void
      finishCopy(success: boolean): string | undefined
    }
    harness.mountRepair(mode)
  }, { path: HARNESS_PATH, mode })
}

async function finishCopy(page: Page, success: boolean): Promise<string | undefined> {
  return page.evaluate(async ({ path, success }) => {
    const harness = await import(path) as {
      mountRepair(mode: RepairMode): void
      finishCopy(success: boolean): string | undefined
    }
    return harness.finishCopy(success)
  }, { path: HARNESS_PATH, success })
}
