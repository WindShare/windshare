import { expect, test } from '@playwright/test'
import { join } from 'node:path'
import { galleryEvidence } from './assertions'

test('delivered downloads keep their result and offer one-click repetition on desktop and mobile', async ({ page }) => {
  await page.goto('/test/browser/receiver-gallery/index.html')
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 900 })
    for (const kind of ['published', 'download-started'] as const) {
      await page.getByLabel('Synthetic scenario').selectOption('video')
      await expect(page.locator('[data-gallery-scenario]')).toHaveAttribute('data-gallery-scenario', 'video')
      await page.evaluate(kind => {
        const gallery = window as unknown as { windshareCompleteDownload: (kind: 'published' | 'download-started') => void }
        gallery.windshareCompleteDownload(kind)
      }, kind)
      const result = page.locator('.share-workspace > .task-card')
      await expect(result).toContainText(kind === 'published' ? 'Saved' : 'Download started — check browser downloads')
      await expect(result).toHaveClass(new RegExp(kind === 'published' ? 'task-tone-positive' : 'task-tone-neutral'))
      if (kind === 'published') await expect(result).toContainText('Downloads')
      await expect(result.locator('progress')).toHaveCount(0)
      await expect(page.locator('.saving-controls')).toHaveCount(0)
      await expect(page.getByRole('button', { name: 'Start another download', exact: true })).toHaveCount(0)
      const again = result.getByRole('button', { name: 'Download again', exact: true })
      await expect(again).toBeEnabled()
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      const evidenceDirectory = process.env.WINDSHARE_GALLERY_EVIDENCE_DIR
      if (evidenceDirectory !== undefined) {
        await page.screenshot({ path: join(evidenceDirectory, 'completed-' + kind + '-' + width + '.png'), fullPage: true })
      }
      await again.click()
      expect((await galleryEvidence(page)).intents.some(intent => intent.startsWith('choose:'))).toBe(true)
    }
  }
  await page.getByLabel('Synthetic scenario').selectOption('folder')
  await page.evaluate(() => {
    const gallery = window as unknown as { windshareCompleteDownload: (kind: 'published') => void }
    gallery.windshareCompleteDownload('published')
  })
  await expect(page.getByRole('button', { name: 'Download this folder', exact: true })).toBeEnabled()
  await expect(page.locator('.share-workspace > .task-card')).toContainText('Saved')
  await expect(page.locator('.saving-controls .action-reason')).toHaveCount(0)
})
