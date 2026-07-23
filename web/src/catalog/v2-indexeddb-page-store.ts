import { catalogNameCollisionKey } from './path-policy'
import {
  V2_PATH_POLICY,
  type V2CatalogPage,
} from './v2-records'
import {
  acquireCatalogShareActivity,
  bindCatalogBudgetPolicy,
  catalogShareEvictionCandidates,
  CATALOG_BUDGET_STORE,
  CATALOG_DIRECTORY_OWNER_INDEX,
  CATALOG_DIRECTORY_STORE,
  CATALOG_PAGE_STORE,
  CATALOG_SHARE_OWNER_INDEX,
  evictCatalogShareRecords,
  releaseCatalogDirectory,
  reserveCatalogPageBudget,
  touchCatalogShareBudget,
  validateCatalogBudgetLimits,
  V2_DEFAULT_CATALOG_CACHE_BUDGET_LIMITS,
  withExclusiveCatalogShareActivity,
  type CatalogBudgetedPageRecord,
  type CatalogDirectoryOwnerKey,
  type CatalogShareActivityLock,
  type V2CatalogCacheBudgetLimits,
} from './v2-cache-budget'
import {
  requireCachedFailure,
  snapshotCachedFailure,
  snapshotDirectory,
  V2CatalogPageStoreError,
  type V2CachedDirectoryFailure,
  type V2CatalogPageStore,
  type V2CommittedDirectory,
} from './v2-page-store-contracts'

const DATABASE_VERSION = 5
const NODE_OWNER_INDEX = 'by-node-owner'
const NAME_OWNER_INDEX = 'by-name-owner'

interface StoredCommittedDirectory extends V2CommittedDirectory {
  readonly ownerKey: CatalogDirectoryOwnerKey
  readonly shareOwnerKey: string
  readonly kind: 'committed'
  readonly pathPolicy: string
}

interface StoredDirectoryFailure extends V2CachedDirectoryFailure {
  readonly ownerKey: CatalogDirectoryOwnerKey
  readonly shareOwnerKey: string
  readonly kind: 'failure'
  readonly pathPolicy: string
}

type StoredDirectoryState = StoredCommittedDirectory | StoredDirectoryFailure

type PageOwnerKey = [
  shareInstanceId: string,
  directoryIdText: string,
  generationText: string,
  pageIndex: number,
]
type NodeOwnerKey = [shareInstanceId: string, nodeIdText: string]
type NameOwnerKey = [
  shareInstanceId: string,
  directoryIdText: string,
  portableNameKey: string,
]

interface StoredPage extends CatalogBudgetedPageRecord {
  readonly pageKey: PageOwnerKey
  readonly directoryOwnerKey: CatalogDirectoryOwnerKey
  readonly nodeOwnerKeys: readonly NodeOwnerKey[]
  readonly nameOwnerKeys: readonly NameOwnerKey[]
  readonly page: V2CatalogPage
}

export class IndexedDbV2CatalogPageStore implements V2CatalogPageStore {
  readonly #database: IDBDatabase
  readonly #shareInstanceId: string
  readonly #databaseName: string
  readonly #limits: V2CatalogCacheBudgetLimits
  #activity: CatalogShareActivityLock
  #lifecycle: 'open' | 'evicting' | 'closed' = 'open'
  #evictionPromise: Promise<void> | undefined

  private constructor(
    database: IDBDatabase,
    shareInstanceId: string,
    databaseName: string,
    limits: V2CatalogCacheBudgetLimits,
    activity: CatalogShareActivityLock,
  ) {
    this.#database = database
    this.#shareInstanceId = shareInstanceId
    this.#databaseName = databaseName
    this.#limits = limits
    this.#activity = activity
    database.addEventListener('versionchange', () => this.close())
  }

  static async open(
    shareInstanceId: string,
    databaseName = 'windshare-v2-catalog',
    limits: V2CatalogCacheBudgetLimits = V2_DEFAULT_CATALOG_CACHE_BUDGET_LIMITS,
  ): Promise<IndexedDbV2CatalogPageStore> {
    if (shareInstanceId.length === 0 || databaseName.length === 0) {
      throw new TypeError('Catalog page store requires share and database identities')
    }
    const validatedLimits = validateCatalogBudgetLimits(limits)
    const activity = await acquireCatalogShareActivity(databaseName, shareInstanceId)
    let database: IDBDatabase | undefined
    try {
      database = await openDatabase(databaseName)
      await bindCatalogBudgetPolicy(database, validatedLimits)
      await touchCatalogShareBudget(database, shareInstanceId)
      return new IndexedDbV2CatalogPageStore(
        database,
        shareInstanceId,
        databaseName,
        validatedLimits,
        activity,
      )
    } catch (error) {
      database?.close()
      activity.release()
      await activity.settled.catch(() => undefined)
      throw error
    }
  }

