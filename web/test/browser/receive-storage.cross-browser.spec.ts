import { expect, test, type Download } from '@playwright/test'
import { Uint8ArrayReader, Uint8ArrayWriter, ZipReader, type FileEntry } from '@zip.js/zip.js'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'
import { requireOriginPrivateStorage } from './browser-storage-support'

const HARNESS_PATH = '/test/browser/receive-storage-harness.ts'

async function readDownloadBytes(download: Download): Promise<Uint8Array> {
  const stream = await download.createReadStream()
  if (stream === null) throw new Error('Playwright download stream is unavailable')
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(Buffer.from(chunk))
  return new Uint8Array(Buffer.concat(chunks))
}

async function readZip(bytes: Uint8Array): Promise<Record<string, string>> {
  const reader = new ZipReader(new Uint8ArrayReader(bytes), {
    checkSignature: true,
    useWebWorkers: false,
  })
  try {
    const result: Record<string, string> = {}
    for (const entry of await reader.getEntries()) {
      if (!entry.directory && 'getData' in entry) {
        const data = await (entry as FileEntry).getData(new Uint8ArrayWriter())
        result[entry.filename] = new TextDecoder().decode(data)
      }
    }
    return result
  } finally {
    await reader.close()
  }
}

test('unified ReceiveStore persists tasks and files and cascades deletion cleanly', async ({ page }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const result = await page.evaluate(async ({ path, db }) => {
    const harness = await import(path) as typeof import('./receive-storage-harness')
    return harness.verifyUnifiedStore(db)
  }, { path: HARNESS_PATH, db: `windshare-store-test-${crypto.randomUUID()}` })

  expect(result).toEqual({
    tasksBefore: 1,
    filesBefore: 1,
    tasksAfter: 0,
    filesAfter: 0,
  })
})

test('native Web Locks prevent concurrent cross-tab mutation of the same task', async ({ page, context }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const taskId = `lock-test-${crypto.randomUUID()}`
  await page.evaluate(async ({ path, id }) => {
    const harness = await import(path) as typeof import('./receive-storage-harness')
    await harness.holdCrossTabLock(id)
  }, { path: HARNESS_PATH, id: taskId })

  const otherTab = await context.newPage()
  try {
    await otherTab.goto(BROWSER_CONTRACT_HOST_PATH)
    const contested = await otherTab.evaluate(async ({ path, id }) => {
      const harness = await import(path) as typeof import('./receive-storage-harness')
      return harness.checkCrossTabLock(id)
    }, { path: HARNESS_PATH, id: taskId })
    expect(contested).toEqual({ lockedElsewhere: true, acquireError: 'InvalidStateError' })

    await page.evaluate(async path => {
      const harness = await import(path) as typeof import('./receive-storage-harness')
      await harness.releaseCrossTabLock()
    }, HARNESS_PATH)

    const released = await otherTab.evaluate(async ({ path, id }) => {
      const harness = await import(path) as typeof import('./receive-storage-harness')
      return harness.checkCrossTabLock(id)
    }, { path: HARNESS_PATH, id: taskId })
    expect(released).toEqual({ lockedElsewhere: false, acquireError: null })
  } finally {
    await otherTab.close()
  }
})

test('OPFS SyncAccessHandle worker supports progressive ZIP resume, partial export, offline Save Again, and delete cleanup', async ({
  page,
  context,
  browserName,
}) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)

  const db = `windshare-opfs-test-${crypto.randomUUID()}`
  const taskId = `opfs-${crypto.randomUUID()}`
  try {
    await page.evaluate(async ({ path, databaseName, id }) => {
      const harness = await import(path) as typeof import('./receive-storage-harness')
      await harness.preparePausedOPFSZip(databaseName, id)
    }, { path: HARNESS_PATH, databaseName: db, id: taskId })

    const partialDownloadPromise = page.waitForEvent('download')
    await page.evaluate(async ({ path, databaseName, id }) => {
      const harness = await import(path) as typeof import('./receive-storage-harness')
      await harness.exportPausedOPFSZip(databaseName, id)
    }, { path: HARNESS_PATH, databaseName: db, id: taskId })
    const partialDownload = await partialDownloadPromise
    expect(partialDownload.suggestedFilename()).toBe('bundle-partial.zip')
    expect(await readZip(await readDownloadBytes(partialDownload))).toEqual({
      'folder/a.txt': 'first-file-payload',
    })

    const completedDownloadPromise = page.waitForEvent('download')
    const resumed = await page.evaluate(async ({ path, databaseName, id }) => {
      const harness = await import(path) as typeof import('./receive-storage-harness')
      return harness.resumeAndCompleteOPFSZip(databaseName, id)
    }, { path: HARNESS_PATH, databaseName: db, id: taskId })
    const completedDownload = await completedDownloadPromise

    expect(resumed).toEqual({
      reopenedF1AlreadyCompleted: true,
      reopenedF2StartEnd: ['0:6'],
      finishedKind: 'download-started',
    })
    expect(completedDownload.suggestedFilename()).toBe('bundle.zip')
    expect(await readZip(await readDownloadBytes(completedDownload))).toEqual({
      'folder/a.txt': 'first-file-payload',
      'folder/b.txt': 'second-file-payload!',
    })

    // Save again must hand off the retained OPFS File directly without network access.
    await context.setOffline(true)
    try {
      const savedAgainDownloadPromise = page.waitForEvent('download')
      const savedAgainKind = await page.evaluate(async ({ path, databaseName, id }) => {
        const harness = await import(path) as typeof import('./receive-storage-harness')
        return harness.saveRetainedOPFSZip(databaseName, id)
      }, { path: HARNESS_PATH, databaseName: db, id: taskId })
      const savedAgainDownload = await savedAgainDownloadPromise

      expect(savedAgainKind).toBe('download-started')
      expect(savedAgainDownload.suggestedFilename()).toBe('bundle.zip')
      expect(await readZip(await readDownloadBytes(savedAgainDownload))).toEqual({
        'folder/a.txt': 'first-file-payload',
        'folder/b.txt': 'second-file-payload!',
      })
    } finally {
      await context.setOffline(false)
    }

    const deleted = await page.evaluate(async ({ path, databaseName, id }) => {
      const harness = await import(path) as typeof import('./receive-storage-harness')
      return harness.deleteOPFSZipTask(databaseName, id)
    }, { path: HARNESS_PATH, databaseName: db, id: taskId })
    expect(deleted).toEqual({ taskRemoved: true, objectRemoved: true })
  } finally {
    await page.evaluate(async ({ path, databaseName, id }) => {
      const harness = await import(path) as typeof import('./receive-storage-harness')
      await harness.cleanupOPFSZipTest(databaseName, id)
    }, { path: HARNESS_PATH, databaseName: db, id: taskId }).catch(() => undefined)
  }
})

test('portable backend streams valid Store-mode ZIP archives to browser download', async ({ page }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const downloadPromise = page.waitForEvent('download')
  const lifecycleKind = await page.evaluate(async path => {
    const harness = await import(path) as typeof import('./receive-storage-harness')
    return harness.verifyPortableStreamingZip()
  }, HARNESS_PATH)
  const download = await downloadPromise

  expect(lifecycleKind).toBe('download-started')
  expect(download.suggestedFilename()).toBe('quick.zip')
  expect(await readZip(await readDownloadBytes(download))).toEqual({ 'note.txt': 'hello' })
})
