import { catalogNameCollisionKey } from './path-policy'
import type { V2CatalogPage } from './v2-records'
import {
  requireCachedFailure,
  snapshotCachedFailure,
  snapshotDirectory,
  V2CatalogPageStoreError,
  type V2CachedDirectoryFailure,
  type V2CatalogPageStore,
  type V2CommittedDirectory,
} from './v2-page-store-contracts'

export {
  V2CatalogPageStoreError,
  type V2CachedDirectoryFailure,
  type V2CatalogPageStore,
  type V2CatalogPageStoreErrorKind,
  type V2CatalogPageStoreResourceScope,
  type V2CommittedDirectory,
} from './v2-page-store-contracts'
export { IndexedDbV2CatalogPageStore } from './v2-indexeddb-page-store'
export {
  catalogPageMetadataCharge,
  V2_DEFAULT_CATALOG_CACHE_BUDGET_LIMITS,
  type V2CatalogCacheBudgetLimits,
} from './v2-cache-budget'

export class MemoryV2CatalogPageStore implements V2CatalogPageStore {
  readonly #directories = new Map<string, V2CommittedDirectory>()
  readonly #failures = new Map<string, V2CachedDirectoryFailure>()
  readonly #pages = new Map<string, Map<string, Map<number, V2CatalogPage>>>()
  readonly #nodeOwners = new Map<string, string>()
  readonly #nameOwners = new Map<string, Set<string>>()

  async loadDirectory(directoryIdText: string): Promise<V2CommittedDirectory | undefined> {
    return this.#directories.get(directoryIdText)
  }

  async loadFailure(directoryIdText: string): Promise<V2CachedDirectoryFailure | undefined> {
    const cached = this.#failures.get(directoryIdText)
    return cached === undefined ? undefined : snapshotCachedFailure(cached)
  }

  async loadPage(
    directory: V2CommittedDirectory,
    pageIndex: number,
  ): Promise<V2CatalogPage | undefined> {
    return this.#pages.get(directory.directoryIdText)?.get(directory.generationText)?.get(pageIndex)
  }

  async begin(directoryIdText: string): Promise<void> {
    await this.abort(directoryIdText)
  }

  async stage(page: V2CatalogPage): Promise<void> {
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
    const names = ownedNames ?? new Set<string>()
    for (const name of pendingNames) names.add(name)
    this.#nameOwners.set(page.directoryIdText, names)
    const generations = this.#pages.get(page.directoryIdText) ?? new Map()
    const pages = generations.get(page.generationText) ?? new Map()
    pages.set(page.pageIndex, page)
    generations.set(page.generationText, pages)
    this.#pages.set(page.directoryIdText, generations)
  }

  async commit(directory: V2CommittedDirectory): Promise<void> {
    this.#failures.delete(directory.directoryIdText)
    this.#directories.set(directory.directoryIdText, snapshotDirectory(directory))
  }

  async storeFailure(cached: V2CachedDirectoryFailure): Promise<void> {
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

function ownershipFailure(message: string): V2CatalogPageStoreError {
  return new V2CatalogPageStoreError('authenticated-ownership', message)
}