  async loadDirectory(directoryIdText: string): Promise<V2CommittedDirectory | undefined> {
    this.#requireOpen()
    const transaction = this.#database.transaction(CATALOG_DIRECTORY_STORE, 'readonly')
    const record = await requestResult<StoredDirectoryState | undefined>(
      transaction.objectStore(CATALOG_DIRECTORY_STORE).get(this.#directoryKey(directoryIdText)),
    )
    await transactionCompletion(transaction)
    return record === undefined || record.kind !== 'committed' || record.pathPolicy !== V2_PATH_POLICY
      ? undefined
      : snapshotDirectory(record)
  }

  async loadFailure(directoryIdText: string): Promise<V2CachedDirectoryFailure | undefined> {
    this.#requireOpen()
    const transaction = this.#database.transaction(CATALOG_DIRECTORY_STORE, 'readonly')
    const record = await requestResult<StoredDirectoryState | undefined>(
      transaction.objectStore(CATALOG_DIRECTORY_STORE).get(this.#directoryKey(directoryIdText)),
    )
    await transactionCompletion(transaction)
    return record?.kind === 'failure' && record.pathPolicy === V2_PATH_POLICY
      ? snapshotCachedFailure(record)
      : undefined
  }

  async loadPage(
    directory: V2CommittedDirectory,
    pageIndex: number,
  ): Promise<V2CatalogPage | undefined> {
    this.#requireOpen()
    const transaction = this.#database.transaction(CATALOG_PAGE_STORE, 'readonly')
    const record = await requestResult<StoredPage | undefined>(
      transaction.objectStore(CATALOG_PAGE_STORE).get(
        this.#pageKey(directory.directoryIdText, directory.generationText, pageIndex),
      ),
    )
    await transactionCompletion(transaction)
    return record?.page
  }

  async begin(directoryIdText: string): Promise<void> {
    this.#requireOpen()
    await this.abort(directoryIdText)
  }

  async stage(page: V2CatalogPage): Promise<void> {
    this.#requireOpen()
    const directoryOwnerKey = this.#directoryKey(page.directoryIdText)
    const nodeOwnerKeys: NodeOwnerKey[] = []
    const nameOwnerKeys: NameOwnerKey[] = []
    const pageNodes = new Set<string>()
    const pageNames = new Set<string>()
    // A multi-entry index de-duplicates equal keys inside one record, so the
    // page must reject its own collisions before IndexedDB arbitrates pages.
    for (const entry of page.entries) {
      if (pageNodes.has(entry.idText)) {
        throw ownershipFailure('Catalog page repeats a node identity')
      }
      const portableNameKey = catalogNameCollisionKey(entry.name)
      if (pageNames.has(portableNameKey)) {
        throw ownershipFailure('Catalog page repeats a portable sibling name')
      }
      pageNodes.add(entry.idText)
      pageNames.add(portableNameKey)
      nodeOwnerKeys.push([this.#shareInstanceId, entry.idText])
      nameOwnerKeys.push([this.#shareInstanceId, page.directoryIdText, portableNameKey])
    }

    for (let attempt = 0; attempt < 2; attempt += 1) {
      try {
        await this.#stageWithBudget(
          page,
          directoryOwnerKey,
          nodeOwnerKeys,
          nameOwnerKeys,
        )
        return
      } catch (error) {
        if (
          attempt === 0 &&
          error instanceof V2CatalogPageStoreError &&
          error.kind === 'resource-limit' &&
          error.resourceScope === 'profile' &&
          await this.evictOldestInactiveShare()
        ) continue
        throw error
      }
    }
  }

  async #stageWithBudget(
    page: V2CatalogPage,
    directoryOwnerKey: CatalogDirectoryOwnerKey,
    nodeOwnerKeys: readonly NodeOwnerKey[],
    nameOwnerKeys: readonly NameOwnerKey[],
  ): Promise<void> {
    const transaction = this.#database.transaction(
      [CATALOG_PAGE_STORE, CATALOG_BUDGET_STORE],
      'readwrite',
    )
    const completion = transactionCompletion(transaction)
    try {
      const charge = await reserveCatalogPageBudget(
        transaction,
        this.#shareInstanceId,
        this.#limits,
        page,
      )
      const record: StoredPage = {
        pageKey: this.#pageKey(page.directoryIdText, page.generationText, page.pageIndex),
        directoryOwnerKey,
        shareOwnerKey: this.#shareInstanceId,
        nodeOwnerKeys,
        nameOwnerKeys,
        ...charge,
        page,
      }
      // Ownership, payload, and budget are one transaction. A failed unique
      // claim therefore cannot publish content or consume durable authority.
      transaction.objectStore(CATALOG_PAGE_STORE).add(record)
      await completion
    } catch (error) {
      abortTransaction(transaction)
      await completion.catch(() => undefined)
      if (error instanceof V2CatalogPageStoreError) throw error
      const kind = isConstraintFailure(error) ? 'authenticated-ownership' : 'local-storage'
      throw new V2CatalogPageStoreError(
        kind,
        kind === 'authenticated-ownership'
          ? 'Catalog page violates node or portable sibling-name uniqueness'
          : 'Catalog page could not be persisted locally',
        { cause: error },
      )
    }
  }

  async commit(directory: V2CommittedDirectory): Promise<void> {
    this.#requireOpen()
    const transaction = this.#database.transaction(CATALOG_DIRECTORY_STORE, 'readwrite')
    const record: StoredCommittedDirectory = {
      ...snapshotDirectory(directory),
      ownerKey: this.#directoryKey(directory.directoryIdText),
      shareOwnerKey: this.#shareInstanceId,
      kind: 'committed',
      pathPolicy: V2_PATH_POLICY,
    }
    transaction.objectStore(CATALOG_DIRECTORY_STORE).put(record)
    await transactionCompletion(transaction)
  }

  async storeFailure(cached: V2CachedDirectoryFailure): Promise<void> {
    this.#requireOpen()
    requireCachedFailure(cached)
    const transaction = this.#database.transaction(
      [CATALOG_DIRECTORY_STORE, CATALOG_PAGE_STORE, CATALOG_BUDGET_STORE],
      'readwrite',
    )
    const completion = transactionCompletion(transaction)
    const record: StoredDirectoryFailure = {
      ...snapshotCachedFailure(cached),
      ownerKey: this.#directoryKey(cached.failure.directoryIdText),
      shareOwnerKey: this.#shareInstanceId,
      kind: 'failure',
      pathPolicy: V2_PATH_POLICY,
    }
    try {
      transaction.objectStore(CATALOG_DIRECTORY_STORE).put(record)
      await releaseCatalogDirectory(transaction, record.ownerKey)
      await completion
    } catch (error) {
      abortTransaction(transaction)
      await completion.catch(() => undefined)
      throw error
    }
  }

  async abort(directoryIdText: string): Promise<void> {
    this.#requireOpen()
    const transaction = this.#database.transaction(
      [CATALOG_DIRECTORY_STORE, CATALOG_PAGE_STORE, CATALOG_BUDGET_STORE],
      'readwrite',
    )
    const completion = transactionCompletion(transaction)
    const directoryKey = this.#directoryKey(directoryIdText)
    try {
      transaction.objectStore(CATALOG_DIRECTORY_STORE).delete(directoryKey)
      await releaseCatalogDirectory(transaction, directoryKey)
      await completion
    } catch (error) {
      abortTransaction(transaction)
      await completion.catch(() => undefined)
      throw error
    }
  }

  async evictShare(): Promise<void> {
    if (this.#evictionPromise !== undefined) return this.#evictionPromise
    this.#requireOpen()
    this.#lifecycle = 'evicting'
    const operation = this.#evictCurrentShare()
      .finally(() => { this.#evictionPromise = undefined })
    this.#evictionPromise = operation
    return operation
  }

  async #evictCurrentShare(): Promise<void> {
    let failure: unknown
    try {
      this.#activity.release()
      await this.#activity.settled
      const evicted = await withExclusiveCatalogShareActivity(
        this.#databaseName,
        this.#shareInstanceId,
        () => evictCatalogShareRecords(this.#database, this.#shareInstanceId),
      )
      if (!evicted) {
        throw new V2CatalogPageStoreError(
          'resource-limit',
          'Catalog cache is still active in another browser context',
          { resourceScope: 'share' },
        )
      }
    } catch (error) {
      failure = error
    }
    if (this.#lifecycle !== 'closed') {
      try {
        this.#activity = await acquireCatalogShareActivity(
          this.#databaseName,
          this.#shareInstanceId,
        )
        this.#lifecycle = 'open'
      } catch (reacquireError) {
        this.#lifecycle = 'closed'
        this.#database.close()
        failure = failure === undefined
          ? reacquireError
          : new AggregateError([failure, reacquireError], 'Catalog eviction and lock recovery failed')
      }
    }
    if (failure !== undefined) throw failure
  }

  async evictOldestInactiveShare(): Promise<boolean> {
    this.#requireOpen()
    const candidates = await catalogShareEvictionCandidates(
      this.#database,
      this.#shareInstanceId,
    )
    for (const shareInstanceId of candidates) {
      if (await withExclusiveCatalogShareActivity(
        this.#databaseName,
        shareInstanceId,
        () => evictCatalogShareRecords(this.#database, shareInstanceId),
      )) return true
    }
    return false
  }

  close(): void {
    if (this.#lifecycle === 'closed') return
    this.#lifecycle = 'closed'
    this.#activity.release()
    this.#database.close()
  }

  #requireOpen(): void {
    if (this.#lifecycle !== 'open') {
      throw new V2CatalogPageStoreError('local-storage', 'Catalog page store is not open')
    }
  }

  #directoryKey(directoryIdText: string): CatalogDirectoryOwnerKey {
    return [this.#shareInstanceId, directoryIdText]
  }

  #pageKey(
    directoryIdText: string,
    generationText: string,
    pageIndex: number,
  ): PageOwnerKey {
    return [this.#shareInstanceId, directoryIdText, generationText, pageIndex]
  }
}

async function openDatabase(name: string): Promise<IDBDatabase> {
  const request = indexedDB.open(name, DATABASE_VERSION)
  request.addEventListener('upgradeneeded', () => {
    const database = request.result
    // Catalog ownership is security authority. A version change resets the
    // private cache so mixed schemas can never weaken uniqueness guarantees.
    for (const storeName of Array.from(database.objectStoreNames)) {
      database.deleteObjectStore(storeName)
    }
    const directories = database.createObjectStore(
      CATALOG_DIRECTORY_STORE,
      { keyPath: 'ownerKey' },
    )
    directories.createIndex(CATALOG_SHARE_OWNER_INDEX, 'shareOwnerKey')
    const pages = database.createObjectStore(CATALOG_PAGE_STORE, { keyPath: 'pageKey' })
    pages.createIndex(CATALOG_DIRECTORY_OWNER_INDEX, 'directoryOwnerKey')
    pages.createIndex(CATALOG_SHARE_OWNER_INDEX, 'shareOwnerKey')
    pages.createIndex(NODE_OWNER_INDEX, 'nodeOwnerKeys', { multiEntry: true, unique: true })
    pages.createIndex(NAME_OWNER_INDEX, 'nameOwnerKeys', { multiEntry: true, unique: true })
    const budgets = database.createObjectStore(CATALOG_BUDGET_STORE, { keyPath: 'budgetKey' })
    budgets.createIndex(CATALOG_SHARE_OWNER_INDEX, 'shareOwnerKey')
  })
  return databaseOpenResult(request)
}

function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function databaseOpenResult(request: IDBOpenDBRequest): Promise<IDBDatabase> {
  return new Promise<IDBDatabase>((resolve, reject) => {
    let settled = false
    request.addEventListener('blocked', () => {
      if (settled) return
      settled = true
      reject(new V2CatalogPageStoreError(
        'local-storage',
        'Catalog database upgrade is blocked by another open receiver; close it and retry',
      ))
    })
    request.addEventListener('success', () => {
      if (settled) {
        // IndexedDB requests cannot be cancelled after `blocked`; close a later
        // success so a rejected open never leaks upgrade ownership.
        request.result.close()
        return
      }
      settled = true
      resolve(request.result)
    }, { once: true })
    request.addEventListener('error', () => {
      if (settled) return
      settled = true
      reject(request.error)
    }, { once: true })
  })
}

function transactionCompletion(transaction: IDBTransaction): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    transaction.addEventListener('complete', () => resolve(), { once: true })
    transaction.addEventListener('abort', () => reject(transaction.error), { once: true })
    transaction.addEventListener('error', () => reject(transaction.error), { once: true })
  })
}

function abortTransaction(transaction: IDBTransaction): void {
  try {
    transaction.abort()
  } catch {
    // Completion or an IndexedDB failure may already have made it inactive.
  }
}

function ownershipFailure(message: string): V2CatalogPageStoreError {
  return new V2CatalogPageStoreError('authenticated-ownership', message)
}

function isConstraintFailure(error: unknown): boolean {
  return typeof error === 'object' && error !== null &&
    'name' in error && error.name === 'ConstraintError'
}
