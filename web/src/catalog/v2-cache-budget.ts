import { V2CatalogPageStoreError } from './v2-page-store-contracts'
import { waitForIndexedDbTransaction } from './v2-indexeddb-transaction'
import {
  V2_CATALOG_PAGE_ENTRIES,
  V2_CATALOG_PAGE_OBJECT_BYTES,
  type V2CatalogPage,
} from './v2-records'

export const CATALOG_DIRECTORY_STORE = 'committed-directories'
export const CATALOG_PAGE_STORE = 'catalog-pages'
export const CATALOG_BUDGET_STORE = 'catalog-budget'
export const CATALOG_DIRECTORY_OWNER_INDEX = 'by-directory-owner'
export const CATALOG_SHARE_OWNER_INDEX = 'by-share-owner'

const CATALOG_PAGE_RECORD_BASE_BYTES = 16 * 1024
const CATALOG_ENTRY_RECORD_BYTES = 512
const CATALOG_AUTHENTICATED_BYTES_MULTIPLIER = 4

export interface V2CatalogCacheBudgetLimits {
  readonly entriesPerShare: number
  readonly entriesPerProfile: number
  readonly metadataBytesPerShare: number
  readonly metadataBytesPerProfile: number
}

export const V2_DEFAULT_CATALOG_CACHE_BUDGET_LIMITS: V2CatalogCacheBudgetLimits = Object.freeze({
  entriesPerShare: 4_194_304,
  entriesPerProfile: 16_777_216,
  metadataBytesPerShare: 2 * 1024 * 1024 * 1024,
  metadataBytesPerProfile: 16 * 1024 * 1024 * 1024,
})

export type CatalogDirectoryOwnerKey = [shareInstanceId: string, directoryIdText: string]

export interface CatalogBudgetedPageRecord {
  readonly shareOwnerKey: string
  readonly entryCharge: number
  readonly metadataChargeBytes: number
}

export type CatalogPageBudgetCharge = Pick<
  CatalogBudgetedPageRecord,
  'entryCharge' | 'metadataChargeBytes'
>

export interface CatalogShareActivityLock {
  readonly release: () => void
  readonly settled: Promise<void>
}

interface StoredBudgetPolicy {
  readonly budgetKey: ['policy']
  readonly kind: 'policy'
  readonly limits: V2CatalogCacheBudgetLimits
}

interface StoredProfileBudget {
  readonly budgetKey: ['profile']
  readonly kind: 'profile'
  readonly entries: number
  readonly metadataBytes: number
}

interface StoredShareBudget {
  readonly budgetKey: ['share', string]
  readonly kind: 'share'
  readonly shareOwnerKey: string
  readonly entries: number
  readonly metadataBytes: number
  readonly lastAccessMilliseconds: number
}

type StoredBudget = StoredBudgetPolicy | StoredProfileBudget | StoredShareBudget

export function catalogPageMetadataCharge(page: V2CatalogPage): number {
  if (
    !Number.isSafeInteger(page.senderObjectBytes) ||
    page.senderObjectBytes <= 0 ||
    page.senderObjectBytes > V2_CATALOG_PAGE_OBJECT_BYTES ||
    page.entries.length > V2_CATALOG_PAGE_ENTRIES
  ) {
    throw new V2CatalogPageStoreError('local-storage', 'Catalog page has an invalid durable footprint')
  }
  // The authenticated object bounds every variable payload. The multiplier
  // covers its decoded copy and both ownership-key projections; fixed charges
  // cover the IndexedDB page and entry record structure.
  return CATALOG_PAGE_RECORD_BASE_BYTES +
    page.entries.length * CATALOG_ENTRY_RECORD_BYTES +
    page.senderObjectBytes * CATALOG_AUTHENTICATED_BYTES_MULTIPLIER
}

export function validateCatalogBudgetLimits(
  input: V2CatalogCacheBudgetLimits,
): V2CatalogCacheBudgetLimits {
  const values = [
    input.entriesPerShare,
    input.entriesPerProfile,
    input.metadataBytesPerShare,
    input.metadataBytesPerProfile,
  ]
  if (
    values.some((value) => !Number.isSafeInteger(value) || value <= 0) ||
    input.entriesPerShare > input.entriesPerProfile ||
    input.metadataBytesPerShare > input.metadataBytesPerProfile ||
    input.entriesPerShare > V2_DEFAULT_CATALOG_CACHE_BUDGET_LIMITS.entriesPerShare ||
    input.entriesPerProfile > V2_DEFAULT_CATALOG_CACHE_BUDGET_LIMITS.entriesPerProfile ||
    input.metadataBytesPerShare > V2_DEFAULT_CATALOG_CACHE_BUDGET_LIMITS.metadataBytesPerShare ||
    input.metadataBytesPerProfile > V2_DEFAULT_CATALOG_CACHE_BUDGET_LIMITS.metadataBytesPerProfile
  ) {
    throw new V2CatalogPageStoreError(
      'local-storage',
      'Catalog cache limits must be positive, nested, and no wider than the frozen defaults',
    )
  }
  return Object.freeze({ ...input })
}

