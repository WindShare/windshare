import { expect, test, type Page } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'

import { encodeBase64Url } from '../../src/crypto/bytes'
import { requireOriginPrivateStorage } from './browser-storage-support'
import type { FsaNamespaceFixture } from './fsa-namespace-atomicity-harness'

const HARNESS_PATH = '/test/browser/fsa-namespace-atomicity-harness.ts'
const INDEXEDDB_DATABASE_PATH = '/src/output/browser/indexeddb-database.ts'
const INDEXEDDB_REPOSITORY_PATH = '/src/output/browser/indexeddb-repository.ts'

interface NamespaceHarness {
  exerciseNativeMutationScheduling(fixture: FsaNamespaceFixture): Promise<{
    siblingInspection: string
    siblingCreatedBeforeWriterClose: boolean
    siblingWriterCompletedBeforeWriterClose: boolean
    removalStartedBeforeWriterClose: boolean
    laterWriterAdmittedDuringRemoval: boolean
    eventOrder: readonly string[]
    siblingFileBytes: readonly number[]
    independentParentFileBytes: readonly number[]
    peakActiveWriters: number
  }>
  exerciseNativeWriterFailureRelease(fixture: FsaNamespaceFixture): Promise<{
    closeFailureObserved: boolean
    abortFailureObserved: boolean
    successorBytes: readonly number[]
    acquiredWriterLeases: number
    releasedWriterLeases: number
  }>
  holdNativeRootThroughWriterDrain(fixture: FsaNamespaceFixture): Promise<void>
  beginHeldNativeRootDrain(fixture: FsaNamespaceFixture): Promise<{
    schedulerState: string
    lateWriterRejection: string
    lateNamespaceRejection: string
    rootReleasePending: boolean
  }>
  probeCompetingNativeRoot(fixture: FsaNamespaceFixture): Promise<{
    busy: boolean
    scope: string | null
  }>
  finishHeldNativeRootDrain(fixture: FsaNamespaceFixture): Promise<void>
  cleanupHeldNativeRootDrain(fixture: FsaNamespaceFixture): Promise<void>
  exerciseTaskRootRestart(fixture: FsaNamespaceFixture): Promise<{
    firstCollisionIndex: number
    suffixPersisted: boolean
    rootIdentityPersisted: boolean
    newTaskIsolated: boolean
    directoryCount: number
  }>
  exerciseSingleFileLayout(fixture: FsaNamespaceFixture): Promise<{
    emptyBeforeRevision: boolean
    revisionOpenedBeforeCreation: boolean
    noExtraRoot: boolean
    prefixVisible: boolean
    restartReusedFile: boolean
    completedBytes: number
  }>
  holdTaskRoot(fixture: FsaNamespaceFixture): Promise<void>
  probeCompetingTask(fixture: FsaNamespaceFixture): Promise<{
    busy: boolean
    scope: string | null
  }>
  releaseHeldTask(fixture: FsaNamespaceFixture): Promise<void>
  exerciseFailedPreExecutionActivation(fixture: FsaNamespaceFixture): Promise<{
    rootAbsentBeforeExecution: boolean
    rootAbsentAfterDetach: boolean
  }>
  exerciseSafeNameScenarioClosure(fixture: FsaNamespaceFixture): Promise<{
    refusals: readonly {
      scope: 'file' | 'directory' | 'result-root'
      expectedKind: 'file' | 'directory'
      rejectionCallCount: number
      rejectedEntriesBefore: readonly string[]
      contentBlockRequestCountAtRefusal: number
      logicalEntryAbsent: boolean
      safeComponentName: boolean
      summaryCount: number
      extraEntriesCount: number
    }[]
    ordinary: {
      summaryPresent: boolean
      unexpectedLookupCount: number
      outputNames: readonly string[]
      bothRevisionsAdmittedBeforeRelease: boolean
    }
  }>
}

