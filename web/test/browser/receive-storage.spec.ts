import { expect, test } from '@playwright/test'
import { Uint8ArrayReader, Uint8ArrayWriter, ZipReader, type FileEntry } from '@zip.js/zip.js'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'
import { requireOriginPrivateStorage } from './browser-storage-support'

const HARNESS_PATH = '/test/browser/receive-storage-harness.ts'

async function readZip(bytes: readonly number[]): Promise<Record<string, string>> {
  const reader = new ZipReader(new Uint8ArrayReader(Uint8Array.from(bytes)), {
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

test('unified ReceiveStore persists tasks, files, and zip-spool and cascades deletion cleanly', async ({ page }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const result = await page.evaluate(async ({ path, db }) => {
    const harness = await import(path) as typeof import('./receive-storage-harness')
    return harness.verifyUnifiedStore(db)
  }, { path: HARNESS_PATH, db: `windshare-store-test-${crypto.randomUUID()}` })

  expect(result).toEqual({
    tasksBefore: 1,
    filesBefore: 1,
    chunksBefore: 1,
    tasksAfter: 0,
    filesAfter: 0,
    chunksAfter: 0,
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
  browserName,
}) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)

  const result = await page.evaluate(async ({ path, db }) => {
    const harness = await import(path) as typeof import('./receive-storage-harness')
    const out = await harness.verifyOPFSProgressiveZipLifecycle(db)
    await new Promise(resolve => setTimeout(resolve, 25))
    return out
  }, { path: HARNESS_PATH, db: `windshare-opfs-test-${crypto.randomUUID()}` })

  expect(result.reopenedF1AlreadyCompleted).toBe(true)
  expect(result.reopenedF2StartEnd).toEqual(['0:6'])
  expect(result.finishedKind).toBe('download-started')
  expect(result.savedAgainKind).toBe('download-started')
  expect(result.finalArchiveName).toBe('bundle.zip')
  expect(result.deletedCleanly).toBe(true)
  expect(await readZip(result.partialBytes)).toEqual({ 'folder/a.txt': 'first-file-payload' })
  expect(await readZip(result.finalArchiveBytes)).toEqual({
    'folder/a.txt': 'first-file-payload',
    'folder/b.txt': 'second-file-payload!',
  })
})

test('FSA backend reserves collision names, verifies physical files on resume, and keeps completed files on discard', async ({
  page,
  browserName,
}) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)

  const result = await page.evaluate(async ({ path, db }) => {
    const harness = await import(path) as typeof import('./receive-storage-harness')
    return harness.verifyFSABackendOverNativeHandles(db)
  }, { path: HARNESS_PATH, db: `windshare-fsa-test-${crypto.randomUUID()}` })

  expect(result).toEqual({
    resumedF1AlreadyCompleted: true,
    resumedF2Ranges: ['0:4'],
    survivingNames: ['pic.bin'],
    taskDeleted: true,
  })
})

test('FSA resume reclaims its uncommitted 0-byte placeholder after closing a page during first write', async ({
  context,
  browserName,
}) => {
  const db = `windshare-fsa-close-test-${crypto.randomUUID()}`
  const sandbox = `fsa-close-sandbox-${crypto.randomUUID()}`
  const taskId = `fsa-close-${crypto.randomUUID()}`

  const firstPage = await context.newPage()
  try {
    await firstPage.goto(BROWSER_CONTRACT_HOST_PATH)
    await requireOriginPrivateStorage(firstPage, browserName)
    await firstPage.evaluate(async ({ path, dbName, sandboxName, id }) => {
      const harness = await import(path) as typeof import('./receive-storage-harness')
      await harness.startUncommittedFSAFirstWrite(dbName, sandboxName, id)
    }, { path: HARNESS_PATH, dbName: db, sandboxName: sandbox, id: taskId })
  } finally {
    await firstPage.close()
  }

  const resumedPage = await context.newPage()
  try {
    await resumedPage.goto(BROWSER_CONTRACT_HOST_PATH)
    const result = await resumedPage.evaluate(async ({ path, dbName, sandboxName, id }) => {
      const harness = await import(path) as typeof import('./receive-storage-harness')
      return harness.resumeFSAPlaceholderAfterPageClose(dbName, sandboxName, id)
    }, { path: HARNESS_PATH, dbName: db, sandboxName: sandbox, id: taskId })

    expect(result).toEqual({
      beforeSize: 0,
      hadRecordBeforeResume: false,
      finalBytes: [1, 2, 3, 4, 5, 6, 7, 8],
      completedAfterResume: true,
    })
  } finally {
    await resumedPage.close()
  }
})

test('FSA discard preserves foreign and replaced files and removes the result directory only when empty', async ({
  page,
  browserName,
}) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)

  const result = await page.evaluate(async ({ path, db }) => {
    const harness = await import(path) as typeof import('./receive-storage-harness')
    return harness.verifyFSADiscardSafetyOverNativeHandles(db)
  }, { path: HARNESS_PATH, db: `windshare-fsa-discard-test-${crypto.randomUUID()}` })

  expect(result).toEqual({
    tripNames: ['user-note.txt'],
    preservedText: 'do-not-delete',
    cleanRootRemoved: true,
    replacedText: 'user-owned-replacement',
    dirToFileText: 'user-backup-file',
    allTasksDeleted: true,
  })
})

test('concurrent FSA single-file downloads to the same directory allocate distinct files without overwriting', async ({
  page,
  browserName,
}) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)

  const result = await page.evaluate(async ({ path, db }) => {
    const harness = await import(path) as typeof import('./receive-storage-harness')
    return harness.verifyFSAConcurrentSingleFileAllocationOverNativeHandles(db)
  }, { path: HARNESS_PATH, db: `windshare-fsa-concurrent-${crypto.randomUUID()}` })

  expect(result).toEqual({
    lifecycleKinds: ['saved', 'saved'],
    names: ['report (1).bin', 'report.bin'],
    contents: ['report-from-task-a', 'report-from-task-b-longer'],
  })
})

test('portable backend streams valid Store-mode ZIP archives to browser download', async ({ page }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const result = await page.evaluate(async path => {
    const harness = await import(path) as typeof import('./receive-storage-harness')
    const out = await harness.verifyPortableStreamingZip()
    await new Promise(resolve => setTimeout(resolve, 25))
    return out
  }, HARNESS_PATH)

  expect(result.lifecycleKind).toBe('download-started')
  expect(result.downloadName).toBe('quick.zip')
  expect(await readZip(result.archiveBytes)).toEqual({ 'note.txt': 'hello' })
})