export async function bindCatalogBudgetPolicy(
  database: IDBDatabase,
  limits: V2CatalogCacheBudgetLimits,
): Promise<void> {
  const transaction = database.transaction(CATALOG_BUDGET_STORE, 'readwrite')
  const budgets = transaction.objectStore(CATALOG_BUDGET_STORE)
  const key: ['policy'] = ['policy']
  const existing = await requestResult<StoredBudget | undefined>(budgets.get(key))
  if (existing === undefined) {
    const policy: StoredBudgetPolicy = { budgetKey: key, kind: 'policy', limits }
    budgets.add(policy)
  } else if (
    existing.kind !== 'policy' ||
    existing.limits.entriesPerShare !== limits.entriesPerShare ||
    existing.limits.entriesPerProfile !== limits.entriesPerProfile ||
    existing.limits.metadataBytesPerShare !== limits.metadataBytesPerShare ||
    existing.limits.metadataBytesPerProfile !== limits.metadataBytesPerProfile
  ) {
    transaction.abort()
    throw new V2CatalogPageStoreError(
      'local-storage',
      'Catalog cache database is already bound to another budget policy',
    )
  }
  await waitForIndexedDbTransaction(transaction)
}

export async function touchCatalogShareBudget(
  database: IDBDatabase,
  shareInstanceId: string,
): Promise<void> {
  const transaction = database.transaction(CATALOG_BUDGET_STORE, 'readwrite')
  const budgets = transaction.objectStore(CATALOG_BUDGET_STORE)
  const record = await requestResult<StoredBudget | undefined>(budgets.get(['share', shareInstanceId]))
  if (record !== undefined) {
    const share = readShareBudget(record, shareInstanceId)
    budgets.put(shareBudget(
      shareInstanceId,
      share.entries,
      share.metadataBytes,
      Date.now(),
    ))
  }
  await waitForIndexedDbTransaction(transaction)
}

export async function reserveCatalogPageBudget(
  transaction: IDBTransaction,
  shareInstanceId: string,
  limits: V2CatalogCacheBudgetLimits,
  page: V2CatalogPage,
): Promise<CatalogPageBudgetCharge> {
  const charge: CatalogPageBudgetCharge = {
    entryCharge: page.entries.length,
    metadataChargeBytes: catalogPageMetadataCharge(page),
  }
  const budgets = transaction.objectStore(CATALOG_BUDGET_STORE)
  const [profileRecord, shareRecord] = await Promise.all([
    requestResult<StoredBudget | undefined>(budgets.get(profileBudgetKey())),
    requestResult<StoredBudget | undefined>(budgets.get(['share', shareInstanceId])),
  ])
  const profile = readProfileBudget(profileRecord)
  const share = readShareBudget(shareRecord, shareInstanceId)
  const projectedShareEntries = share.entries + charge.entryCharge
  const projectedProfileEntries = profile.entries + charge.entryCharge
  const projectedShareBytes = share.metadataBytes + charge.metadataChargeBytes
  const projectedProfileBytes = profile.metadataBytes + charge.metadataChargeBytes
  if (
    projectedShareEntries > limits.entriesPerShare ||
    projectedShareBytes > limits.metadataBytesPerShare
  ) throw new V2CatalogPageStoreError(
    'resource-limit',
    'Catalog cache exceeded its share entry or metadata budget',
    { resourceScope: 'share' },
  )
  if (
    projectedProfileEntries > limits.entriesPerProfile ||
    projectedProfileBytes > limits.metadataBytesPerProfile
  ) throw new V2CatalogPageStoreError(
    'resource-limit',
    'Catalog cache exceeded its browser-profile entry or metadata budget',
    { resourceScope: 'profile' },
  )
  budgets.put(profileBudget(projectedProfileEntries, projectedProfileBytes))
  budgets.put(shareBudget(
    shareInstanceId,
    projectedShareEntries,
    projectedShareBytes,
    Date.now(),
  ))
  return charge
}