test('native FSA sibling files progress while same-file removal drains its writer', async ({
  browserName,
  page,
}) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)
  expect(await callHarness(
    page,
    'exerciseNativeMutationScheduling',
    testFixture('native-scheduler'),
  )).toEqual({
    siblingInspection: 'NotFoundError',
    siblingCreatedBeforeWriterClose: true,
    siblingWriterCompletedBeforeWriterClose: true,
    removalStartedBeforeWriterClose: false,
    laterWriterAdmittedDuringRemoval: false,
    eventOrder: [
      'sibling-created',
      'independent-parent-created',
      'same-file-removal',
      'later-same-file-writer',
    ],
    siblingFileBytes: [4, 5],
    independentParentFileBytes: [8, 9],
    peakActiveWriters: 2,
  })
})

test('native writable close and abort failures release scheduler capacity', async ({
  browserName,
  page,
}) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)
  expect(await callHarness(
    page,
    'exerciseNativeWriterFailureRelease',
    testFixture('native-writer-failure'),
  )).toEqual({
    closeFailureObserved: true,
    abortFailureObserved: true,
    successorBytes: [21, 22, 23],
    acquiredWriterLeases: 3,
    releasedWriterLeases: 3,
  })
})

test('FSA parent Web Lock remains held through writer drain and rejects late admission', async ({
  browserName,
  context,
  page,
}) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)
  const competitor = await context.newPage()
  await competitor.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(competitor, browserName)
  const fixture = testFixture('native-cross-tab-drain')
  try {
    await callHarness(page, 'holdNativeRootThroughWriterDrain', fixture)
    expect(await callHarness(competitor, 'probeCompetingNativeRoot', fixture)).toEqual({
      busy: true,
      scope: 'fsa-parent',
    })
    expect(await callHarness(page, 'beginHeldNativeRootDrain', fixture)).toEqual({
      schedulerState: 'draining',
      lateWriterRejection: 'InvalidStateError',
      lateNamespaceRejection: 'InvalidStateError',
      rootReleasePending: true,
    })
    expect(await callHarness(competitor, 'probeCompetingNativeRoot', fixture)).toEqual({
      busy: true,
      scope: 'fsa-parent',
    })

    await callHarness(page, 'finishHeldNativeRootDrain', fixture)
    expect(await callHarness(competitor, 'probeCompetingNativeRoot', fixture)).toEqual({
      busy: false,
      scope: null,
    })
  } finally {
    await callHarness(page, 'cleanupHeldNativeRootDrain', fixture).catch(() => undefined)
    await competitor.close()
  }
})

test('FSA task roots keep suffix and ownership across restart', async ({ browserName, page }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)
  const fixture = testFixture('task-root')
  expect(await callHarness(page, 'exerciseTaskRootRestart', fixture)).toEqual({
    firstCollisionIndex: 1,
    suffixPersisted: true,
    rootIdentityPersisted: true,
    newTaskIsolated: true,
    directoryCount: 3,
  })
})

test('single-file DirectoryTree has no extra root and resumes its visible prefix', async ({
  browserName,
  page,
}) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)
  const fixture = testFixture('single-file')
  expect(await callHarness(page, 'exerciseSingleFileLayout', fixture)).toEqual({
    emptyBeforeRevision: true,
    revisionOpenedBeforeCreation: true,
    noExtraRoot: true,
    prefixVisible: true,
    restartReusedFile: true,
    completedBytes: 4,
  })
})

test('FSA parent Web Lock spans tabs', async ({ browserName, context, page }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)
  const competitor = await context.newPage()
  await competitor.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(competitor, browserName)
  const fixture = testFixture('cross-tab')
  await callHarness(page, 'holdTaskRoot', fixture)
  expect(await callHarness(competitor, 'probeCompetingTask', fixture)).toEqual({
    busy: true,
    scope: 'fsa-parent',
  })
  await callHarness(page, 'releaseHeldTask', fixture)
  await competitor.close()
})

test('failed activation before DirectTree execution leaves no visible task root', async ({
  browserName,
  page,
}) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)
  expect(await callHarness(
    page,
    'exerciseFailedPreExecutionActivation',
    testFixture('pre-execution-failure'),
  )).toEqual({ rootAbsentBeforeExecution: true, rootAbsentAfterDetach: true })
})

