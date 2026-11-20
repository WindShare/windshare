import { catalogNameCollisionKey } from './path-policy'
import type { CatalogPage } from './records'
import {
  catalogPageMetadataCharge,
  DEFAULT_CATALOG_CACHE_BUDGET_LIMITS,
  validateCatalogBudgetLimits,
  type CatalogCacheBudgetLimits,
} from './cache-budget'
import { IndexedDbCatalogPageStore } from './indexeddb-page-store'
import {
  isSameCommittedGeneration,
  requireCachedFailure,
  snapshotCachedFailure,
  snapshotDirectory,
  CatalogPageStoreError,
  type CachedDirectoryFailure,
  type CatalogPageStore,
  type CatalogPageStoreScope,
  type CommittedDirectory,
} from './page-store-contracts'

export {
  CatalogPageStoreError,
  type CachedDirectoryFailure,
  type CatalogPageStore,
  type CatalogPageStoreErrorKind,
  type CatalogPageStoreResourceScope,
  type CatalogPageStoreScope,
  type CommittedDirectory,
} from './page-store-contracts'
export { IndexedDbCatalogPageStore } from './indexeddb-page-store'
export {
  catalogPageMetadataCharge,
  DEFAULT_CATALOG_CACHE_BUDGET_LIMITS,
  type CatalogCacheBudgetLimits,
} from './cache-budget'

const DEFAULT_CATALOG_DATABASE_NAME = 'windshare-v2-catalog'

export interface MemoryCatalogCacheBudgetLimits {
  readonly maxEntries: number
  readonly maxMetadataBytes: number
}

// Page-local fallback storage resides in the tab heap, so its default cap stays
// tighter than the disk-backed browser-profile cache to avoid tab OOM.
export const DEFAULT_MEMORY_CATALOG_CACHE_BUDGET_LIMITS: MemoryCatalogCacheBudgetLimits = Object.freeze({
  maxEntries: 524_288,
  maxMetadataBytes: 256 * 1024 * 1024,
})

export type CatalogPageStoreFallbackReason = 'storage-unavailable' | 'storage-denied'

export interface CatalogPageStoreFallback {
  readonly reason: CatalogPageStoreFallbackReason
  readonly failure?: unknown
}

export interface OpenCatalogPageStoreOptions {
  readonly databaseName?: string
  readonly limits?: CatalogCacheBudgetLimits
  readonly memoryLimits?: MemoryCatalogCacheBudgetLimits
  readonly openIndexedDb?: (
    shareInstanceId: string,
    databaseName: string,
    limits: CatalogCacheBudgetLimits,
  ) => Promise<CatalogPageStore>
}

export type OpenedCatalogPageStore =
  | Readonly<{
      store: CatalogPageStore
      mode: 'indexeddb'
      fallback?: undefined
    }>
  | Readonly<{
      store: CatalogPageStore
      mode: 'memory'
      fallback: CatalogPageStoreFallback
    }>

interface DirectoryBudgetCharge {
  entries: number
  metadataBytes: number
}

export class MemoryCatalogPageStore implements CatalogPageStore {
  readonly coordinationScope: CatalogPageStoreScope = 'process'
  readonly #limits: MemoryCatalogCacheBudgetLimits
  readonly #directories = new Map<string, CommittedDirectory>()
  readonly #failures = new Map<string, CachedDirectoryFailure>()
  readonly #pages = new Map<string, Map<string, Map<number, CatalogPage>>>()
  readonly #nodeOwners = new Map<string, string>()
  readonly #nameOwners = new Map<string, Map<string, string>>()
  readonly #directoryCharges = new Map<string, DirectoryBudgetCharge>()
  #entries = 0
  #metadataBytes = 0

  constructor(limits: MemoryCatalogCacheBudgetLimits = DEFAULT_MEMORY_CATALOG_CACHE_BUDGET_LIMITS) {
    this.#limits = validateMemoryBudgetLimits(limits)
  }

  async loadDirectory(directoryIdText: string): Promise<CommittedDirectory | undefined> {
    return this.#directories.get(directoryIdText)
  }

  async loadFailure(directoryIdText: string): Promise<CachedDirectoryFailure | undefined> {
    const cached = this.#failures.get(directoryIdText)
    return cached === undefined ? undefined : snapshotCachedFailure(cached)
  }

  async loadPage(
    directory: CommittedDirectory,
    pageIndex: number,
  ): Promise<CatalogPage | undefined> {
    return this.#pages.get(directory.directoryIdText)?.get(directory.generationText)?.get(pageIndex)
  }

