import { expect, test, type Page } from '@playwright/test'
import { join } from 'node:path'
import type { Scenario } from './receiver-gallery/fixtures'

const GALLERY_PATH = '/test/browser/receiver-gallery/index.html'
const EVIDENCE_DIRECTORY = process.env.WINDSHARE_GALLERY_EVIDENCE_DIR

test.beforeEach(({ page }) => { page.on('pageerror', error => { console.error(error.stack) }) })

async function scenario(page: Page, value: Scenario) {
  await page.getByLabel('Synthetic scenario').selectOption(value)
  await expect(page.locator('[data-gallery-scenario]')).toHaveAttribute('data-gallery-scenario', value)
  await expect(page.locator('.receiver-shell')).toBeVisible()
}

async function evidence(page: Page) {
  return page.evaluate(async () => {
    const path = '/test/browser/receiver-gallery/harness.tsx'
    const gallery = await import(path) as { galleryEvidence(): { intents: string[]; taskId: string | null; taskLabel: string | null; taskBytes: string; draftEmpty: boolean } }
    return gallery.galleryEvidence()
  })
}

async function screenshot(page: Page, name: string) {
  if (EVIDENCE_DIRECTORY !== undefined) {
    // This host contains only generated media and synthetic operation facts.
    expect(new URL(page.url()).pathname).toBe(GALLERY_PATH)
    await page.screenshot({ path: join(EVIDENCE_DIRECTORY, name + '.png'), fullPage: true })
  }
}