export async function releaseCatalogDirectory(
  transaction: IDBTransaction,
  directoryKey: CatalogDirectoryOwnerKey,
): Promise<void> {
  const [shareInstanceId] = directoryKey
  let entries = 0
  let metadataBytes = 0
  await visitIndexRecords(
    transaction.objectStore(CATALOG_PAGE_STORE).index(CATALOG_DIRECTORY_OWNER_INDEX),
    directoryKey,
    (cursor) => {
      const page = cursor.value as CatalogBudgetedPageRecord
      requireBudgetCounter(page.entryCharge, page.metadataChargeBytes)
      entries += page.entryCharge
      metadataBytes += page.metadataChargeBytes
      cursor.delete()
    },
  )
  if (entries === 0 && metadataBytes === 0) return
  const budgets = transaction.objectStore(CATALOG_BUDGET_STORE)
  const [profileRecord, shareRecord] = await Promise.all([
    requestResult<StoredBudget | undefined>(budgets.get(profileBudgetKey())),
    requestResult<StoredBudget | undefined>(budgets.get(['share', shareInstanceId])),
  ])
  const profile = readProfileBudget(profileRecord)
  const share = readShareBudget(shareRecord, shareInstanceId)
  if (
    entries > profile.entries || entries > share.entries ||
    metadataBytes > profile.metadataBytes || metadataBytes > share.metadataBytes
  ) throw corruptedBudget()
  budgets.put(profileBudget(profile.entries - entries, profile.metadataBytes - metadataBytes))
  const remainingEntries = share.entries - entries
  const remainingBytes = share.metadataBytes - metadataBytes
  if (remainingEntries === 0 && remainingBytes === 0) {
    budgets.delete(['share', shareInstanceId])
  } else {
    budgets.put(shareBudget(shareInstanceId, remainingEntries, remainingBytes, Date.now()))
  }
}

export async function evictCatalogShareRecords(
  database: IDBDatabase,
  shareInstanceId: string,
): Promise<void> {
  const transaction = database.transaction(
    [CATALOG_DIRECTORY_STORE, CATALOG_PAGE_STORE, CATALOG_BUDGET_STORE],
    'readwrite',
  )
  const budgets = transaction.objectStore(CATALOG_BUDGET_STORE)
  const [profileRecord, shareRecord] = await Promise.all([
    requestResult<StoredBudget | undefined>(budgets.get(profileBudgetKey())),
    requestResult<StoredBudget | undefined>(budgets.get(['share', shareInstanceId])),
  ])
  const profile = readProfileBudget(profileRecord)
  const share = readShareBudget(shareRecord, shareInstanceId)
  let observedEntries = 0
  let observedBytes = 0
  await visitIndexRecords(
    transaction.objectStore(CATALOG_PAGE_STORE).index(CATALOG_SHARE_OWNER_INDEX),
    shareInstanceId,
    (cursor) => {
      const page = cursor.value as CatalogBudgetedPageRecord
      requireBudgetCounter(page.entryCharge, page.metadataChargeBytes)
      observedEntries += page.entryCharge
      observedBytes += page.metadataChargeBytes
      cursor.delete()
    },
  )
  if (observedEntries !== share.entries || observedBytes !== share.metadataBytes) {
    transaction.abort()
    throw corruptedBudget()
  }
  if (share.entries > profile.entries || share.metadataBytes > profile.metadataBytes) {
    transaction.abort()
    throw corruptedBudget()
  }
  await visitIndexRecords(
    transaction.objectStore(CATALOG_DIRECTORY_STORE).index(CATALOG_SHARE_OWNER_INDEX),
    shareInstanceId,
    (cursor) => { cursor.delete() },
  )
  budgets.delete(['share', shareInstanceId])
  budgets.put(profileBudget(
    profile.entries - share.entries,
    profile.metadataBytes - share.metadataBytes,
  ))
  await waitForIndexedDbTransaction(transaction)
}

export async function catalogShareEvictionCandidates(
  database: IDBDatabase,
  currentShare: string,
): Promise<readonly string[]> {
  const transaction = database.transaction(CATALOG_BUDGET_STORE, 'readonly')
  const records = await requestResult<StoredBudget[]>(
    transaction.objectStore(CATALOG_BUDGET_STORE).getAll(),
  )
  await waitForIndexedDbTransaction(transaction)
  return records
    .filter((record): record is StoredShareBudget =>
      record.kind === 'share' && record.shareOwnerKey !== currentShare)
    .sort((left, right) => left.lastAccessMilliseconds - right.lastAccessMilliseconds)
    .map((record) => record.shareOwnerKey)
}

