import { expect, test, type Page } from '@playwright/test'
import type { ReceiverPathActivitySnapshot } from '../../../src/receiver/path-activity'
import { capture, expectNoHorizontalOverflow, GALLERY_PATH, showScenario } from './assertions'

async function updatePathActivity(page: Page, snapshot: ReceiverPathActivitySnapshot) {
  await page.evaluate(path => {
    const gallery = window as typeof window & {
      windshareUpdatePathActivity(snapshot: ReceiverPathActivitySnapshot): void
    }
    gallery.windshareUpdatePathActivity(path)
  }, snapshot)
}

test('connection details show individual routes and follow channel activity and disconnection', async ({ page }) => {
  await page.goto(GALLERY_PATH)
  const connection = page.getByRole('button', { name: /Sender connected/ })
  await expect(connection).toContainText('3 channels connected')
  await connection.click()
  const dialog = page.getByRole('dialog', { name: 'Encryption and connection' })
  const channels = dialog.getByRole('list', { name: 'Connected download channels' })
  await expect(channels.getByRole('listitem')).toHaveCount(3)
  await expect(channels.getByRole('listitem').filter({ hasText: 'Channel 1' })).toContainText('Application relay')
  await expect(channels.getByRole('listitem').filter({ hasText: 'Channel 3' })).toContainText('Direct (P2P)')
  await expect(channels.getByRole('listitem').filter({ hasText: 'Channel 3' })).toContainText('Received recently')
  await expect(channels.getByRole('listitem').filter({ hasText: 'Channel 5' })).toContainText('TURN relay')
  await expect(channels.getByRole('listitem').filter({ hasText: 'Channel 5' })).toContainText('No recent data')

  await updatePathActivity(page, { lanes: [{ laneId: 5, laneEpoch: 1, route: 'turn', recentContent: true }] })
  await expect(channels.getByRole('listitem')).toHaveCount(1)
  await expect(channels).toContainText('Received recently')
  await expect(dialog.getByRole('status')).toHaveText('1 channel connected')
  await expect(connection).toContainText('1 channel connected')
  await updatePathActivity(page, { lanes: [{ laneId: 5, laneEpoch: 1, route: 'turn', recentContent: false }] })
  await expect(channels).toContainText('No recent data')
  await updatePathActivity(page, { lanes: [] })
  await expect(dialog.getByRole('status')).toHaveText('0 channels connected')
  await expect(dialog.getByRole('list')).toHaveCount(0)
  await expect(connection).not.toContainText('channels connected')
  await expect(dialog).toContainText('Channels appear when available for downloads or previews.')
  await page.keyboard.press('Escape')
  await expect(connection).toBeFocused()
})

test('channel details stay readable on desktop and narrow screens in both themes', async ({ page }) => {
  await page.goto(GALLERY_PATH)
  for (const width of [1280, 360]) {
    await page.setViewportSize({ width, height: 900 })
    for (const colorScheme of ['light', 'dark'] as const) {
      await page.emulateMedia({ colorScheme, reducedMotion: 'reduce' })
      await showScenario(page, 'folder')
      await page.getByRole('button', { name: /Sender connected/ }).click()
      const dialog = page.getByRole('dialog', { name: 'Encryption and connection' })
      await expect(dialog.getByRole('heading', { name: 'Download channels' })).toBeVisible()
      await expect(dialog.getByRole('listitem')).toHaveCount(3)
      await expectNoHorizontalOverflow(page)
      expect(await dialog.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true)
      await capture(page, 'download-channels-' + width + '-' + colorScheme)
      await page.keyboard.press('Escape')
    }
  }
})