test('safe-name fallback is exact-call-locus, writes no extra files, and stays dormant for ordinary output', async ({
  browserName,
  page,
}) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)
  const proof = await callHarness<Awaited<ReturnType<
    NamespaceHarness['exerciseSafeNameScenarioClosure']
  >>>(page, 'exerciseSafeNameScenarioClosure', testFixture('safe-name-scenarios'))
  expect(proof).toEqual({
    refusals: [
      {
        scope: 'file',
        expectedKind: 'file',
        rejectionCallCount: 1,
        rejectedEntriesBefore: [],
        contentBlockRequestCountAtRefusal: 0,
        logicalEntryAbsent: true,
        safeComponentName: true,
        summaryCount: 1,
        extraEntriesCount: 0,
      },
      {
        scope: 'directory',
        expectedKind: 'directory',
        rejectionCallCount: 1,
        rejectedEntriesBefore: [],
        contentBlockRequestCountAtRefusal: 0,
        logicalEntryAbsent: true,
        safeComponentName: true,
        summaryCount: 1,
        extraEntriesCount: 0,
      },
      {
        scope: 'result-root',
        expectedKind: 'directory',
        rejectionCallCount: 1,
        rejectedEntriesBefore: [],
        contentBlockRequestCountAtRefusal: 0,
        logicalEntryAbsent: true,
        safeComponentName: true,
        summaryCount: 1,
        extraEntriesCount: 0,
      },
    ],
    ordinary: {
      summaryPresent: false,
      unexpectedLookupCount: 0,
      outputNames: ['ordinary-a.bin', 'ordinary-b.bin'],
      bothRevisionsAdmittedBeforeRelease: true,
    },
  })
})

