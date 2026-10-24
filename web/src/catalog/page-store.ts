import { catalogNameCollisionKey } from './path-policy'
import type { CatalogPage } from './records'
import {
  isSameCommittedGeneration,
  requireCachedFailure,
  snapshotCachedFailure,
  snapshotDirectory,
  CatalogPageStoreError,
  type CachedDirectoryFailure,
  type CatalogPageStore,
  type CommittedDirectory,
} from './page-store-contracts'

export {
  CatalogPageStoreError,
  type CachedDirectoryFailure,
  type CatalogPageStore,
  type CatalogPageStoreErrorKind,
  type CatalogPageStoreResourceScope,
  type CommittedDirectory,
} from './page-store-contracts'
export { IndexedDbCatalogPageStore } from './indexeddb-page-store'
export {
  catalogPageMetadataCharge,
  DEFAULT_CATALOG_CACHE_BUDGET_LIMITS,
  type CatalogCacheBudgetLimits,
} from './cache-budget'

export class MemoryCatalogPageStore implements CatalogPageStore {
  readonly #directories = new Map<string, CommittedDirectory>()
  readonly #failures = new Map<string, CachedDirectoryFailure>()
  readonly #pages = new Map<string, Map<string, Map<number, CatalogPage>>>()
  readonly #nodeOwners = new Map<string, string>()
  readonly #nameOwners = new Map<string, Map<string, string>>()

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
    for (const node of pendingNodes) this.#nodeOwners.set(node, page.directoryIdText)
    const names = ownedNames ?? new Map<string, string>()
    for (const name of pendingNames) names.set(name, page.generationText)
    this.#nameOwners.set(page.directoryIdText, names)
    const generations = this.#pages.get(page.directoryIdText) ?? new Map()
    const pages = generations.get(page.generationText) ?? new Map()
    pages.set(page.pageIndex, page)
    generations.set(page.generationText, pages)
    this.#pages.set(page.directoryIdText, generations)
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
  }

  async evictShare(): Promise<void> {
    this.#directories.clear()
    this.#failures.clear()
    this.#pages.clear()
    this.#nodeOwners.clear()
    this.#nameOwners.clear()
  }

  async evictOldestInactiveShare(): Promise<boolean> {
    return false
  }

  close(): void {}
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
