import { expect, test, type Download } from '@playwright/test'
import {
  Uint8ArrayReader,
  Uint8ArrayWriter,
  ZipReader,
  type FileEntry,
} from '@zip.js/zip.js'

import {
  capabilityUrl,
  DirectProductStack,
} from './fixtures/direct-product-stack'

const SCENARIO_ID = 'chromium-micro-directory'
const DIRECTORY_NAME = 'micro-share'
const FILE_NAME = 'pixel.png'
const FILE_BYTES = Uint8Array.from(Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=',
  'base64',
))
const COMPLETE_STATUS = 'Transfer complete.'
const DOWNLOAD_TIMEOUT_MILLISECONDS = 20_000

test('receives a tiny directory from the real sender and relay', async ({ page }, testInfo) => {
  const stack = new DirectProductStack(SCENARIO_ID)
  const pageErrors: string[] = []
  page.on('pageerror', (error) => pageErrors.push(error.message))
  await stack.start()
  try {
    const directory = await stack.createDirectory(DIRECTORY_NAME, [
      { name: FILE_NAME, bytes: FILE_BYTES },
    ])
    const share = await stack.share(directory)

    // The portable path avoids an operating-system picker while still exercising
    // the production UI, output authority, ZIP writer, and browser download.
    await page.addInitScript(() => {
      Object.defineProperties(window, {
        // The ordinary smoke owns the relay product path; weekly hot-switch owns
        // native peer negotiation, so an external STUN dependency cannot delay CI.
        RTCPeerConnection: { configurable: true, value: undefined },
        showDirectoryPicker: { configurable: true, value: undefined },
        showSaveFilePicker: { configurable: true, value: undefined },
      })
      if (navigator.storage !== undefined) {
        Object.defineProperty(navigator.storage, 'getDirectory', {
          configurable: true,
          value: undefined,
        })
      }
    })
    await page.goto(capabilityUrl(share))

    await expect(page.getByRole('heading', { name: 'Browse and save shared files' })).toBeVisible()
    await expect(page.getByRole('status')).toHaveText('Choose what to receive.')
    await expect(page.getByText(DIRECTORY_NAME, { exact: true })).toBeVisible()
    await page.getByRole('button', { name: 'Open' }).click()
    await expect(page.getByRole('status')).toHaveText('Choose what to receive.')
    await expect(page.getByText(FILE_NAME, { exact: true })).toBeVisible()
    await page.getByRole('button', { name: 'Preview' }).click()
    const preview = page.getByRole('img', { name: `Preview of ${FILE_NAME}` })
    await expect(preview).toBeVisible()
    expect(await preview.evaluate(async (image) => {
      const response = await fetch((image as HTMLImageElement).src)
      return [...new Uint8Array(await response.arrayBuffer())]
    })).toEqual([...FILE_BYTES])
    await page.getByRole('button', { name: 'Close preview' }).click()
    await expect(page.getByRole('radio', { name: /Browser download/u })).toBeChecked()
    await expect.poll(() => new URL(page.url()).hash).toBe('')

    const downloadStarted = page.waitForEvent('download', {
      timeout: DOWNLOAD_TIMEOUT_MILLISECONDS,
    })
    await page.getByRole('button', { name: 'Receive selected' }).click()
    const download = await downloadStarted
    await expect(page.getByRole('status')).toHaveText(COMPLETE_STATUS)
    await expect(page.getByText(/1 file\(s\), .* total/u)).toBeVisible()
    await assertDirectoryDownload(download)
  } catch (error) {
    const pageDiagnostic = await page.evaluate(() => ({
      status: document.querySelector('[role="status"]')?.textContent ?? null,
      error: document.querySelector('[role="alert"]')?.textContent ?? null,
    })).catch(() => ({ status: null, error: null }))
    await testInfo.attach('direct-stack-diagnostic', {
      body: JSON.stringify({
        component: 'browser-direct-smoke',
        scenarioId: SCENARIO_ID,
        milestone: 'failed',
        pageErrors,
        ...pageDiagnostic,
        processes: stack.diagnostic(),
      }, null, 2),
      contentType: 'application/json',
    })
    throw error
  } finally {
    await stack.dispose()
  }
})

async function assertDirectoryDownload(download: Download): Promise<void> {
  expect(download.suggestedFilename()).toBe('windshare.zip')
  const reader = new ZipReader(new Uint8ArrayReader(await readDownload(download)))
  try {
    const entries = await reader.getEntries()
    const file = entries.find(
      (entry): entry is FileEntry =>
        'getData' in entry && entry.filename === `${DIRECTORY_NAME}/${FILE_NAME}`,
    )
    if (file === undefined) {
      throw new Error('Downloaded directory archive is missing the expected file')
    }
    expect(await file.getData(new Uint8ArrayWriter())).toEqual(FILE_BYTES)
  } finally {
    await reader.close()
  }
}

async function readDownload(download: Download): Promise<Uint8Array> {
  const stream = await download.createReadStream()
  if (stream === null) throw new Error('Playwright download stream is unavailable')
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(Buffer.from(chunk))
  return Buffer.concat(chunks)
}
