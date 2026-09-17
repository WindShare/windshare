import { expect, test } from '@playwright/test'
import { join } from 'node:path'
import { galleryEvidence } from './assertions'
import type { CompletedDownloadScenario } from './fixtures'

test('completed results export retained content and keep a separate new receive action', async ({ page }) => {
  await page.goto('/test/browser/receiver-gallery/index.html')
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 900 })
    for (const kind of ['workspace', 'published', 'download-started'] as const) {
      await page.getByLabel('Synthetic scenario').selectOption('video')
      await expect(page.locator('[data-gallery-scenario]')).toHaveAttribute('data-gallery-scenario', 'video')
      await page.evaluate(kind => {
        const gallery = window as unknown as { windshareCompleteDownload: (kind: CompletedDownloadScenario) => void }
        gallery.windshareCompleteDownload(kind)
      }, kind)
      const result = page.getByRole('region', { name: 'Download: Summer afternoon.mp4', exact: true })
      await expect(result).toContainText(kind === 'published' ? 'Saved' : 'Download started — check browser downloads')
      await expect(result.getByRole('progressbar')).toHaveCount(0)
      await expect(result).toContainText('Elapsed: 2 min 5 sec')
      const before = await galleryEvidence(page)
      const freshReceive = page.getByRole('button', { name: 'Download from sender again', exact: true })
      if (kind === 'workspace') {
        const again = result.getByRole('button', { name: 'Download again', exact: true })
        await expect(again).toBeEnabled()
        await again.click()
        expect((await galleryEvidence(page)).intents.slice(before.intents.length))
          .toEqual([`retained:${before.taskId}:redownload`])
        await expect(freshReceive).toBeHidden()
        await page.locator('summary').filter({ hasText: 'Download from sender again' }).click()
      } else {
        await expect(result.getByRole('button', { name: 'Download again', exact: true })).toHaveCount(0)
      }
      // Routes without a retained artifact still open the next receive in one click.
      await expect(freshReceive).toBeEnabled()
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      const evidenceDirectory = process.env.WINDSHARE_GALLERY_EVIDENCE_DIR
      if (evidenceDirectory !== undefined) {
        await page.screenshot({ path: join(evidenceDirectory, 'completed-' + kind + '-' + width + '.png'), fullPage: true })
      }
      await freshReceive.click()
      expect((await galleryEvidence(page)).intents.at(-1)).toMatch(/^choose:/u)
    }
  }
  await page.getByLabel('Synthetic scenario').selectOption('folder')
  await page.evaluate(() => {
    const gallery = window as unknown as { windshareCompleteDownload: (kind: CompletedDownloadScenario) => void }
    gallery.windshareCompleteDownload('published')
  })
  await expect(page.getByRole('button', { name: 'Download this folder', exact: true })).toBeEnabled()
})
