import { expect, type Download, type Page } from '@playwright/test'

export const RETAINED_DIRECTORY_CAPABILITY_KEY = 'windshare-smoke-retained-directory'

const DOWNLOAD_TIMEOUT_MILLISECONDS = 20_000

/** A fresh page must reopen its retained artifact through the shipped UI without a sender. */
export async function assertRetainedDirectoryDownload(
  page: Page,
  navigationUrl: string,
  assertDownload: (download: Download) => Promise<void>,
): Promise<void> {
  await page.evaluate(key => window.sessionStorage.setItem(key, 'enabled'), RETAINED_DIRECTORY_CAPABILITY_KEY)
  // Restoring only the capability fragment would reuse the portable document.
  await page.goto('about:blank')
  await page.goto(navigationUrl)
  const action = page.getByRole('button', { name: 'Download this folder', exact: true })
  await expect(action, 'The fresh receiver document offers the folder download').toBeEnabled()
  const firstDownload = page.waitForEvent('download', { timeout: DOWNLOAD_TIMEOUT_MILLISECONDS })
  await action.click()
  await assertDownload(await firstDownload)
  const activeTask = page.locator('.share-workspace > .task-card')
  await expect(activeTask.getByText('Download started \u2014 check browser downloads', { exact: true }))
    .toBeVisible()
  const operationId = await activeTask.getAttribute('data-operation-id')
  if (operationId === null) throw new Error('The completed directory task has no operation identity')

  // Reload removes the live transfer and its in-memory artifact. The consumed
  // capability cannot reconnect; the retained Downloads action owns local recovery.
  await expect.poll(() => new URL(page.url()).hash).toBe('')
  await page.reload()
  await page.getByRole('button', { name: /^(?:Downloads|下载记录)/u }).click()
  const downloads = page.getByRole('dialog').filter({
    has: page.getByRole('heading', { name: 'Downloads', exact: true }),
  })
  const retainedTask = downloads.locator(`[data-operation-id="${operationId}"]`)
  await expect(retainedTask).toBeVisible()
  const retry = retainedTask.getByRole('button', { name: 'Download again', exact: true })
  await expect(retry).toBeEnabled()

  await page.context().setOffline(true)
  try {
    const secondDownload = page.waitForEvent('download', { timeout: DOWNLOAD_TIMEOUT_MILLISECONDS })
    await retry.click()
    await assertDownload(await secondDownload)
    // Browser handoff precedes durable settlement. Navigation remains blocked
    // until the retained action releases ownership and reloads its task.
    await expect(retry, 'The retained download settles before reopening the share').toBeEnabled()
  } finally {
    await page.context().setOffline(false)
  }

  // Reopening the original capability must reconnect this same portal document.
  await page.keyboard.press('Escape')
  await expect(downloads).toBeHidden()
  const timeOrigin = await page.evaluate(() => performance.timeOrigin)
  expect(await page.goto(navigationUrl)).toBeNull()
  await expect(action, 'Reopening the capability reconnects the existing portal document').toBeEnabled()
  await expect.poll(() => new URL(page.url()).hash).toBe('')
  expect(await page.evaluate(() => performance.timeOrigin)).toBe(timeOrigin)
  await page.getByRole('button', { name: /^(?:Downloads|下载记录)/u }).click()
  await expect(downloads.locator(`[data-operation-id="${operationId}"]`)).toBeVisible()
  await page.keyboard.press('Escape')
}
