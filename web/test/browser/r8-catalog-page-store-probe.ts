import {
  catalogPageMetadataCharge,
  IndexedDbV2CatalogPageStore,
  V2CatalogPageStoreError,
  type V2CatalogCacheBudgetLimits,
  type V2CommittedDirectory,
} from '../../src/catalog/v2-page-store'
import {
  V2CatalogClient,
  V2CatalogClientError,
  V2DirectoryFailureError,
} from '../../src/catalog/v2-client'
import type {
  V2CatalogEntry,
  V2CatalogPage,
  V2DirectoryFailure,
} from '../../src/catalog/v2-records'
import { waitForIndexedDbTransaction } from '../../src/catalog/v2-indexeddb-transaction'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { createSignedRootCollisionFixture } from '../catalog/signed-root-collision-fixture'
import { runSignedCatalogOwnershipProbe } from '../catalog/signed-catalog-ownership-probe'

const LEGACY_DATABASE_VERSION = 3
const CURRENT_DATABASE_VERSION = 5
const DIRECTORY_STORE = 'committed-directories'
const PAGE_STORE = 'catalog-pages'
const BUDGET_STORE = 'catalog-budget'
const LEGACY_NODE_STORE = 'catalog-node-owners'
const LEGACY_NAME_STORE = 'catalog-name-owners'
const LEGACY_SENTINEL_STORE = 'legacy-sentinel'
const INDEXED_DB_HANG_CEILING_MILLISECONDS = 30_000

export async function probeSamePageCollisions(): Promise<Readonly<Record<string, boolean>>> {
  const databaseName = uniqueDatabaseName('same-page')
  const store = await IndexedDbV2CatalogPageStore.open('share', databaseName)
  try {
    await store.begin('root')
    const nodeCollisionRejected = await rejectsAuthenticatedOwnership(store, page(0, 'root', [
      entry('node-1', 'first'),
      entry('node-1', 'second'),
    ]))
    await store.stage(page(0, 'root', [entry('node-1', 'first')]))
    await store.abort('root')

    const nameCollisionRejected = await rejectsAuthenticatedOwnership(store, page(0, 'root', [
      entry('node-1', 'Straße'),
      entry('node-2', 'STRASSE'),
    ]))
    await store.stage(page(0, 'root', [entry('node-2', 'STRASSE')]))
    return Object.freeze({
      nodeCollisionRejected,
      nameCollisionRejected,
      failedPageKeyRemainedReusable: true,
    })
  } finally {
    store.close()
    await deleteDatabase(databaseName)
  }
}

export async function probeCrossPageOwnership(): Promise<Readonly<Record<string, boolean>>> {
  const databaseName = uniqueDatabaseName('cross-page')
  const store = await IndexedDbV2CatalogPageStore.open('share', databaseName)
  let otherShare: IndexedDbV2CatalogPageStore | undefined
  try {
    await store.begin('root')
    await store.stage(page(0, 'root', [entry('node-1', 'Straße')]))

    const crossPageNameRejected = await rejectsAuthenticatedOwnership(
      store,
      page(1, 'root', [entry('node-2', 'STRASSE')]),
    )
    await store.stage(page(1, 'root', [entry('node-2', 'valid-name')]))

    const crossPageNodeRejected = await rejectsAuthenticatedOwnership(
      store,
      page(2, 'root', [entry('node-1', 'released-name')]),
    )
    await store.stage(page(2, 'root', [entry('node-3', 'released-name')]))

    await store.begin('second')
    const crossDirectoryNodeRejected = await rejectsAuthenticatedOwnership(
      store,
      page(0, 'second', [entry('node-1', 'second-only')]),
    )
    await store.stage(page(0, 'second', [entry('node-4', 'STRASSE')]))

    otherShare = await IndexedDbV2CatalogPageStore.open('other-share', databaseName)
    await otherShare.stage(page(0, 'root', [entry('node-1', 'Straße')]))
    return Object.freeze({
      crossPageNameRejected,
      crossPageNodeRejected,
      crossDirectoryNodeRejected,
      failedNodeOwnershipRolledBack: true,
      failedNameOwnershipRolledBack: true,
      namesRemainDirectoryScoped: true,
      ownershipRemainsShareScoped: true,
    })
  } finally {
    otherShare?.close()
    store.close()
    await deleteDatabase(databaseName)
  }
}