  async hasCommittedName(directory: CommittedDirectory, name: string): Promise<boolean> {
    const committed = this.#directories.get(directory.directoryIdText)
    if (committed === undefined || !isSameCommittedGeneration(committed, directory)) {
      throw committedGenerationUnavailable()
    }
    return this.#nameOwners.get(directory.directoryIdText)?.get(catalogNameCollisionKey(name)) ===
      directory.generationText
  }

  async begin(directoryIdText: string): Promise<void> {
    await this.abort(directoryIdText)
  }

  async stage(page: CatalogPage): Promise<void> {
    const pendingNodes = new Set<string>()
    const pendingNames = new Set<string>()
    const ownedNames = this.#nameOwners.get(page.directoryIdText)
    if (this.#pages.get(page.directoryIdText)?.get(page.generationText)?.has(page.pageIndex)) {
      throw ownershipFailure('Catalog page is already staged')
    }
    for (const entry of page.entries) {
      const owner = this.#nodeOwners.get(entry.idText)
      if (owner !== undefined && owner !== page.directoryIdText) {
        throw ownershipFailure('Catalog node identity is already owned by another directory')
      }
      if (owner !== undefined || pendingNodes.has(entry.idText)) {
        throw ownershipFailure('Catalog directory repeats a node identity')
      }
      const nameKey = catalogNameCollisionKey(entry.name)
      if (ownedNames?.has(nameKey) === true || pendingNames.has(nameKey)) {
        throw ownershipFailure('Catalog directory repeats a portable sibling name')
      }
      pendingNodes.add(entry.idText)
      pendingNames.add(nameKey)
    }
    const entryCharge = page.entries.length
    const metadataChargeBytes = catalogPageMetadataCharge(page)
    if (
      this.#entries + entryCharge > this.#limits.maxEntries ||
      this.#metadataBytes + metadataChargeBytes > this.#limits.maxMetadataBytes
    ) {
      throw new CatalogPageStoreError(
        'resource-limit',
        'Catalog cache exceeded its share entry or metadata budget',
        { resourceScope: 'share' },
      )
    }
    for (const node of pendingNodes) this.#nodeOwners.set(node, page.directoryIdText)
    const names = ownedNames ?? new Map<string, string>()
    for (const name of pendingNames) names.set(name, page.generationText)
    this.#nameOwners.set(page.directoryIdText, names)
    const generations = this.#pages.get(page.directoryIdText) ?? new Map()
    const pages = generations.get(page.generationText) ?? new Map()
    pages.set(page.pageIndex, page)
    generations.set(page.generationText, pages)
    this.#pages.set(page.directoryIdText, generations)
    const charge = this.#directoryCharges.get(page.directoryIdText) ?? { entries: 0, metadataBytes: 0 }
    charge.entries += entryCharge
    charge.metadataBytes += metadataChargeBytes
    this.#directoryCharges.set(page.directoryIdText, charge)
    this.#entries += entryCharge
    this.#metadataBytes += metadataChargeBytes
  }

  async commit(directory: CommittedDirectory): Promise<void> {
    this.#failures.delete(directory.directoryIdText)
    this.#directories.set(directory.directoryIdText, snapshotDirectory(directory))
  }

  async storeFailure(cached: CachedDirectoryFailure): Promise<void> {
    requireCachedFailure(cached)
    await this.abort(cached.failure.directoryIdText)
    this.#failures.set(cached.failure.directoryIdText, snapshotCachedFailure(cached))
  }

  async abort(directoryIdText: string): Promise<void> {
    this.#directories.delete(directoryIdText)
    this.#failures.delete(directoryIdText)
    this.#pages.delete(directoryIdText)
    for (const [node, owner] of this.#nodeOwners) {
      if (owner === directoryIdText) this.#nodeOwners.delete(node)
    }
    this.#nameOwners.delete(directoryIdText)
    const charge = this.#directoryCharges.get(directoryIdText)
    if (charge !== undefined) {
      this.#entries = Math.max(0, this.#entries - charge.entries)
      this.#metadataBytes = Math.max(0, this.#metadataBytes - charge.metadataBytes)
      this.#directoryCharges.delete(directoryIdText)
    }
  }

  async evictShare(): Promise<void> {
    this.#directories.clear()
    this.#failures.clear()
    this.#pages.clear()
    this.#nodeOwners.clear()
    this.#nameOwners.clear()
    this.#directoryCharges.clear()
    this.#entries = 0
    this.#metadataBytes = 0
  }

  async evictOldestInactiveShare(): Promise<boolean> {
    return false
  }

