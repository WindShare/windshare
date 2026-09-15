import { expect, test } from '@playwright/test'
import { capture, galleryEvidence, GALLERY_PATH, JADE, showScenario } from './assertions'

const MINIMUM_TOUCH_TARGET_HEIGHT = 44

test('download records remain reachable across viewport sizes and console tabs', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce', colorScheme: 'light' })
  await page.goto(GALLERY_PATH)
  await showScenario(page, 'portal')
  for (const width of [1440, 900, 390]) {
    await page.setViewportSize({ width, height: 1000 })
    const trigger = page.getByRole('button', { name: /^下载记录/ })
    await expect(trigger).toBeVisible()
    await expect(trigger).toBeEnabled()
    const entry = (await trigger.boundingBox())!
    expect(entry.height).toBeGreaterThanOrEqual(MINIMUM_TOUCH_TARGET_HEIGHT)
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    await capture(page, 'portal-entry-' + width)
  }
  for (const name of ['CLI 命令行分享', '桌面客户端', '极速接收 (Receive)']) {
    await page.getByRole('tab', { name, exact: true }).click()
    await expect(page.getByRole('button', { name: /^下载记录/ })).toBeVisible()
  }
})

test('portal Downloads and nested confirmations stay dark and return focus without changing task ownership', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce', colorScheme: 'light' })
  await page.goto(GALLERY_PATH)
  await showScenario(page, 'portal')
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 1000 })
    const trigger = page.getByRole('button', { name: /^下载记录/ })
    await expect(trigger.locator('.attention-count')).toHaveText('2')
    const before = await galleryEvidence(page)
    await trigger.click()
    const dialog = page.getByRole('dialog', { name: 'Downloads', exact: true })
    await expect(dialog).toHaveCSS('background-color', JADE.dark.paper)
    await expect(dialog).toHaveCSS('color', JADE.dark.ink)
    await expect(dialog.locator('.task-card')).toHaveCount(3)
    await capture(page, 'portal-downloads-' + width + '-os-light')
    await dialog.getByRole('button', { name: 'Details', exact: true }).last().click()
    const details = page.locator('.downloads-sheet')
    await expect(details.getByRole('button', { name: 'All downloads', exact: true })).toBeFocused()
    await capture(page, 'portal-task-details-' + width + '-os-light')
    await details.getByText('Remove task or retained data', { exact: true }).click()
    const remove = details.getByRole('button', { name: 'Delete retained result', exact: true })
    await remove.click()
    const confirmation = page.getByRole('dialog', { name: 'Delete retained result', exact: true })
    await expect(confirmation).toHaveCSS('background-color', JADE.dark.paper)
    await expect(confirmation).toHaveCSS('color', JADE.dark.ink)
    await capture(page, 'portal-confirmation-' + width + '-os-light')
    await page.keyboard.press('Escape')
    await expect(remove).toBeFocused()
    await expect(details).toBeVisible()
    await page.getByRole('button', { name: 'All downloads', exact: true }).click()
    await expect(dialog.getByRole('button', { name: 'Details', exact: true }).last()).toBeFocused()
    for (const colorScheme of ['dark', 'light'] as const) {
      await page.emulateMedia({ colorScheme })
      await expect(dialog).toHaveCSS('background-color', JADE.dark.paper)
      await expect(page.locator('.portal-root')).toHaveCSS('background-color', JADE.dark.page)
    }
    await dialog.getByRole('button', { name: 'Close downloads', exact: true }).click()
    await expect(trigger).toBeFocused()
    expect(await galleryEvidence(page)).toMatchObject({ taskId: before.taskId, taskLabel: before.taskLabel, taskBytes: before.taskBytes })
    expect((await galleryEvidence(page)).intents.slice(before.intents.length))
      .toEqual(['open-downloads', 'open-download-task-details', 'close-download-task-details', 'close-downloads'])
  }
})

test('stable quiet entry distinguishes empty, loading, failed and saved inventories inside Downloads', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce', colorScheme: 'light' })
  await page.goto(GALLERY_PATH)
  for (const scenario of ['portal-empty', 'portal-loading', 'portal-failed', 'portal-saved'] as const) {
    await showScenario(page, scenario)
    const trigger = page.getByRole('button', { name: '下载记录', exact: true })
    await expect(trigger).toBeVisible()
    await expect(trigger.locator('.attention-count')).toHaveCount(0)
    await expect(page.getByRole('dialog')).toHaveCount(0)
    await trigger.click()
    const dialog = page.getByRole('dialog')
    await expect(dialog.getByText('No downloads yet.', { exact: true })).toHaveCount(scenario === 'portal-empty' ? 1 : 0)
    await expect(dialog.getByText('Loading downloads…', { exact: true })).toHaveCount(scenario === 'portal-loading' ? 1 : 0)
    await expect(dialog.getByRole('alert')).toHaveCount(scenario === 'portal-failed' ? 1 : 0)
    if (scenario === 'portal-failed') await expect(dialog).toContainText('Stored receive tasks could not be loaded.')
    if (scenario === 'portal-saved') {
      await expect(dialog.locator('.task-card')).toHaveCount(1)
      await expect(dialog.locator('.task-stage')).toHaveText('Saved')
      await expect(dialog).toContainText('1 filename')
    }
    await capture(page, scenario)
    await page.keyboard.press('Escape')
    await expect(trigger).toBeFocused()
  }
})
