import { expect, test, type Download } from '@playwright/test'
import { openApplication, reloadApplication } from './application/navigation'

const SHARE_PATH = '/AAAAAAAAAAAAAAAA'
const TEST_LINK = SHARE_PATH + '?trace=1#invalid-key'

test('mobile diagnostic link captures startup failure, exports bundle, and stops cleanly across reload', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await openApplication(page, TEST_LINK)
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics.lastFailure()?.stage)).toBe('join')
  expect(await page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(true)
  expect(new URL(page.url()).searchParams.has('trace')).toBe(false)
  expect(new URL(page.url()).hash).toBe('')

  const bar = page.getByRole('complementary', { name: 'Diagnostics' })
  await bar.getByRole('button', { name: 'Export diagnostics' }).click()
  const dialog = page.getByRole('dialog', { name: 'Diagnostics', exact: true })
  await expect(dialog).toBeVisible()
  const current = dialog.getByRole('region', { name: 'Current diagnostics' })
  const pending = page.waitForEvent('download')
  await current.getByRole('button', { name: 'Save file' }).click()
  const download = await pending
  expect(download.suggestedFilename()).toMatch(/^windshare-diagnostics-.+\.ndjson$/u)
  const text = await downloadText(download)
  expect(text).toContain('"transition":"started"')
  expect(text).toContain('"line_type":"failure"')

  await dialog.getByRole('button', { name: 'Stop recording', exact: true }).click()
  expect(await page.evaluate(() => window.windshareDiagnostics.activation().kind)).toBe('off')
  await reloadApplication(page)
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(false)
})

test('pasting a diagnostic link captures the first join and keeps recording until manually stopped', async ({ page }) => {
  await openApplication(page, '/')
  await page.getByRole('textbox').fill(new URL(TEST_LINK, page.url()).href)
  await page.getByRole('textbox').press('Enter')
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics.lastFailure()?.stage)).toBe('join')
  expect(await page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(true)
  const evidence = await page.evaluate(() => window.windshareDiagnostics.export())
  expect(evidence).toContain('"event":"join_transition"')
  expect(evidence).toContain('"transition":"started"')
  const bar = page.getByRole('complementary', { name: 'Diagnostics' })
  await bar.getByRole('button', { name: 'Stop recording', exact: true }).click()
  await bar.getByRole('button', { name: 'Hide notification' }).click()
  await expect(bar).toHaveCount(0)
  await page.getByRole('alert').getByRole('button', { name: 'Export diagnostics' }).click()
  const dialog = page.getByRole('dialog', { name: 'Diagnostics', exact: true })
  await expect(dialog.getByRole('region', { name: 'Current diagnostics' })).toBeVisible()
  expect(await page.evaluate(() => window.windshareDiagnostics.export())).toContain('"event":"join_transition"')
  expect(await page.evaluate(() => window.windshareDiagnostics.activation().kind)).toBe('off')
  await dialog.getByRole('button', { name: 'Start recording' }).click()
  await expect(bar).toBeVisible()
  await dialog.getByRole('button', { name: 'Stop recording', exact: true }).click()
  await reloadApplication(page)
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics.activation().kind)).toBe('off')
  expect(await page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(false)
})

test('page controls stop link activation across reload and support a fresh manual capture', async ({ page }) => {
  await openApplication(page, '/?trace=1')
  const bar = page.getByRole('complementary', { name: 'Diagnostics' })
  await bar.getByRole('button', { name: 'Stop recording' }).click()
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(false)
  await reloadApplication(page)
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(false)
  await page.getByRole('button', { name: 'Diagnostics', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Diagnostics', exact: true })
  await dialog.getByRole('button', { name: 'Start recording' }).click()
  expect(await page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(true)
  await dialog.getByRole('button', { name: 'Stop recording' }).click()
  expect(await page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(false)
})

test('ordinary startup failures remain exportable while trace recording is off', async ({ page }) => {
  await openApplication(page, SHARE_PATH + '#invalid-key')
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics.lastFailure() !== null)).toBe(true)
  await page.getByRole('alert').getByRole('button', { name: 'Export diagnostics' }).click()
  const dialog = page.getByRole('dialog', { name: 'Diagnostics', exact: true })
  const download = page.waitForEvent('download')
  await dialog.getByRole('button', { name: 'Save file' }).click()
  expect(await downloadText(await download)).toContain('"line_type":"failure"')
  expect(await page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(false)
})

test('denied storage and clipboard still allow manual export, and share cancellation does not download', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await page.addInitScript(() => {
    for (const property of ['sessionStorage', 'indexedDB']) {
      Object.defineProperty(window, property, { get() { throw new DOMException('denied', 'SecurityError') } })
    }
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: async () => { throw new Error('denied') } } })
    Object.defineProperty(navigator, 'canShare', { value: (data: ShareData) => data.files?.[0]?.name.endsWith('.txt') })
    Object.defineProperty(navigator, 'share', { value: async (data: ShareData) => {
      if (data.files?.length !== 1 || !data.files[0]!.name.endsWith('.txt')) throw new Error('missing diagnostic attachment')
      throw new DOMException('canceled', 'AbortError')
    } })
  })
  await openApplication(page, TEST_LINK)
  await page.getByRole('complementary', { name: 'Diagnostics' }).getByRole('button', { name: 'Export diagnostics' }).click()
  const dialog = page.getByRole('dialog', { name: 'Diagnostics', exact: true })
  const downloads: Download[] = []
  page.on('download', download => downloads.push(download))
  await dialog.getByRole('button', { name: 'Share file' }).click()
  await expect(dialog.getByRole('button', { name: 'Share file' })).toBeEnabled()
  expect(downloads).toEqual([])
  await dialog.getByRole('button', { name: 'Copy log' }).click()
  const log = dialog.getByRole('textbox', { name: 'Diagnostic log' })
  await expect(log).toBeVisible()
  expect(await log.inputValue()).toContain('"line_type":"bundle_header"')
})

async function downloadText(download: Download): Promise<string> {
  const stream = await download.createReadStream()
  if (stream === null) throw new Error('Diagnostic download has no content')
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(Buffer.from(chunk))
  return Buffer.concat(chunks).toString('utf8')
}
