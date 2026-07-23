import type { V2CatalogPage, V2DirectoryFailure } from './v2-records'

export interface V2CommittedDirectory {
  readonly directoryIdText: string
  readonly generationText: string
  readonly pageCount: number
  readonly entryCount: number
  readonly omittedCount: bigint
  readonly terminalCommitment: Uint8Array<ArrayBuffer>
}

export interface V2CachedDirectoryFailure {
  readonly failure: V2DirectoryFailure
  /** Null denotes a permanent authenticated failure. */
  readonly retryAtMilliseconds: number | null
}

export interface V2CatalogPageStore {
  loadDirectory(directoryIdText: string): Promise<V2CommittedDirectory | undefined>
  loadFailure(directoryIdText: string): Promise<V2CachedDirectoryFailure | undefined>
  loadPage(directory: V2CommittedDirectory, pageIndex: number): Promise<V2CatalogPage | undefined>
  begin(directoryIdText: string): Promise<void>
  stage(page: V2CatalogPage): Promise<void>
  commit(directory: V2CommittedDirectory): Promise<void>
  storeFailure(cached: V2CachedDirectoryFailure): Promise<void>
  abort(directoryIdText: string): Promise<void>
  evictShare?(): Promise<void>
  evictOldestInactiveShare?(): Promise<boolean>
  close(): void
}

export type V2CatalogPageStoreErrorKind =
  | 'authenticated-ownership'
  | 'resource-limit'
  | 'local-storage'
export type V2CatalogPageStoreResourceScope = 'share' | 'profile'

export class V2CatalogPageStoreError extends Error {
  readonly kind: V2CatalogPageStoreErrorKind
  readonly resourceScope: V2CatalogPageStoreResourceScope | undefined

  constructor(
    kind: V2CatalogPageStoreErrorKind,
    message: string,
    options?: ErrorOptions & { readonly resourceScope?: V2CatalogPageStoreResourceScope },
  ) {
    super(message, options)
    this.name = 'V2CatalogPageStoreError'
    this.kind = kind
    this.resourceScope = options?.resourceScope
  }
}

export function snapshotDirectory(directory: V2CommittedDirectory): V2CommittedDirectory {
  return Object.freeze({
    directoryIdText: directory.directoryIdText,
    generationText: directory.generationText,
    pageCount: directory.pageCount,
    entryCount: directory.entryCount,
    omittedCount: directory.omittedCount,
    terminalCommitment: directory.terminalCommitment.slice(),
  })
}

export function snapshotCachedFailure(cached: V2CachedDirectoryFailure): V2CachedDirectoryFailure {
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

export function requireCachedFailure(cached: V2CachedDirectoryFailure): void {
  const retryAt = cached.retryAtMilliseconds
  if (
    cached.failure.directoryIdText.length === 0 ||
    (cached.failure.retryable && (retryAt === null || !Number.isFinite(retryAt))) ||
    (!cached.failure.retryable && retryAt !== null)
  ) throw new V2CatalogPageStoreError(
    'local-storage',
    'Cached directory failure has inconsistent retry authority',
  )
}
