import { expect, test, type Page } from '@playwright/test'
import type { ReceiverPathActivitySnapshot } from '../../../src/receiver/path-activity'
import type { ReceiverConnectionSnapshot, ReceiverReconnectActivity } from '../../../src/receiver/connection-state'
import { capture, expectNoHorizontalOverflow, galleryEvidence, GALLERY_PATH, showScenario } from './assertions'

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

async function updateRecovery(page: Page, reason: 'connecting' | 'connected' | 'backoff' | 'capacity' | 'server', delay = 8_000) {
  await page.evaluate(({ reason, delay }) => {
    const gallery = window as typeof window & { windshareUpdateConnection(snapshot: ReceiverConnectionSnapshot): void }
    const activity: ReceiverReconnectActivity = reason === 'connecting' || reason === 'connected'
      ? { kind: 'connecting' } : { kind: 'waiting', reason, retryAt: performance.now() + delay }
    gallery.windshareUpdateConnection(reason === 'connected' ? { kind: 'connected' } : { kind: 'reconnecting', activity })
  }, { reason, delay })
}

test('recovery controls follow real attempt admission and keep a deadline across unrelated renders', async ({ page }) => {
  await page.goto(GALLERY_PATH)
  await showScenario(page, 'reconnecting')
  await page.clock.install()
  await updateRecovery(page, 'capacity')
  const banner = page.getByRole('region', { name: 'Connection recovery' })
  await expect(banner).toContainText('Taking a short pause after repeated connection attempts.')
  await expect(banner.getByRole('button', { name: 'Please wait' })).toBeDisabled()
  await banner.getByRole('button').evaluate(button => (button as HTMLButtonElement).click())
  expect((await galleryEvidence(page)).intents).not.toContain('reconnect-now')
  await page.clock.runFor(3_000)
  await expect(banner.getByRole('timer')).toHaveText('Retrying automatically in 5 s.')
  await updatePathActivity(page, { lanes: [] })
  await expect(banner.getByRole('timer')).toHaveText('Retrying automatically in 5 s.')
  await expect(banner.getByRole('timer')).toHaveAttribute('aria-live', 'off')
  await page.clock.runFor(5_000)
  await expect(banner.getByRole('timer')).toHaveText('Retrying shortly…')
  await expect(banner.getByRole('button')).toBeDisabled()
  await updateRecovery(page, 'server')
  await expect(banner).toContainText('The connection service asked us to wait before retrying.')
  await expect(banner.getByRole('button')).toBeDisabled()
  await updateRecovery(page, 'backoff')
  await banner.getByRole('button', { name: 'Retry now' }).click()
  expect((await galleryEvidence(page)).intents.filter(intent => intent === 'reconnect-now')).toHaveLength(1)
  await expect(banner.getByRole('button', { name: 'Connecting…' })).toBeDisabled()
  await expect(banner.getByRole('timer')).toHaveCount(0)
  await expect(banner.getByRole('status')).toHaveText('Reconnecting to the sender…')
  await updateRecovery(page, 'connected')
  await expect(banner).toHaveCount(0)
})

test('recovery status and actions fit desktop and mobile in both themes', async ({ page }) => {
  await page.goto(GALLERY_PATH)
  await showScenario(page, 'reconnecting')
  for (const width of [1280, 360]) {
    await page.setViewportSize({ width, height: 900 })
    for (const colorScheme of ['light', 'dark'] as const) {
      await page.emulateMedia({ colorScheme, reducedMotion: 'reduce' })
      await updateRecovery(page, 'capacity', 75_000)
      const banner = page.getByRole('region', { name: 'Connection recovery' })
      await expect(banner.getByRole('button', { name: 'Please wait' })).toBeVisible()
      await expectNoHorizontalOverflow(page)
      expect(await banner.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true)
      await capture(page, `reconnect-waiting-${width}-${colorScheme}`)
      await updateRecovery(page, 'connecting')
      await expect(banner.getByRole('button', { name: 'Connecting…' })).toBeVisible()
      await expectNoHorizontalOverflow(page)
      await capture(page, `reconnect-connecting-${width}-${colorScheme}`)
    }
  }
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