export async function probeCommitAbortAndReopen(): Promise<Readonly<Record<string, boolean>>> {
  const databaseName = uniqueDatabaseName('lifecycle')
  const committed = directory('root')
  const storedPage = page(0, 'root', [entry('node-1', 'durable-name')])
  let store: IndexedDbV2CatalogPageStore | undefined
  try {
    store = await IndexedDbV2CatalogPageStore.open('share', databaseName)
    await store.begin('root')
    await store.stage(storedPage)
    await store.commit(committed)
    store.close()

    store = await IndexedDbV2CatalogPageStore.open('share', databaseName)
    const reopenedDirectory = await store.loadDirectory('root')
    const reopenedPage = reopenedDirectory === undefined
      ? undefined
      : await store.loadPage(reopenedDirectory, 0)
    const commitSurvivedReopen = reopenedDirectory?.generationText === committed.generationText &&
      reopenedPage?.entries[0]?.idText === 'node-1'
    await store.abort('root')
    store.close()

    store = await IndexedDbV2CatalogPageStore.open('share', databaseName)
    const abortSurvivedReopen = await store.loadDirectory('root') === undefined
    await store.stage(storedPage)
    store.close()

    store = await IndexedDbV2CatalogPageStore.open('share', databaseName)
    const crashResidueRejected = await rejectsAuthenticatedOwnership(store, storedPage)
    await store.begin('root')
    await store.stage(storedPage)
    await store.commit(committed)
    return Object.freeze({
      commitSurvivedReopen,
      abortSurvivedReopen,
      abortReleasedPageAndOwnershipKeys: true,
      crashResidueRejected,
      beginReleasedCrashResidue: true,
    })
  } finally {
    store?.close()
    await deleteDatabase(databaseName)
  }
}

export async function probeSignedRootCollision(): Promise<Readonly<Record<string, boolean>>> {
  const databaseName = uniqueDatabaseName('root-collision')
  const fixture = await createSignedRootCollisionFixture()
  const store = await IndexedDbV2CatalogPageStore.open(
    fixture.descriptor.shareInstanceId,
    databaseName,
  )
  const protocolFailures: unknown[] = []
  const client = new V2CatalogClient({
    descriptor: fixture.descriptor,
    readSecret: fixture.readSecret,
    operations: {
      fetchPage: async () => fixture.catalogObject,
      failProtocol: async (reason) => { protocolFailures.push(reason) },
    },
    store,
  })
  let reopened: IndexedDbV2CatalogPageStore | undefined
  try {
    let signedTrafficRejected = false
    try {
      await client.loadDirectory(fixture.descriptor.syntheticRoot)
    } catch (error) {
      if (!(error instanceof V2CatalogClientError)) throw error
      signedTrafficRejected = true
    }
    const noCommitBeforeClose = await store.loadDirectory(
      fixture.descriptor.syntheticRootId,
    ) === undefined
    client.close()
    reopened = await IndexedDbV2CatalogPageStore.open(
      fixture.descriptor.shareInstanceId,
      databaseName,
    )
    const noCommitAfterReopen = await reopened.loadDirectory(
      fixture.descriptor.syntheticRootId,
    ) === undefined
    return Object.freeze({
      signedTrafficRejected,
      protocolFailureReported: protocolFailures.length === 1,
      noCommitBeforeClose,
      noCommitAfterReopen,
    })
  } finally {
    reopened?.close()
    client.close()
    fixture.close()
    await deleteDatabase(databaseName)
  }
}

export async function probeSignedOwnershipCollisions() {
  const databaseName = uniqueDatabaseName('signed-ownership-collisions')
  const store = await IndexedDbV2CatalogPageStore.open('signed-share', databaseName)
  try {
    return await runSignedCatalogOwnershipProbe(store)
  } finally {
    store.close()
    await deleteDatabase(databaseName)
  }
}

