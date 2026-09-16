import { expect, test, type Download, type Page } from '@playwright/test'

const SHARE_PATH = '/AAAAAAAAAAAAAAAA'
const TEST_LINK = SHARE_PATH + '?trace=1#invalid-key'

test('mobile diagnostic link captures startup failure and exports retained evidence after reload', async ({ page, context }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto(TEST_LINK)
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics.status().state)).toBe('sealed')
  expect(new URL(page.url()).searchParams.has('trace')).toBe(false)
  expect(new URL(page.url()).hash).toBe('')
  await expect.poll(() => savedEvidence(page)).toContain('"event":"join_transition"')

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
  expect(text).toContain('"line_type":"incident"')
  const originalRun = JSON.parse(text.split('\n')[0]!).runtime_run_id as string

  // Stop capture activation before reload; recovery must export the old run, not a new empty bundle.
  await dialog.getByRole('button', { name: 'Stop recording', exact: true }).click()
  expect(await page.evaluate(() => window.windshareDiagnostics.activation().kind)).toBe('off')
  await page.reload()
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(false)
  await bar.getByRole('button', { name: 'Export diagnostics' }).click()
  const previous = dialog.getByRole('region', { name: 'Previous diagnostics' })
  await expect(previous).toBeVisible()
  await expect(current).toHaveCount(0)
  await context.setOffline(true)
  const oldDownload = page.waitForEvent('download')
  // The only offered file must contain the retained failure, not this empty run.
  await dialog.getByRole('button', { name: 'Save file' }).click()
  const restoredText = await downloadText(await oldDownload)
  expect(restoredText).toContain('"event":"join_transition"')
  expect(JSON.parse(restoredText.split('\n')[0]!).runtime_run_id).toBe(originalRun)
})

test('pasting a diagnostic link captures the first join and can stop a sealed capture', async ({ page }) => {
  await page.goto('/')
  await page.getByRole('textbox').fill(new URL(TEST_LINK, page.url()).href)
  await page.getByRole('textbox').press('Enter')
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics.status().state)).toBe('sealed')
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
  await page.reload()
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics.activation().kind)).toBe('off')
  expect(await page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(false)
})

test('page controls stop link activation across reload and support a fresh manual capture', async ({ page }) => {
  await page.goto('/?trace=1')
  const bar = page.getByRole('complementary', { name: 'Diagnostics' })
  await bar.getByRole('button', { name: 'Stop recording' }).click()
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(false)
  await page.reload()
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(false)
  await page.getByRole('button', { name: 'Diagnostics', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Diagnostics', exact: true })
  await dialog.getByRole('button', { name: 'Start recording' }).click()
  expect(await page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(true)
  await dialog.getByRole('button', { name: 'Stop recording' }).click()
  expect(await page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(false)
})

test('ordinary startup failures remain exportable while trace recording is off', async ({ page }) => {
  await page.goto(SHARE_PATH + '#invalid-key')
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics.inspectLastFailure() !== null)).toBe(true)
  await page.getByRole('alert').getByRole('button', { name: 'Export diagnostics' }).click()
  const dialog = page.getByRole('dialog', { name: 'Diagnostics', exact: true })
  const download = page.waitForEvent('download')
  await dialog.getByRole('button', { name: 'Save file' }).click()
  expect(await downloadText(await download)).toContain('"line_type":"incident"')
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
  await page.goto(TEST_LINK)
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

async function savedEvidence(page: Page): Promise<string> {
  return page.evaluate(async () => {
    const modulePath = '/src/diagnostics/browser/indexeddb-archive.ts'
    const { IndexedDBDiagnosticsArchive } = await import(/* @vite-ignore */ modulePath) as typeof import('../../src/diagnostics/browser/indexeddb-archive')
    const archive = new IndexedDBDiagnosticsArchive(() => indexedDB)
    const captures = await archive.list()
    const files = await Promise.all(captures.map(capture => archive.readFile(capture.id)))
    return files.map(file => file?.text ?? '').join('\n')
  })
}

async function downloadText(download: Download): Promise<string> {
  const stream = await download.createReadStream()
  if (stream === null) throw new Error('Diagnostic download has no content')
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(Buffer.from(chunk))
  return Buffer.concat(chunks).toString('utf8')
}