test('IndexedDB closes on versionchange and rejects blocked upgrades without leaking a connection', async ({
  browserName,
  page,
}) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)
  const result = await page.evaluate(async ({ upgradeName, blockedName, operationId, modulePaths }) => {
    const databaseModule = await import(modulePaths.database) as
      typeof import('../../src/output/browser/indexeddb-database')
    const repositoryModule = await import(modulePaths.repository) as
      typeof import('../../src/output/browser/indexeddb-repository')
    const legacyReceiveLeaseStore = 'receive-operation-v1-leases'
    const v8 = await openRaw(upgradeName, databaseModule.OUTPUT_DATABASE_VERSION - 1, database => {
      database.createObjectStore(legacyReceiveLeaseStore, { keyPath: 'id' })
    })
    const write = v8.transaction(legacyReceiveLeaseStore, 'readwrite')
    write.objectStore(legacyReceiveLeaseStore).put({
      id: `legacy-${operationId}`,
      operationId,
    })
    await transactionDone(write)
    v8.close()

    const upgraded = await repositoryModule.IndexedDbReceiveOperationRepository.open(upgradeName)
    upgraded.close()
    const reopened = await repositoryModule.IndexedDbReceiveOperationRepository.open(upgradeName)
    const rawV9 = await openRaw(upgradeName, databaseModule.CHECKPOINT_DATABASE_VERSION)
    const legacyLeaseStorePresent = rawV9.objectStoreNames.contains(legacyReceiveLeaseStore)
    const countTransaction = rawV9.transaction(
      databaseModule.INDEXEDDB_RECEIVE_LEASE_STORE,
      'readonly',
    )
    const currentLeaseCount = await requestValue<number>(
      countTransaction.objectStore(databaseModule.INDEXEDDB_RECEIVE_LEASE_STORE).count(),
    )
    await transactionDone(countTransaction)
    rawV9.close()
    const laterVersion = await openRaw(
      upgradeName,
      databaseModule.CHECKPOINT_DATABASE_VERSION + 1,
    )
    let closedAfterVersionchange = false
    try {
      await reopened.readLifecycle(operationId)
    } catch (error) {
      closedAfterVersionchange = error instanceof DOMException && error.name === 'InvalidStateError'
    }
    laterVersion.close()
    await deleteRaw(upgradeName)

    const blocker = await openRaw(blockedName, databaseModule.OUTPUT_DATABASE_VERSION - 1, database => {
      database.createObjectStore('incompatible-format', { keyPath: 'id' })
    })
    let blockedRejected = false
    await repositoryModule.IndexedDbReceiveOperationRepository.open(blockedName).catch((error: unknown) => {
      blockedRejected = error instanceof DOMException && error.name === 'InvalidStateError'
    })
    blocker.close()
    // The rejected open request still completes later; its blocked flag must close that late connection
    // so a following upgrade can finish without timers or cross-tab cleanup heuristics.
    const afterBlocked = await openRaw(blockedName, databaseModule.CHECKPOINT_DATABASE_VERSION)
    afterBlocked.close()
    await deleteRaw(blockedName)
    return {
      legacyLeaseStorePresent,
      currentLeaseCount,
      closedAfterVersionchange,
      blockedRejected,
      lateConnectionClosed: true,
    }

    function openRaw(
      databaseName: string,
      version: number,
      upgrade?: (database: IDBDatabase, transaction: IDBTransaction) => void,
    ): Promise<IDBDatabase> {
      return new Promise((resolve, reject) => {
        const request = indexedDB.open(databaseName, version)
        request.addEventListener('upgradeneeded', () => {
          if (request.transaction !== null) upgrade?.(request.result, request.transaction)
        })
        request.addEventListener('success', () => resolve(request.result), { once: true })
        request.addEventListener('error', () => reject(request.error), { once: true })
        request.addEventListener('blocked', () => reject(
          new DOMException('raw test upgrade was blocked', 'InvalidStateError'),
        ), { once: true })
      })
    }

    function requestValue<T>(request: IDBRequest<T>): Promise<T> {
      return new Promise((resolve, reject) => {
        request.addEventListener('success', () => resolve(request.result), { once: true })
        request.addEventListener('error', () => reject(request.error), { once: true })
      })
    }

    function transactionDone(transaction: IDBTransaction): Promise<void> {
      return new Promise((resolve, reject) => {
        transaction.addEventListener('complete', () => resolve(), { once: true })
        transaction.addEventListener('abort', () => reject(transaction.error), { once: true })
        transaction.addEventListener('error', () => reject(transaction.error), { once: true })
      })
    }

    function deleteRaw(databaseName: string): Promise<void> {
      return new Promise((resolve, reject) => {
        const request = indexedDB.deleteDatabase(databaseName)
        request.addEventListener('success', () => resolve(), { once: true })
        request.addEventListener('error', () => reject(request.error), { once: true })
        request.addEventListener('blocked', () => reject(
          new DOMException('raw test deletion was blocked', 'InvalidStateError'),
        ), { once: true })
      })
    }
  }, {
    upgradeName: `fsa-v8-upgrade-${crypto.randomUUID()}`,
    blockedName: `fsa-v8-blocked-${crypto.randomUUID()}`,
    operationId: identity(16, 51),
    modulePaths: {
      database: INDEXEDDB_DATABASE_PATH,
      repository: INDEXEDDB_REPOSITORY_PATH,
    },
  })

  expect(result).toEqual({
    legacyLeaseStorePresent: false,
    currentLeaseCount: 0,
    closedAfterVersionchange: true,
    blockedRejected: true,
    lateConnectionClosed: true,
  })
})

function testFixture(label: string): FsaNamespaceFixture {
  const nonce = crypto.randomUUID()
  return Object.freeze({
    databaseName: `fsa-${label}-${nonce}`,
    parentName: `fsa-${label}-${nonce}`,
  })
}

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}

async function callHarness<T>(
  page: Page,
  method: keyof NamespaceHarness,
  fixture: FsaNamespaceFixture,
): Promise<T> {
  return page.evaluate(async ({ path, operation, value }) => {
    const harness = (await import(path)) as NamespaceHarness
    return harness[operation](value) as Promise<T>
  }, { path: HARNESS_PATH, operation: method, value: fixture })
}