export async function probeCompositeKeyIsolation(): Promise<Readonly<Record<string, boolean>>> {
  const databaseName = uniqueDatabaseName('composite-keys')
  const first = await IndexedDbV2CatalogPageStore.open('share\0scope', databaseName)
  const second = await IndexedDbV2CatalogPageStore.open('share', databaseName)
  try {
    const firstPage = page(0, 'root', [entry('node\0id', 'same')])
    const secondPage = page(0, 'scope\0root', [entry('scope\0node\0id', 'same')])
    await first.stage(firstPage)
    await first.commit(directory('root'))
    await second.stage(secondPage)
    await second.commit(directory('scope\0root'))

    await second.abort('scope\0root')
    const firstDirectory = await first.loadDirectory('root')
    const firstPageAfterOtherAbort = firstDirectory === undefined
      ? undefined
      : await first.loadPage(firstDirectory, 0)
    let delimiterNameRejected = false
    try {
      await first.stage(page(1, 'root', [entry('safe-node', 'bad\0name')]))
    } catch (error) {
      if (!(error instanceof TypeError)) throw error
      delimiterNameRejected = true
    }
    return Object.freeze({
      collidingLegacyKeysCoexisted: firstPageAfterOtherAbort?.entries[0]?.idText === 'node\0id',
      crossShareAbortStayedScoped: await second.loadDirectory('scope\0root') === undefined,
      delimiterNameRejected,
    })
  } finally {
    second.close()
    first.close()
    await deleteDatabase(databaseName)
  }
}

export async function probeBlockedUpgrade(): Promise<Readonly<Record<string, boolean>>> {
  const databaseName = uniqueDatabaseName('blocked-upgrade')
  const legacy = await openLegacyDatabase(databaseName)
  let fresh: IndexedDbV2CatalogPageStore | undefined
  try {
    let blockedRejected = false
    let actionableMessage = false
    try {
      await withHangCeiling(IndexedDbV2CatalogPageStore.open('share', databaseName))
    } catch (error) {
      if (!(error instanceof V2CatalogPageStoreError)) throw error
      blockedRejected = true
      actionableMessage = /blocked.*close.*retry/iu.test(error.message)
    }
    legacy.close()
    fresh = await withHangCeiling(IndexedDbV2CatalogPageStore.open('share', databaseName))
    await fresh.stage(page(0, 'root', [entry('node-1', 'fresh')]))
    return Object.freeze({ blockedRejected, actionableMessage, freshOpenSucceeded: true })
  } finally {
    legacy.close()
    fresh?.close()
    await deleteDatabase(databaseName)
  }
}

export async function probeFailurePersistence(): Promise<Readonly<Record<string, boolean>>> {
  const databaseName = uniqueDatabaseName('failures')
  const fixture = await createSignedRootCollisionFixture()
  const store = await IndexedDbV2CatalogPageStore.open(
    fixture.descriptor.shareInstanceId,
    databaseName,
  )
  const permanentId = identity(0x61)
  const retryableId = identity(0x62)
  const permanent = directoryFailure(fixture.descriptor.shareInstance, permanentId, false)
  const retryable = directoryFailure(fixture.descriptor.shareInstance, retryableId, true)
  await store.storeFailure({ failure: permanent, retryAtMilliseconds: null })
  await store.storeFailure({ failure: retryable, retryAtMilliseconds: 1_000 })
  let now = 999
  let fetches = 0
  const client = new V2CatalogClient({
    descriptor: fixture.descriptor,
    readSecret: fixture.readSecret,
    operations: {
      fetchPage: async () => {
        fetches += 1
        throw new Error('post-cooldown fetch reached transport')
      },
    },
    store,
    now: () => now,
  })
  let reopened: IndexedDbV2CatalogPageStore | undefined
  try {
    const permanentReused = await rejectsDirectoryFailure(
      client.loadDirectory(permanentId, { explicitRetry: true }),
    )
    const cooldownReused = await rejectsDirectoryFailure(
      client.loadDirectory(retryableId, { explicitRetry: true }),
    )
    now = 1_000
    let postCooldownFetch = false
    try {
      await client.loadDirectory(retryableId, { explicitRetry: true })
    } catch (error) {
      postCooldownFetch = error instanceof Error && error.message === 'post-cooldown fetch reached transport'
    }
    client.close()
    reopened = await IndexedDbV2CatalogPageStore.open(
      fixture.descriptor.shareInstanceId,
      databaseName,
    )
    return Object.freeze({
      permanentReused,
      cooldownReused,
      transportSkippedBeforeCooldown: fetches === 1,
      postCooldownFetch,
      permanentSurvivedReopen: await reopened.loadFailure(permanent.directoryIdText) !== undefined,
      retryableClearedByAttempt: await reopened.loadFailure(retryable.directoryIdText) === undefined,
    })
  } finally {
    reopened?.close()
    client.close()
    fixture.close()
    await deleteDatabase(databaseName)
  }
}

