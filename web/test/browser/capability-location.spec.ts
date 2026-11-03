import { expect, test } from '@playwright/test'
import { openApplication, reloadApplication } from './application/navigation'

const EMPTY_SHARE_PATH = '/s/AAAAAAAAAAAAAAAA'
const INVALID_SHARE_LINK = `${EMPTY_SHARE_PATH}#invalid-key`

test('same-document share navigation reaches the receiver and consumes its fragment', async ({ page }) => {
  await openApplication(page, EMPTY_SHARE_PATH)
  await expect(page.locator('.portal-root')).toBeVisible()
  const documentTimeOrigin = await page.evaluate(() => {
    window.windshareDiagnostics.enable()
    return performance.timeOrigin
  })

  // Invalid credentials exercise the shipped intake and join boundary without a network dependency.
  const response = await openApplication(page, INVALID_SHARE_LINK)
  expect(response).toBeNull()
  await expect(page.locator('.share-workspace')).toBeVisible()
  await expect.poll(() => new URL(page.url()).hash).toBe('')
  expect(await page.evaluate(() => performance.timeOrigin)).toBe(documentTimeOrigin)
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics.export()))
    .toContain('"event":"join_transition"')

  await reloadApplication(page)
  await expect(page.locator('.portal-root')).toBeVisible()
  expect(await openApplication(page, INVALID_SHARE_LINK)).toBeNull()
  await expect(page.locator('.share-workspace')).toBeVisible()
  await expect.poll(() => new URL(page.url()).hash).toBe('')
})

test('portal anchors remain navigation anchors before and after reload', async ({ page }) => {
  await openApplication(page, '/')
  await expect(page.locator('.portal-root')).toBeVisible()
  await page.evaluate(() => window.windshareDiagnostics.enable())
  for (const fragment of ['features', 'how-it-works', 'cli', 'self-host']) {
    await page.locator(`.portal-nav a[href="#${fragment}"]`).click()
    await expect.poll(() => new URL(page.url()).hash).toBe(`#${fragment}`)
    await expect(page.locator('.portal-root')).toBeVisible()
  }
  await reloadApplication(page)
  await expect(page.locator('.portal-root')).toBeVisible()
  expect(new URL(page.url()).hash).toBe('#self-host')
  expect(await page.evaluate(() => window.windshareDiagnostics.export())).not.toContain('"event":"join_transition"')
})

test('a persisted page retains its location listener', async ({ page }) => {
  await openApplication(page, EMPTY_SHARE_PATH)
  await expect(page.locator('.portal-root')).toBeVisible()
  await page.evaluate(() => {
    window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true }))
    window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true }))
  })
  expect(await openApplication(page, INVALID_SHARE_LINK)).toBeNull()
  await expect(page.locator('.share-workspace')).toBeVisible()
  await expect.poll(() => new URL(page.url()).hash).toBe('')
})
