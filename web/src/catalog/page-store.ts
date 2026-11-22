import { catalogNameCollisionKey } from './path-policy'
import {
  CATALOG_IDENTITY_BYTES,
  CATALOG_PAGE_ENTRIES,
  MAX_CATALOG_PAYLOAD_BYTES,
  type CatalogPage,
  type DirectoryFailure,
} from './records'

const CATALOG_PAGE_RECORD_BASE_BYTES = 16 * 1024
const CATALOG_ENTRY_RECORD_BYTES = 512
const CATALOG_AUTHENTICATED_BYTES_MULTIPLIER = 4
const MAX_SHARE_CATALOG_ENTRIES = 4_194_304
const MAX_SHARE_CATALOG_METADATA_BYTES = 2 * 1024 * 1024 * 1024

export interface CommittedDirectory {
  readonly directoryIdText: string
  readonly generationText: string
  /** Raw authenticated identities are the authority; text is only a display/key projection. */
  readonly directoryId: Uint8Array<ArrayBuffer>
  readonly generation: Uint8Array<ArrayBuffer>
  readonly pageCount: number
  readonly entryCount: number
  readonly omittedCount: bigint
  readonly terminalCommitment: Uint8Array<ArrayBuffer>
}

export interface CachedDirectoryFailure {
  readonly failure: DirectoryFailure
  /** Null denotes a permanent authenticated failure. */
  readonly retryAtMilliseconds: number | null
}

export interface CatalogPageStore {
  loadDirectory(directoryIdText: string): Promise<CommittedDirectory | undefined>
  loadFailure(directoryIdText: string): Promise<CachedDirectoryFailure | undefined>
  loadPage(directory: CommittedDirectory, pageIndex: number): Promise<CatalogPage | undefined>
  begin(directoryIdText: string): Promise<void>
  stage(page: CatalogPage): Promise<void>
  commit(directory: CommittedDirectory): Promise<void>
  storeFailure(cached: CachedDirectoryFailure): Promise<void>
  abort(directoryIdText: string): Promise<void>
  close(): void
}

export type CatalogPageStoreErrorKind =
  | 'authenticated-ownership'
  | 'resource-limit'
  | 'local-storage'

export class CatalogPageStoreError extends Error {
  readonly kind: CatalogPageStoreErrorKind

  constructor(
    kind: CatalogPageStoreErrorKind,
    message: string,
    options?: ErrorOptions,
  ) {
    super(message, options)
    this.name = 'CatalogPageStoreError'
    this.kind = kind
  }
}

export interface MemoryCatalogCacheBudgetLimits {
  readonly maxEntries: number
  readonly maxMetadataBytes: number
}

// Bounds in-tab heap usage per live share; committed directories stay intact
// for progressive discovery and replay until the share closes.
export const DEFAULT_MEMORY_CATALOG_CACHE_BUDGET_LIMITS: MemoryCatalogCacheBudgetLimits = Object.freeze({
  maxEntries: 524_288,
  maxMetadataBytes: 256 * 1024 * 1024,
})

interface DirectoryBudgetCharge {
  entries: number
  metadataBytes: number
}

export function catalogPageMetadataCharge(page: CatalogPage): number {
  if (
    !Number.isSafeInteger(page.encodedBytes) ||
    page.encodedBytes <= 0 ||
    page.encodedBytes > MAX_CATALOG_PAYLOAD_BYTES ||
    page.entries.length > CATALOG_PAGE_ENTRIES
  ) {
    throw new CatalogPageStoreError('local-storage', 'Catalog page has an invalid memory footprint')
  }
  // Account for the decoded page object alongside node/name ownership index entries.
  return CATALOG_PAGE_RECORD_BASE_BYTES +
    page.entries.length * CATALOG_ENTRY_RECORD_BYTES +
    page.encodedBytes * CATALOG_AUTHENTICATED_BYTES_MULTIPLIER
}

export class MemoryCatalogPageStore implements CatalogPageStore {
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

  close(): void {}
}

function validateMemoryBudgetLimits(
  limits: MemoryCatalogCacheBudgetLimits,
): MemoryCatalogCacheBudgetLimits {
  if (
    !Number.isSafeInteger(limits.maxEntries) ||
    limits.maxEntries <= 0 ||
    !Number.isSafeInteger(limits.maxMetadataBytes) ||
    limits.maxMetadataBytes <= 0 ||
    limits.maxEntries > MAX_SHARE_CATALOG_ENTRIES ||
    limits.maxMetadataBytes > MAX_SHARE_CATALOG_METADATA_BYTES
  ) {
    throw new CatalogPageStoreError(
      'local-storage',
      'Memory catalog cache limits must be positive and within the share ceiling',
    )
  }
  return Object.freeze({ ...limits })
}

function snapshotDirectory(directory: CommittedDirectory): CommittedDirectory {
  const directoryId = requireCatalogIdentity(directory.directoryId, 'directory ID')
  const generation = requireCatalogIdentity(directory.generation, 'directory generation')
  return Object.freeze({
    directoryIdText: directory.directoryIdText,
    generationText: directory.generationText,
    directoryId,
    generation,
    pageCount: directory.pageCount,
    entryCount: directory.entryCount,
    omittedCount: directory.omittedCount,
    terminalCommitment: directory.terminalCommitment.slice(),
  })
}

function requireCatalogIdentity(
  value: Uint8Array<ArrayBuffer>,
  label: string,
): Uint8Array<ArrayBuffer> {
  if (!(value instanceof Uint8Array) || value.byteLength !== CATALOG_IDENTITY_BYTES ||
      value.every((byte) => byte === 0)) {
    throw new CatalogPageStoreError(
      'authenticated-ownership',
      `${label} is not a non-zero ${CATALOG_IDENTITY_BYTES}-byte identity`,
    )
  }
  return value.slice()
}

function snapshotCachedFailure(cached: CachedDirectoryFailure): CachedDirectoryFailure {
  requireCachedFailure(cached)
  const failure = cached.failure
  return Object.freeze({
    failure: Object.freeze({
      shareInstance: failure.shareInstance.slice(),
      directoryId: failure.directoryId.slice(),
      directoryIdText: failure.directoryIdText,
      attemptId: failure.attemptId.slice(),
      attemptIdText: failure.attemptIdText,
      code: failure.code,
      kind: failure.kind,
      retryable: failure.retryable,
      ...(failure.retryAfterMilliseconds === undefined
        ? {}
        : { retryAfterMilliseconds: failure.retryAfterMilliseconds }),
    }),
    retryAtMilliseconds: cached.retryAtMilliseconds,
  })
}

function requireCachedFailure(cached: CachedDirectoryFailure): void {
  const retryAt = cached.retryAtMilliseconds
  if (
    cached.failure.directoryIdText.length === 0 ||
    (cached.failure.retryable && (retryAt === null || !Number.isFinite(retryAt))) ||
    (!cached.failure.retryable && retryAt !== null)
  ) {
    throw new CatalogPageStoreError(
      'local-storage',
      'Cached directory failure has inconsistent retry authority',
    )
  }
}

function ownershipFailure(message: string): CatalogPageStoreError {
  return new CatalogPageStoreError('authenticated-ownership', message)
}
