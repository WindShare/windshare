import { expect, test } from '@playwright/test'
import { Uint8ArrayReader, Uint8ArrayWriter, ZipReader } from '@zip.js/zip.js'
import { BROWSER_CONTRACT_HOST_PATH } from '../contract-host'
import { requireOriginPrivateStorage } from '../browser-storage-support'

test('retains a paused native ZIP in Downloads with its partial-save authority without reload', async ({ page, browserName }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)
  const result = await page.evaluate(async () => {
    const path = '/test/browser/downloads/paused-download-retention-harness.ts'
    const harness = await import(path) as typeof import('./paused-download-retention-harness')
    return harness.provePausedDownloadRetention()
  })
  expect(result).toMatchObject({
    hiddenWhileOwned: true, copiedRejected: true, sameOperation: true, sameIntent: true,
    sameGeneration: true, lifecycle: 'resumable-receive', continuation: 'resume-receive',
    completeFileCount: '1',
    display: { objectLabel: 'Retained folder', destinationLabel: 'Browser downloads', createdAtMilliseconds: 1234 },
  })
  expect(result.actions).toContain('save-partial')
  expect(result.actions).toContain('continue')
  const archive = new ZipReader(new Uint8ArrayReader(Uint8Array.from(result.exportedBytes)), {
    checkSignature: true, useWebWorkers: false,
  })
  try {
    const entries = await archive.getEntries()
    const files = entries.filter(entry => !entry.directory)
    expect(files.map(entry => entry.filename)).toEqual(['windshare/micro-share/retained.bin'])
    expect(await files[0]!.getData!(new Uint8ArrayWriter())).toHaveLength(4)
  } finally { await archive.close() }
})
