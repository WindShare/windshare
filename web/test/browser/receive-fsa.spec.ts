import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'
import { requireOriginPrivateStorage } from './browser-storage-support'

const HARNESS_PATH = '/test/browser/receive-fsa-harness.ts'

test('FSA backend reserves collision names, verifies physical files on resume, and keeps completed files on discard', async ({
  page,
  browserName,
}) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)

  const result = await page.evaluate(async ({ path, db }) => {
    const harness = await import(path) as typeof import('./receive-fsa-harness')
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
      const harness = await import(path) as typeof import('./receive-fsa-harness')
      await harness.startUncommittedFSAFirstWrite(dbName, sandboxName, id)
    }, { path: HARNESS_PATH, dbName: db, sandboxName: sandbox, id: taskId })
  } finally {
    await firstPage.close()
  }

  const resumedPage = await context.newPage()
  try {
    await resumedPage.goto(BROWSER_CONTRACT_HOST_PATH)
    const result = await resumedPage.evaluate(async ({ path, dbName, sandboxName, id }) => {
      const harness = await import(path) as typeof import('./receive-fsa-harness')
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
    const harness = await import(path) as typeof import('./receive-fsa-harness')
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
    const harness = await import(path) as typeof import('./receive-fsa-harness')
    return harness.verifyFSAConcurrentSingleFileAllocationOverNativeHandles(db)
  }, { path: HARNESS_PATH, db: `windshare-fsa-concurrent-${crypto.randomUUID()}` })

  expect(result).toEqual({
    lifecycleKinds: ['saved', 'saved'],
    names: ['report (1).bin', 'report.bin'],
    contents: ['report-from-task-a', 'report-from-task-b-longer'],
  })
})
