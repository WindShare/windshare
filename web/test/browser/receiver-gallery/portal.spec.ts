import { expect, test } from '@playwright/test'
import { writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { capture, galleryEvidence, GALLERY_PATH, JADE, portalStyles, showScenario } from './assertions'

test('download records sit below the console while the original homepage navigation keeps its balance', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce', colorScheme: 'light' })
  await page.goto(GALLERY_PATH)
  await showScenario(page, 'portal')
  const facts = []
  for (const width of [1440, 900, 841, 840, 390]) {
    await page.setViewportSize({ width, height: 1000 })
    await page.evaluate(() => scrollTo(0, 0))
    const header = page.locator('.portal-header')
    const trigger = page.getByRole('button', { name: /^下载记录/ })
    await expect(header.locator('.downloads-entry')).toHaveCount(0)
    await expect(header.locator('.portal-container > *')).toHaveCount(3)
    await expect(header.locator('.portal-nav a')).toHaveText(['核心优势', '工作原理', 'CLI & 客户端', '自建中转'])
    expect(await header.locator('.portal-nav a').evaluateAll(links => links.map(link => link.getAttribute('href'))))
      .toEqual(['#features', '#how-it-works', '#cli', '#self-host'])
    const brand = (await header.locator('.portal-brand').boundingBox())!
    const repository = (await header.locator('.portal-btn-github').boundingBox())!
    if (width > 840) {
      await expect(header.locator('.portal-nav')).toBeVisible()
      const nav = (await header.locator('.portal-nav').boundingBox())!
      const leftGap = nav.x - brand.x - brand.width
      const rightGap = repository.x - nav.x - nav.width
      expect(leftGap).toBeGreaterThan(0)
      expect(Math.abs(leftGap - rightGap)).toBeLessThan(1)
    } else await expect(header.locator('.portal-nav')).toBeHidden()
    await expect(trigger).toHaveAccessibleDescription('当前浏览器的下载任务与记录')
    await expect(trigger).toHaveCSS('background-color', 'rgba(0, 0, 0, 0)')
    const card = (await page.locator('.portal-console-card').boundingBox())!
    const entry = (await trigger.boundingBox())!
    expect(entry.height).toBeGreaterThanOrEqual(44)
    expect(entry.y).toBeGreaterThanOrEqual(card.y + card.height)
    expect(entry.y - card.y - card.height).toBeLessThanOrEqual(32)
    expect(Math.abs(entry.x + entry.width / 2 - card.x - card.width / 2)).toBeLessThan(1)
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    facts.push({ width, styles: await portalStyles(page) })
    await capture(page, 'portal-entry-' + width)
  }
  for (const name of ['CLI 命令行分享', '桌面客户端', '极速接收 (Receive)']) {
    await page.getByRole('tab', { name, exact: true }).click()
    await expect(page.getByRole('button', { name: /^下载记录/ })).toBeVisible()
    await expect(page.locator('.portal-console-body .downloads-entry')).toHaveCount(0)
  }
  const directory = process.env.WINDSHARE_GALLERY_EVIDENCE_DIR
  if (directory !== undefined) await writeFile(join(directory, 'portal-final.json'), JSON.stringify(facts, null, 2))
})

test('portal Downloads and nested confirmations stay dark and return focus without changing task ownership', async ({ page }) => {
  expect(await page.evaluate(() => CSS.supports('color', 'light-dark(white, black)'))).toBe(true)
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
    await expect(dialog).toContainText('Tasks and saved records in this browser.')
    await expect(dialog.locator('.task-card')).toHaveCount(3)
    const rect = (await dialog.boundingBox())!
    if (width === 1440) {
      expect(rect.width).toBeGreaterThanOrEqual(520)
      expect(rect.width).toBeLessThanOrEqual(580)
      expect(rect.x + rect.width).toBeCloseTo(width, 0)
    } else {
      expect(rect.width).toBe(width)
      expect(rect.height).toBe(1000)
    }
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
    if (scenario === 'portal-empty') await expect(dialog).toContainText('Downloads you start here will appear in this browser.')
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