export async function probeAggregateBudgetAuthority(): Promise<Readonly<Record<string, boolean>>> {
  const databaseName = uniqueDatabaseName('aggregate-budget')
  const charge = catalogPageMetadataCharge(page(0, 'root', [entry('node-1', 'first')]))
  const limits: V2CatalogCacheBudgetLimits = {
    entriesPerShare: 3,
    entriesPerProfile: 3,
    metadataBytesPerShare: charge * 3,
    metadataBytesPerProfile: charge * 3,
  }
  const first = await IndexedDbV2CatalogPageStore.open('share-a', databaseName, limits)
  const sameShare = await IndexedDbV2CatalogPageStore.open('share-a', databaseName, limits)
  const otherShare = await IndexedDbV2CatalogPageStore.open('share-b', databaseName, limits)
  try {
    const sameShareRace = await Promise.allSettled([
      first.stage(page(0, 'first', [entry('node-1', 'first'), entry('node-2', 'second')])),
      sameShare.stage(page(0, 'second', [entry('node-3', 'third'), entry('node-4', 'fourth')])),
    ])
    const shareRaceWasAtomic = fulfilledCount(sameShareRace) === 1 &&
      resourceLimitCount(sameShareRace) === 1

    const profileRace = await Promise.allSettled([
      otherShare.stage(page(0, 'other', [entry('node-5', 'fifth'), entry('node-6', 'sixth')])),
      otherShare.stage(page(1, 'other', [entry('node-7', 'seventh'), entry('node-8', 'eighth')])),
    ])
    const profileLimitRejected = fulfilledCount(profileRace) === 0 &&
      resourceLimitCount(profileRace) === 2

    await first.abort('first')
    await sameShare.abort('second')
    await otherShare.stage(page(0, 'other', [entry('node-5', 'fifth')]))
    await otherShare.commit({ ...directory('other'), entryCount: 1 })
    otherShare.close()
    await nextTask()

    const reopened = await IndexedDbV2CatalogPageStore.open('share-b', databaseName, limits)
    try {
      const recoveredChargeRejected = await rejectsResourceLimit(
        reopened.stage(page(1, 'new', [
          entry('node-6', 'sixth'),
          entry('node-7', 'seventh'),
          entry('node-8', 'eighth'),
        ])),
      )
      await reopened.abort('other')
      await reopened.stage(page(0, 'new', [
        entry('node-6', 'sixth'),
        entry('node-7', 'seventh'),
        entry('node-8', 'eighth'),
      ]))
      return Object.freeze({
        shareRaceWasAtomic,
        profileLimitRejected,
        recoveredChargeRejected,
        abortReleasedBudget: true,
      })
    } finally {
      reopened.close()
    }
  } finally {
    otherShare.close()
    sameShare.close()
    first.close()
    await nextTask()
    await deleteDatabase(databaseName)
  }
}