export async function acquireCatalogShareActivity(
  databaseName: string,
  shareInstanceId: string,
): Promise<CatalogShareActivityLock> {
  if (typeof navigator === 'undefined' || navigator.locks === undefined) {
    throw new V2CatalogPageStoreError(
      'local-storage',
      'Cross-context catalog cache locking is unavailable',
    )
  }
  let release!: () => void
  let acquiredResolve!: () => void
  let acquiredReject!: (error: unknown) => void
  const held = new Promise<void>((resolve) => { release = once(resolve) })
  const acquired = new Promise<void>((resolve, reject) => {
    acquiredResolve = resolve
    acquiredReject = reject
  })
  const settled = navigator.locks.request(
    catalogShareActivityIdentity(databaseName, shareInstanceId),
    { mode: 'shared' },
    async () => {
      acquiredResolve()
      await held
    },
  )
  settled.catch(acquiredReject)
  await acquired
  return { release, settled }
}

export async function withExclusiveCatalogShareActivity(
  databaseName: string,
  shareInstanceId: string,
  operation: () => Promise<void>,
): Promise<boolean> {
  if (typeof navigator === 'undefined' || navigator.locks === undefined) {
    throw new V2CatalogPageStoreError('local-storage', 'Cross-context catalog eviction is unavailable')
  }
  let acquired = false
  await navigator.locks.request(
    catalogShareActivityIdentity(databaseName, shareInstanceId),
    { mode: 'exclusive', ifAvailable: true },
    async (lock) => {
      if (lock === null) return
      acquired = true
      await operation()
    },
  )
  return acquired
}

function profileBudgetKey(): ['profile'] {
  return ['profile']
}

function profileBudget(entries: number, metadataBytes: number): StoredProfileBudget {
  requireBudgetCounter(entries, metadataBytes)
  return { budgetKey: profileBudgetKey(), kind: 'profile', entries, metadataBytes }
}

function shareBudget(
  shareInstanceId: string,
  entries: number,
  metadataBytes: number,
  lastAccessMilliseconds: number,
): StoredShareBudget {
  requireBudgetCounter(entries, metadataBytes)
  if (!Number.isFinite(lastAccessMilliseconds)) throw corruptedBudget()
  return {
    budgetKey: ['share', shareInstanceId],
    kind: 'share',
    shareOwnerKey: shareInstanceId,
    entries,
    metadataBytes,
    lastAccessMilliseconds,
  }
}

function readProfileBudget(record: StoredBudget | undefined): StoredProfileBudget {
  if (record === undefined) return profileBudget(0, 0)
  if (record.kind !== 'profile') throw corruptedBudget()
  requireBudgetCounter(record.entries, record.metadataBytes)
  return record
}

function readShareBudget(
  record: StoredBudget | undefined,
  shareInstanceId: string,
): StoredShareBudget {
  if (record === undefined) return shareBudget(shareInstanceId, 0, 0, 0)
  if (record.kind !== 'share' || record.shareOwnerKey !== shareInstanceId) throw corruptedBudget()
  requireBudgetCounter(record.entries, record.metadataBytes)
  if (!Number.isFinite(record.lastAccessMilliseconds)) throw corruptedBudget()
  return record
}

function requireBudgetCounter(entries: number, metadataBytes: number): void {
  if (
    !Number.isSafeInteger(entries) || entries < 0 ||
    !Number.isSafeInteger(metadataBytes) || metadataBytes < 0
  ) throw corruptedBudget()
}

function corruptedBudget(): V2CatalogPageStoreError {
  return new V2CatalogPageStoreError('local-storage', 'Catalog cache budget ledger is malformed')
}

function catalogShareActivityIdentity(databaseName: string, shareInstanceId: string): string {
  return JSON.stringify(['windshare:v2:catalog-cache', databaseName, shareInstanceId])
}

function visitIndexRecords(
  index: IDBIndex,
  key: IDBValidKey,
  visit: (cursor: IDBCursorWithValue) => void,
): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    let settled = false
    const fail = (error: unknown) => {
      if (settled) return
      settled = true
      try {
        index.objectStore.transaction.abort()
      } catch {
        // The transaction may already be aborting because the cursor failed.
      }
      reject(error)
    }
    const request = index.openCursor(IDBKeyRange.only(key))
    request.addEventListener('error', () => fail(request.error), { once: true })
    request.addEventListener('success', () => {
      if (settled) return
      try {
        const cursor = request.result
        if (cursor === null) {
          settled = true
          resolve()
          return
        }
        visit(cursor)
        cursor.continue()
      } catch (error) {
        fail(error)
      }
    })
  })
}

function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function once(action: () => void): () => void {
  let called = false
  return () => {
    if (called) return
    called = true
    action()
  }
}
