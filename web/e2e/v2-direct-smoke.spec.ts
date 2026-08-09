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
import {
  createCapabilityRedactor,
  withCapabilityRedaction,
} from './fixtures/capability-redactor'

const DIRECTORY_NAME = 'micro-share'
const FILE_NAME = 'pixel.png'
const FILE_BYTES = Uint8Array.from(Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=',
  'base64',
))
const ARCHIVE_NAME = 'windshare.zip'
const SYNTHETIC_RESULT_ROOT_NAME = 'windshare'
const DOWNLOAD_TIMEOUT_MILLISECONDS = 20_000

test('receives an explicit directory artifact from the real sender and relay', async ({ browserName, page }, testInfo) => {
  const scenarioId = microDirectoryScenarioId(browserName)
  const stack = new DirectProductStack(scenarioId)
  const pageErrors: string[] = []
  let redactor: ReturnType<typeof createCapabilityRedactor> | undefined
  page.on('pageerror', (error) => pageErrors.push(error.message))
  page.on('console', (message) => {
    const value = message.text()
    if (message.type() === 'info' && value.startsWith('windshare.receive')) console.info(value)
  })
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
    const navigationUrl = capabilityUrl(share)
    redactor = createCapabilityRedactor({
      completeUrl: navigationUrl,
      fragment: new URL(navigationUrl).hash,
      separateKey: share.key,
    })
    await withCapabilityRedaction(() => page.goto(navigationUrl), {
      completeUrl: navigationUrl,
      fragment: new URL(navigationUrl).hash,
      separateKey: share.key,
    })

    const browseStatus = page.locator('.status-line')
    await expect(page.getByRole('heading', { name: 'Browse and save shared files' })).toBeVisible()
    await expect(browseStatus).toHaveText('Choose what to receive.')
    await expect(page.getByText(DIRECTORY_NAME, { exact: true })).toBeVisible()
    await page.getByRole('button', { name: 'Open' }).click()
    await expect(browseStatus).toHaveText('Choose what to receive.')
    await expect(page.getByText(FILE_NAME, { exact: true })).toBeVisible()
    await page.getByRole('button', { name: 'Preview' }).click()
    const preview = page.getByRole('img', { name: `Preview of ${FILE_NAME}` })
    await expect(preview).toBeVisible()
    expect(await preview.evaluate(async (image) => {
      const response = await fetch((image as HTMLImageElement).src)
      return [...new Uint8Array(await response.arrayBuffer())]
    })).toEqual([...FILE_BYTES])
    await page.getByRole('button', { name: 'Close preview' }).click()
    const artifactAction = page.getByRole('button', { name: 'Check then download' })
    await expect(artifactAction).toBeVisible()
    await expect(page.getByText(
      'Checks that the complete ZIP fits before receiving any file content. The browser takes over when the package is ready.',
      { exact: true },
    )).toBeVisible()
    await expect.poll(() => new URL(page.url()).hash).toBe('')

    const downloadStarted = page.waitForEvent('download', {
      timeout: DOWNLOAD_TIMEOUT_MILLISECONDS,
    })
    await artifactAction.click()
    const download = await downloadStarted
    await expect(page.getByText('Download started', { exact: true })).toBeVisible({
      timeout: DOWNLOAD_TIMEOUT_MILLISECONDS,
    })
    await expect(page.getByText(
      'The browser took over the download. WindShare cannot confirm where or whether it was saved.',
      { exact: true },
    )).toBeVisible()
    await expect(page.getByText('Ready to save', { exact: true })).toHaveCount(0)
    await expect(page.getByText('Saved', { exact: true })).toHaveCount(0)
    await expect(page.getByText(/1 file\(s\), .* total/u)).toBeVisible()
    await assertDirectoryDownload(download)
  } catch (error) {
    const pageDiagnostic = await page.evaluate(() => ({
      status: document.querySelector('[role="status"]')?.textContent ?? null,
      error: document.querySelector('[role="alert"]')?.textContent ?? null,
    })).catch(() => ({ status: null, error: null }))
    await testInfo.attach('direct-stack-diagnostic', {
      body: redactor?.text({
        component: 'browser-direct-smoke',
        scenarioId,
        milestone: 'failed',
        pageErrors,
        ...pageDiagnostic,
        processes: stack.diagnostic(),
      }) ?? JSON.stringify({
        component: 'browser-direct-smoke',
        scenarioId,
        milestone: 'failed',
        pageErrors,
        ...pageDiagnostic,
        processes: stack.diagnostic(),
      }),
      contentType: 'application/json',
    }).catch(() => undefined)
    const message = error instanceof Error ? error.message : String(error)
    // Attach only the recursively redacted snapshot; Playwright must never
    // retain the original capability-bearing assertion as an Error.cause.
    throw new Error(redactor?.redactText(message) ?? message, {
      // eslint-disable-next-line preserve-caught-error -- safe cause is the only permitted boundary value
      cause: redactor?.value(error),
    })
  } finally {
    try {
      await stack.dispose()
    } finally {
      redactor?.clear()
    }
  }
})

function microDirectoryScenarioId(browserName: string): string {
  if (browserName !== 'chromium' && browserName !== 'firefox' && browserName !== 'webkit') {
    throw new TypeError(`Unsupported browser engine for direct smoke: ${browserName}`)
  }
  return `${browserName}-micro-directory`
}

async function assertDirectoryDownload(download: Download): Promise<void> {
  expect(download.suggestedFilename()).toBe(ARCHIVE_NAME)
  const reader = new ZipReader(new Uint8ArrayReader(await readDownload(download)))
  try {
    const entries = await reader.getEntries()
    const file = entries.find(
      (entry): entry is FileEntry =>
        'getData' in entry && entry.filename ===
          `${SYNTHETIC_RESULT_ROOT_NAME}/${DIRECTORY_NAME}/${FILE_NAME}`,
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