test('production explorer keeps semantic selection, preview focus and current task independent', async ({ page }) => {
  await page.goto(GALLERY_PATH)
  const task = page.locator('.share-workspace > .task-card')
  await expect(task).toContainText('Downloading')
  const before = await evidence(page)
  await page.getByRole('button', { name: 'Select items', exact: true }).click()
  const checkbox = page.getByRole('checkbox', { name: 'Select Summer photos', exact: true })
  await expect(checkbox).toHaveAttribute('aria-checked', 'mixed')
  await checkbox.focus()
  await page.keyboard.press('Space')
  await expect(checkbox).toBeChecked()
  expect((await evidence(page)).intents).toEqual(['toggle:photos'])
  await page.getByRole('button', { name: 'Clear selection', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Download selected', exact: true })).toBeDisabled()
  await page.getByRole('button', { name: 'Done selecting', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Download this folder', exact: true })).toBeDisabled()
  await expect(page.getByText('Pause the current download before starting another.', { exact: true })).toBeVisible()

  const folder = page.getByRole('button', { name: 'Summer photos', exact: true }).last()
  await folder.focus()
  await page.keyboard.press('Enter')
  await expect(page.getByRole('button', { name: 'Portrait.png', exact: true })).toBeVisible()
  await expect(task).toContainText('Downloading')
  const previewTrigger = page.getByRole('button', { name: 'Portrait.png', exact: true })
  await previewTrigger.click()
  await expect(page.getByRole('dialog')).toBeVisible()
  await expect(page.getByRole('img', { name: 'Preview of Portrait.png' })).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.getByRole('dialog')).toHaveCount(0)
  await expect(previewTrigger).toBeFocused()

  const details = task.getByRole('button', { name: 'Details', exact: true })
  await details.click()
  await expect(page.getByRole('dialog')).toContainText('Receiving the selected content')
  await page.keyboard.press('Tab')
  await expect(page.getByRole('dialog')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(details).toBeFocused()

  const downloads = page.getByRole('button', { name: /^Downloads/ })
  await downloads.click()
  await expect(page.getByRole('dialog').locator('.task-card')).toHaveCount(3)
  await expect(page.getByRole('dialog')).toContainText('Archive')
  await page.getByRole('dialog').getByRole('button', { name: 'Details', exact: true }).last().click()
  await expect(page.getByRole('dialog')).toContainText('Created')
  await page.getByRole('button', { name: 'All downloads', exact: true }).click()
  await page.keyboard.press('Escape')
  await expect(downloads).toBeFocused()
  expect(await evidence(page)).toMatchObject({ taskId: before.taskId, taskLabel: before.taskLabel, taskBytes: before.taskBytes })
})

test('production responsive gallery keeps media uncropped and controls reachable with touch', async ({ browser }) => {
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true })
  const page = await context.newPage()
  await page.goto(GALLERY_PATH)
  await page.getByRole('button', { name: 'Next', exact: true }).tap()
  await expect(page.getByRole('navigation', { name: 'Directory pages' })).toContainText('Page 2 of 2')
  await page.getByRole('button', { name: /^Downloads/ }).tap()
  await expect(page.getByRole('dialog')).toBeVisible()
  await screenshot(page, 'mobile-downloads')
  await page.getByRole('button', { name: 'Back to share', exact: true }).tap()
  await expect(page.locator('.share-workspace > .task-card')).toContainText('Downloading')

  for (const [device, width, height] of [['desktop', 1440, 1000], ['tablet', 820, 1180], ['mobile', 390, 844]] as const) {
    await page.setViewportSize({ width, height })
    for (const value of ['folder', 'portrait', 'landscape', 'video', 'unsupported'] as const) {
      await scenario(page, value)
      if (value === 'video') {
        await page.getByRole('button', { name: 'Preview a frame', exact: true }).click()
        await expect(page.locator('video')).toBeVisible()
        await expect.poll(() => page.locator('video').evaluate(video => (video as HTMLVideoElement).readyState)).toBeGreaterThanOrEqual(2)
        await expect(page.getByRole('slider', { name: 'Seek Summer afternoon.mp4' })).toBeVisible()
      }
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      if (value === 'portrait' || value === 'landscape') {
        const photo = page.getByRole('img', { name: /^Preview of/ })
        await expect(photo).toBeVisible()
        expect(await photo.evaluate(image => getComputedStyle(image).objectFit)).toBe('contain')
        const box = await photo.boundingBox()
        expect(box!.width).toBeLessThanOrEqual(width)
      }
      await screenshot(page, device + '-' + value)
    }
  }
  await scenario(page, 'unsupported')
  await page.getByRole('button', { name: 'Preview', exact: true }).tap()
  await expect(page.getByRole('alert')).toContainText('cannot be previewed')
  await expect(page.getByRole('button', { name: 'Download file', exact: true })).toBeEnabled()
  await context.close()
})

test('a full catalog page keeps download controls close while every item remains reachable', async ({ page }) => {
  await page.goto(GALLERY_PATH)
  for (const [device, width, height] of [['desktop', 1440, 1000], ['mobile', 390, 844]] as const) {
    await page.setViewportSize({ width, height })
    await scenario(page, 'full-directory')
    const contents = page.getByRole('list', { name: 'Folder contents' })
    await expect(contents.locator('.explorer-row')).toHaveCount(256)
    const download = page.getByRole('button', { name: 'Download this folder', exact: true })
    const pause = page.locator('.share-workspace > .task-card').getByRole('button', { name: 'Pause', exact: true })
    const downloadBefore = await download.boundingBox()
    const pauseBefore = await pause.boundingBox()
    expect(downloadBefore!.y + downloadBefore!.height).toBeLessThan(height)
    expect(pauseBefore!.y + pauseBefore!.height).toBeLessThan(height * 1.25)
    await contents.focus()
    await page.keyboard.press('Control+End')
    const lastItem = page.getByRole('button', { name: 'Document 256.txt', exact: true })
    await expect(lastItem).toBeInViewport()
    expect(await download.boundingBox()).toEqual(downloadBefore)
    expect(await pause.boundingBox()).toEqual(pauseBefore)
    await lastItem.click()
    await expect(page.getByRole('dialog')).toBeVisible()
    await page.keyboard.press('Escape')
    await expect(lastItem).toBeFocused()
    await screenshot(page, device + '-full-directory')
  }
})

test('production task stages retain honest publication and partial-result wording', async ({ page }) => {
  await page.goto(GALLERY_PATH)
  for (const [value, headline] of [
    ['reconnecting', 'Reconnecting to the sender'],
    ['capacity', 'Waiting for sender capacity'],
    ['verifying', 'Verifying ZIP'],
    ['partial-ready', 'Ready to save'],
    ['browser-handoff', 'Download started — check browser downloads'],
    ['saved-cleanup', 'Saved'],
  ] as const) {
    await scenario(page, value)
    await expect(page.locator('.share-workspace > .task-card .task-stage')).toHaveText(headline)
    if (value === 'partial-ready') await expect(page.locator('.share-workspace > .task-card')).toContainText('Partial result')
    if (value === 'saved-cleanup') await expect(page.locator('.share-workspace > .task-card')).toContainText('1 filename')
    await screenshot(page, 'stage-' + value)
  }
})
