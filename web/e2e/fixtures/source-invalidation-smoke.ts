import { utimes, writeFile } from 'node:fs/promises'
import { expect, type Download, type Page } from '@playwright/test'
import { capabilityUrl, type DirectProductStack } from './direct-product-stack'

const SOURCE_NAME = 'Makefile'
const ORIGINAL_BYTES = Uint8Array.of(1, 2, 3, 4, 5)
const REPLACEMENT_BYTES = Uint8Array.of(5, 4, 3, 2, 1)
const SOURCE_TIMESTAMP_ADVANCE_MILLISECONDS = 2_000
const DOWNLOAD_TIMEOUT_MILLISECONDS = 20_000

/** Reuse the running product stack so a source-failure regression needs no extra build or relay. */
export async function assertSourceInvalidationRecovery(page: Page, stack: DirectProductStack): Promise<string> {
  const source = await stack.createFile(SOURCE_NAME, ORIGINAL_BYTES)
  const share = await stack.share(source)
  await page.goto(capabilityUrl(share))
  const downloadFile = page.getByRole('button', { name: 'Download file', exact: true })
  await expect(downloadFile).toBeEnabled()
  await page.evaluate(() => window.windshareDiagnostics.enable())

  // Catalog admission must precede mutation: the sender must reject the old
  // revision instead of admitting a fresh catalog entry for the replacement.
  await writeFile(source, REPLACEMENT_BYTES)
  const changedTime = new Date(Date.now() + SOURCE_TIMESTAMP_ADVANCE_MILLISECONDS)
  await utimes(source, changedTime, changedTime)
  await downloadFile.click()
  const task = page.locator('.share-workspace > .task-card')
  await expect(task.getByText('Source file changed', { exact: true })).toBeVisible()
  await expect(task.getByRole('button', { name: 'Continue receiving', exact: true })).toHaveCount(0)
  const invalidatedOperation = await task.getAttribute('data-operation-id')
  expect(invalidatedOperation).not.toBeNull()
  const trace = await page.evaluate(() => {
    window.windshareDiagnostics.disable()
    return window.windshareDiagnostics.export()
  })
  expect(trace).toContain('source_invalidated')
  expect(trace).not.toContain('native_output_failure')

  await page.getByRole('button', { name: 'Start another download', exact: true }).click()
  await expect(downloadFile).toBeEnabled()
  // Starting again releases receiving ownership while retaining explicit cleanup
  // authority for the invalidated task. A new share then supplies valid content.
  const replacement = await stack.share(source)
  await page.goto(capabilityUrl(replacement))
  await expect(downloadFile).toBeEnabled()
  const downloadStarted = page.waitForEvent('download', { timeout: DOWNLOAD_TIMEOUT_MILLISECONDS })
  await downloadFile.click()
  await assertReplacementDownload(await downloadStarted)
  await expect(task).not.toHaveAttribute('data-operation-id', invalidatedOperation!)
  const completedOperation = await task.getAttribute('data-operation-id')
  if (completedOperation === null) throw new Error('The replacement task has no operation identity')

  await page.reload()
  await page.getByRole('button', { name: /^(?:Downloads|下载记录)/u }).click()
  const retained = page.getByRole('dialog').locator(`[data-operation-id="${invalidatedOperation}"]`)
  await expect(retained.getByText('Source file changed', { exact: true })).toBeVisible()
  await expect(retained.getByRole('button', { name: /Continue/u })).toHaveCount(0)
  const completed = page.getByRole('dialog').locator(`[data-operation-id="${completedOperation}"]`)
  const downloadAgain = completed.getByRole('button', { name: 'Download again', exact: true })
  await expect(downloadAgain).toBeEnabled()
  // Reuse this existing reload to protect original-file names at retained handoff.
  await page.context().setOffline(true)
  try {
    const repeated = page.waitForEvent('download', { timeout: DOWNLOAD_TIMEOUT_MILLISECONDS })
    await downloadAgain.click()
    await assertReplacementDownload(await repeated)
  } finally {
    await page.context().setOffline(false)
  }
  return trace
}

async function assertReplacementDownload(download: Download): Promise<void> {
  expect(download.suggestedFilename()).toBe(SOURCE_NAME)
  const stream = await download.createReadStream()
  if (stream === null) throw new Error('Replacement download stream is unavailable')
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(Buffer.from(chunk))
  expect([...Buffer.concat(chunks)]).toEqual([...REPLACEMENT_BYTES])
}