  close(): void {}
}

/**
 * Opens durable cross-context catalog storage when browser storage policy permits it,
 * falling back to a bounded page-local store only when storage APIs are absent or
 * explicitly denied (`SecurityError`). Runtime database failures and upgrade blocks
 * still surface so corrupted or contested durable stores are never silently masked.
 */
export async function openCatalogPageStore(
  shareInstanceId: string,
  options: OpenCatalogPageStoreOptions = {},
): Promise<OpenedCatalogPageStore> {
  const databaseName = options.databaseName ?? DEFAULT_CATALOG_DATABASE_NAME
  if (shareInstanceId.length === 0 || databaseName.length === 0) {
    throw new TypeError('Catalog page store requires share and database identities')
  }
  const limits = validateCatalogBudgetLimits(options.limits ?? DEFAULT_CATALOG_CACHE_BUDGET_LIMITS)
  const memoryLimits = validateMemoryBudgetLimits(
    options.memoryLimits ?? DEFAULT_MEMORY_CATALOG_CACHE_BUDGET_LIMITS,
  )
  if (options.openIndexedDb === undefined) {
    const availability = checkBrowserCatalogStorageAvailability()
    if (availability.kind !== 'available') {
      return memoryFallbackResult(
        memoryLimits,
        availability.kind === 'denied' ? 'storage-denied' : 'storage-unavailable',
        availability.kind === 'denied' ? availability.failure : undefined,
      )
    }
  }
  try {
    const open = options.openIndexedDb ?? IndexedDbCatalogPageStore.open
    const store = await open(shareInstanceId, databaseName, limits)
    return Object.freeze({ store, mode: 'indexeddb' as const })
  } catch (failure) {
    if (isStoragePolicyDenial(failure)) {
      return memoryFallbackResult(memoryLimits, 'storage-denied', failure)
    }
    throw failure
  }
}

type BrowserCatalogStorageAvailability =
  | { readonly kind: 'available' }
  | { readonly kind: 'unavailable' }
  | { readonly kind: 'denied'; readonly failure: DOMException }

function checkBrowserCatalogStorageAvailability(): BrowserCatalogStorageAvailability {
  try {
    if (!('indexedDB' in globalThis)) return { kind: 'unavailable' }
    const idb = globalThis.indexedDB
    if (idb === undefined || idb === null || typeof idb.open !== 'function') {
      return { kind: 'unavailable' }
    }
    if (typeof navigator === 'undefined') return { kind: 'unavailable' }
    const locks = navigator.locks
    if (locks === undefined || locks === null || typeof locks.request !== 'function') {
      return { kind: 'unavailable' }
    }
    return { kind: 'available' }
  } catch (failure) {
    if (isStoragePolicyDenial(failure)) return { kind: 'denied', failure }
    throw failure
  }
}

function memoryFallbackResult(
  memoryLimits: MemoryCatalogCacheBudgetLimits,
  reason: CatalogPageStoreFallbackReason,
  failure?: unknown,
): OpenedCatalogPageStore {
  return Object.freeze({
    store: new MemoryCatalogPageStore(memoryLimits),
    mode: 'memory' as const,
    fallback: Object.freeze({
      reason,
      ...(failure === undefined ? {} : { failure }),
    }),
  })
}

function isStoragePolicyDenial(failure: unknown): failure is DOMException {
  return failure instanceof DOMException && failure.name === 'SecurityError'
}

function validateMemoryBudgetLimits(
  limits: MemoryCatalogCacheBudgetLimits,
): MemoryCatalogCacheBudgetLimits {
  if (
    !Number.isSafeInteger(limits.maxEntries) ||
    limits.maxEntries <= 0 ||
    !Number.isSafeInteger(limits.maxMetadataBytes) ||
    limits.maxMetadataBytes <= 0 ||
    limits.maxEntries > DEFAULT_CATALOG_CACHE_BUDGET_LIMITS.entriesPerShare ||
    limits.maxMetadataBytes > DEFAULT_CATALOG_CACHE_BUDGET_LIMITS.metadataBytesPerShare
  ) {
    throw new CatalogPageStoreError(
      'local-storage',
      'Memory catalog cache limits must be positive and within the share ceiling',
    )
  }
  return Object.freeze({ ...limits })
}

function committedGenerationUnavailable(): CatalogPageStoreError {
  return new CatalogPageStoreError(
    'local-storage',
    'Catalog directory generation is no longer committed',
  )
}

function ownershipFailure(message: string): CatalogPageStoreError {
  return new CatalogPageStoreError('authenticated-ownership', message)
}