export async function probeInactiveShareEviction(): Promise<Readonly<Record<string, boolean>>> {
  const databaseName = uniqueDatabaseName('inactive-eviction')
  const candidate = await IndexedDbV2CatalogPageStore.open('candidate', databaseName)
  const candidatePeer = await IndexedDbV2CatalogPageStore.open('candidate', databaseName)
  const peer = await IndexedDbV2CatalogPageStore.open('peer', databaseName)
  let reopened: IndexedDbV2CatalogPageStore | undefined
  try {
    await candidate.stage(page(0, 'root', [entry('node-1', 'first')]))
    await candidate.commit(directory('root'))
    const activeShareProtected = !await peer.evictOldestInactiveShare()
    const activeExplicitEvictionRejected = await rejectsResourceLimit(candidate.evictShare())

    candidate.close()
    candidatePeer.close()
    await nextTask()
    const firstEviction = await peer.evictOldestInactiveShare()
    const repeatedEviction = await peer.evictOldestInactiveShare()
    reopened = await IndexedDbV2CatalogPageStore.open('candidate', databaseName)
    const committedRemoved = await reopened.loadDirectory('root') === undefined
    return Object.freeze({
      activeShareProtected,
      activeExplicitEvictionRejected,
      inactiveShareEvicted: firstEviction,
      repeatedEvictionWasIdempotent: !repeatedEviction,
      committedRemoved,
    })
  } finally {
    reopened?.close()
    peer.close()
    candidatePeer.close()
    candidate.close()
    await nextTask()
    await deleteDatabase(databaseName)
  }
}

export async function probeMalformedBudgetChargeFailsClosed(): Promise<Readonly<Record<string, boolean>>> {
  const databaseName = uniqueDatabaseName('malformed-budget-charge')
  const store = await IndexedDbV2CatalogPageStore.open('share', databaseName)
  let database: IDBDatabase | undefined
  try {
    await store.stage(page(0, 'root', [entry('node-1', 'first')]))
    database = await openCurrentDatabase(databaseName)
    const corruption = database.transaction(PAGE_STORE, 'readwrite')
    const pages = corruption.objectStore(PAGE_STORE)
    const key: IDBValidKey = ['share', 'root', 'generation', 0]
    const record = await requestResult<Record<string, unknown>>(pages.get(key))
    pages.put({ ...record, entryCharge: -1 })
    await waitForIndexedDbTransaction(corruption)

    let rejectedClosed = false
    try {
      await withHangCeiling(store.abort('root'))
    } catch (error) {
      if (!(error instanceof V2CatalogPageStoreError)) throw error
      rejectedClosed = error.kind === 'local-storage' && /budget ledger/iu.test(error.message)
    }
    return Object.freeze({ rejectedClosed, didNotHang: true })
  } finally {
    database?.close()
    store.close()
    await nextTask()
    await deleteDatabase(databaseName)
  }
}

export async function probeEvictionLifecycleRecovery(): Promise<Readonly<Record<string, boolean>>> {
  const databaseName = uniqueDatabaseName('eviction-lifecycle')
  const store = await IndexedDbV2CatalogPageStore.open('candidate', databaseName)
  const observer = await IndexedDbV2CatalogPageStore.open('observer', databaseName)
  let database: IDBDatabase | undefined
  let replacement: IndexedDbV2CatalogPageStore | undefined
  try {
    await store.stage(page(0, 'root', [entry('node-1', 'first')]))
    database = await openCurrentDatabase(databaseName)
    await rewritePageCharge(database, ['candidate', 'root', 'generation', 0], -1)
    database.close()
    database = undefined

    const injectedFailureRejected = await rejectsLocalStorage(withHangCeiling(store.evictShare()))
    const failedEvictionReacquiredProtection = !await observer.evictOldestInactiveShare()

    database = await openCurrentDatabase(databaseName)
    await rewritePageCharge(database, ['candidate', 'root', 'generation', 0], 1)
    database.close()
    database = undefined
    const racingEviction = withHangCeiling(store.evictShare())
    store.close()
    await Promise.allSettled([racingEviction])
    const closedStoreRejected = await rejectsLocalStorage(store.loadDirectory('root'))
    await nextTask()
    replacement = await IndexedDbV2CatalogPageStore.open('candidate', databaseName)
    return Object.freeze({
      injectedFailureRejected,
      failedEvictionReacquiredProtection,
      closeDuringEvictionLeftStoreClosed: closedStoreRejected,
      activityLockWasReleased: true,
    })
  } finally {
    replacement?.close()
    database?.close()
    observer.close()
    store.close()
    await nextTask()
    await deleteDatabase(databaseName)
  }
}

