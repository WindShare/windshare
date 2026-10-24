import {
  CATALOG_IDENTITY_BYTES,
  type CatalogPage,
  type DirectoryFailure,
} from './records'

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
  /** Answers from the exact authenticated generation only; staged or superseded state is unavailable. */
  hasCommittedName(directory: CommittedDirectory, name: string): Promise<boolean>
  begin(directoryIdText: string): Promise<void>
  stage(page: CatalogPage): Promise<void>
  commit(directory: CommittedDirectory): Promise<void>
  storeFailure(cached: CachedDirectoryFailure): Promise<void>
  abort(directoryIdText: string): Promise<void>
  evictShare?(): Promise<void>
  evictOldestInactiveShare?(): Promise<boolean>
  close(): void
}

export type CatalogPageStoreErrorKind =
  | 'authenticated-ownership'
  | 'resource-limit'
  | 'local-storage'
export type CatalogPageStoreResourceScope = 'share' | 'profile'

export class CatalogPageStoreError extends Error {
  readonly kind: CatalogPageStoreErrorKind
  readonly resourceScope: CatalogPageStoreResourceScope | undefined

  constructor(
    kind: CatalogPageStoreErrorKind,
    message: string,
    options?: ErrorOptions & { readonly resourceScope?: CatalogPageStoreResourceScope },
  ) {
    super(message, options)
    this.name = 'CatalogPageStoreError'
    this.kind = kind
    this.resourceScope = options?.resourceScope
  }
}

export function isSameCommittedGeneration(
  left: CommittedDirectory,
  right: CommittedDirectory,
): boolean {
  return left.directoryIdText === right.directoryIdText &&
    left.generationText === right.generationText &&
    equalBytes(left.directoryId, right.directoryId) &&
    equalBytes(left.generation, right.generation)
}

export function snapshotDirectory(directory: CommittedDirectory): CommittedDirectory {
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

function equalBytes(left: Uint8Array, right: Uint8Array): boolean {
  return left.byteLength === right.byteLength && left.every((byte, index) => byte === right[index])
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

export function snapshotCachedFailure(cached: CachedDirectoryFailure): CachedDirectoryFailure {
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

export function requireCachedFailure(cached: CachedDirectoryFailure): void {
  const retryAt = cached.retryAtMilliseconds
  if (
    cached.failure.directoryIdText.length === 0 ||
    (cached.failure.retryable && (retryAt === null || !Number.isFinite(retryAt))) ||
    (!cached.failure.retryable && retryAt !== null)
  ) throw new CatalogPageStoreError(
    'local-storage',
    'Cached directory failure has inconsistent retry authority',
  )
}