export async function probeSchemaReset(): Promise<Readonly<Record<string, unknown>>> {
  const databaseName = uniqueDatabaseName('schema')
  const legacy = await openLegacyDatabase(databaseName)
  legacy.close()
  const store = await IndexedDbV2CatalogPageStore.open('share', databaseName)
  store.close()

  const database = await openCurrentDatabase(databaseName)
  try {
    const transaction = database.transaction([DIRECTORY_STORE, PAGE_STORE, BUDGET_STORE], 'readonly')
    const directories = transaction.objectStore(DIRECTORY_STORE)
    const pages = transaction.objectStore(PAGE_STORE)
    const budgets = transaction.objectStore(BUDGET_STORE)
    const result = Object.freeze({
      version: database.version,
      storeNames: Array.from(database.objectStoreNames),
      directoryKeyPath: directories.keyPath,
      directoryIndexNames: Array.from(directories.indexNames),
      pageKeyPath: pages.keyPath,
      pageIndexNames: Array.from(pages.indexNames),
      budgetKeyPath: budgets.keyPath,
      budgetIndexNames: Array.from(budgets.indexNames),
      directoryRecords: await requestResult(directories.count()),
      pageRecords: await requestResult(pages.count()),
      budgetRecords: await requestResult(budgets.count()),
      currentVersion: CURRENT_DATABASE_VERSION,
    })
    await waitForIndexedDbTransaction(transaction)
    return result
  } finally {
    database.close()
    await deleteDatabase(databaseName)
  }
}

function entry(idText: string, name: string): V2CatalogEntry {
  const id = new Uint8Array(16)
  id[0] = idText.charCodeAt(idText.length - 1)
  return Object.freeze({ kind: 'file', id, idText, name, expectedSize: 1n })
}

function page(
  pageIndex: number,
  directoryIdText: string,
  entries: readonly V2CatalogEntry[],
): V2CatalogPage {
  return Object.freeze({
    shareInstance: identity(0x11),
    directoryId: identity(directoryIdText.charCodeAt(0)),
    directoryIdText,
    generation: identity(0x33),
    generationText: 'generation',
    pageIndex,
    terminal: false,
    previousCommitment: new Uint8Array(32),
    entries: Object.freeze(entries),
    omittedCount: 0n,
    objectCommitment: new Uint8Array(32),
    senderObjectBytes: 1_024,
  })
}

function directory(directoryIdText: string): V2CommittedDirectory {
  return Object.freeze({
    directoryIdText,
    generationText: 'generation',
    pageCount: 1,
    entryCount: 1,
    omittedCount: 0n,
    terminalCommitment: new Uint8Array(32),
  })
}

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function fulfilledCount(results: readonly PromiseSettledResult<unknown>[]): number {
  return results.filter((result) => result.status === 'fulfilled').length
}

function resourceLimitCount(results: readonly PromiseSettledResult<unknown>[]): number {
  return results.filter((result) =>
    result.status === 'rejected' &&
    result.reason instanceof V2CatalogPageStoreError &&
    result.reason.kind === 'resource-limit').length
}

async function rejectsResourceLimit(operation: Promise<unknown>): Promise<boolean> {
  try {
    await operation
    return false
  } catch (error) {
    if (!(error instanceof V2CatalogPageStoreError)) throw error
    return error.kind === 'resource-limit'
  }
}

async function rejectsLocalStorage(operation: Promise<unknown>): Promise<boolean> {
  try {
    await operation
    return false
  } catch (error) {
    if (!(error instanceof V2CatalogPageStoreError)) throw error
    return error.kind === 'local-storage'
  }
}

async function rewritePageCharge(
  database: IDBDatabase,
  key: IDBValidKey,
  entryCharge: number,
): Promise<void> {
  const transaction = database.transaction(PAGE_STORE, 'readwrite')
  const pages = transaction.objectStore(PAGE_STORE)
  const record = await requestResult<Record<string, unknown>>(pages.get(key))
  pages.put({ ...record, entryCharge })
  await waitForIndexedDbTransaction(transaction)
}

function nextTask(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0))
}

function directoryFailure(
  shareInstance: Uint8Array<ArrayBuffer>,
  directoryId: Uint8Array<ArrayBuffer>,
  retryable: boolean,
): V2DirectoryFailure {
  const attemptId = identity(retryable ? 0x71 : 0x72)
  return Object.freeze({
    shareInstance: shareInstance.slice(),
    directoryId: directoryId.slice(),
    directoryIdText: encodeBase64Url(directoryId),
    attemptId,
    attemptIdText: encodeBase64Url(attemptId),
    code: retryable ? 0x2007 : 0x2006,
    kind: retryable ? 'retryable' : 'permanent',
    retryable,
    ...(retryable ? { retryAfterMilliseconds: 250 } : {}),
  })
}

async function rejectsDirectoryFailure(promise: Promise<unknown>): Promise<boolean> {
  try {
    await promise
    return false
  } catch (error) {
    if (!(error instanceof V2DirectoryFailureError)) throw error
    return true
  }
}

function withHangCeiling<T>(promise: Promise<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timeout = globalThis.setTimeout(() => {
      reject(new Error('IndexedDB operation exceeded its generous hang ceiling'))
    }, INDEXED_DB_HANG_CEILING_MILLISECONDS)
    promise.then(
      (value) => {
        globalThis.clearTimeout(timeout)
        resolve(value)
      },
      (error: unknown) => {
        globalThis.clearTimeout(timeout)
        reject(error)
      },
    )
  })
}

async function rejectsAuthenticatedOwnership(
  store: IndexedDbV2CatalogPageStore,
  candidate: V2CatalogPage,
): Promise<boolean> {
  try {
    await store.stage(candidate)
    return false
  } catch (error) {
    if (!(error instanceof V2CatalogPageStoreError)) throw error
    return error.kind === 'authenticated-ownership'
  }
}

function uniqueDatabaseName(scenario: string): string {
  return `windshare-r8-page-store-${scenario}-${globalThis.crypto.randomUUID()}`
}

function openLegacyDatabase(databaseName: string): Promise<IDBDatabase> {
  return new Promise<IDBDatabase>((resolve, reject) => {
    const request = globalThis.indexedDB.open(databaseName, LEGACY_DATABASE_VERSION)
    request.addEventListener('upgradeneeded', () => {
      const database = request.result
      database.createObjectStore(DIRECTORY_STORE, { keyPath: 'id' }).add({ id: 'legacy' })
      database.createObjectStore(PAGE_STORE, { keyPath: 'id' }).add({ id: 'legacy-page' })
      database.createObjectStore(LEGACY_NODE_STORE, { keyPath: 'id' }).add({ id: 'legacy-node' })
      database.createObjectStore(LEGACY_NAME_STORE, { keyPath: 'id' }).add({ id: 'legacy-name' })
      database.createObjectStore(LEGACY_SENTINEL_STORE).add('legacy', 'sentinel')
    }, { once: true })
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function openCurrentDatabase(databaseName: string): Promise<IDBDatabase> {
  return new Promise<IDBDatabase>((resolve, reject) => {
    const request = globalThis.indexedDB.open(databaseName)
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function deleteDatabase(databaseName: string): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const request = globalThis.indexedDB.deleteDatabase(databaseName)
    request.addEventListener('success', () => resolve(), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('blocked', () => {
      reject(new Error('Catalog page-store probe left an IndexedDB connection open'))
    }, { once: true })
  })
}

function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}
