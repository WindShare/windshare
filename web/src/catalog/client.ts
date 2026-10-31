import { captureFailureEvidence } from '../diagnostics/failure/evidence'
import { catalogFailure } from './failure-evidence'
import { encodeBase64Url, equalBytes } from '../crypto/bytes'
import { SenderObjectError } from '../crypto/sender-object'
import { CborError } from '../protocol/cbor'
import type {
  CatalogEntry,
  CatalogPage,
  CatalogPageRequest,
  DirectoryFailure,
  ShareDescriptor,
} from './records'
import {
  openCatalogObject,
  CATALOG_DIRECTORY_ENTRIES,
  CATALOG_PAGE_ENTRIES,
  CATALOG_PAGE_OBJECT_BYTES,
} from './records'
import {
  CatalogPageStoreError,
  type CachedDirectoryFailure,
  type CatalogPageStore,
  type CommittedDirectory,
} from './page-store'

export const MAXIMUM_DIRECTORY_PAGES = Math.ceil(
  CATALOG_DIRECTORY_ENTRIES / CATALOG_PAGE_ENTRIES,
)
export const MAXIMUM_CATALOG_SPOOL_BYTES = 256 * 1024 * 1024
export const MAXIMUM_CONCURRENT_DIRECTORY_LOADS = 4

export interface CatalogScanProgress {
  readonly directoryId: Uint8Array<ArrayBuffer>
  readonly attemptId: Uint8Array<ArrayBuffer>
  readonly discoveredEntries: bigint
}

export type CatalogScanProgressListener = (progress: CatalogScanProgress) => void

export interface CatalogOperationClient {
  fetchPage(
    request: CatalogPageRequest,
    signal: AbortSignal,
    onProgress?: CatalogScanProgressListener,
  ): Promise<Uint8Array>
  failProtocol?(reason: unknown): Promise<void>
}

export interface CatalogClientOptions {
  readonly descriptor: ShareDescriptor
  readonly readSecret: Uint8Array
  readonly operations: CatalogOperationClient
  readonly store: CatalogPageStore
  readonly storageIdentity?: string
  readonly now?: () => number
  readonly maximumConcurrentLoads?: number
}

interface DirectoryLoadCall {
  readonly controller: AbortController
  readonly promise: Promise<CommittedDirectory>
  waiters: number
}

export class DirectoryFailureError extends Error {
  readonly failure: DirectoryFailure

  constructor(failure: DirectoryFailure) {
    super(`Directory scan failed with authenticated code 0x${failure.code.toString(16)}`)
    this.name = 'DirectoryFailureError'
    this.failure = failure
  }
}

export class CatalogClientError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'CatalogClientError'
  }
}

/**
 * A committed directory is spooled page-by-page. Callers receive only a compact
 * handle, so a million-child directory cannot become one browser array.
 */
export class CatalogClient {
  readonly #descriptor: ShareDescriptor
  readonly #readSecret: Uint8Array<ArrayBuffer>
  readonly #operations: CatalogOperationClient
  readonly #store: CatalogPageStore
  readonly #storageIdentity: string
  readonly #now: () => number
  readonly #maximumConcurrentLoads: number
  readonly #inflight = new Map<string, DirectoryLoadCall>()
  readonly #progressListeners = new Set<CatalogScanProgressListener>()
  #closed = false

  constructor(options: CatalogClientOptions) {
    const maximum = options.maximumConcurrentLoads ?? MAXIMUM_CONCURRENT_DIRECTORY_LOADS
    if (!Number.isSafeInteger(maximum) || maximum <= 0 || maximum > MAXIMUM_CONCURRENT_DIRECTORY_LOADS) {
      throw new RangeError('Catalog concurrent-load limit exceeds the protocol-session budget')
    }
    this.#descriptor = options.descriptor
    this.#readSecret = options.readSecret.slice()
    this.#operations = options.operations
    this.#store = options.store
    this.#storageIdentity = options.storageIdentity ?? options.descriptor.shareInstanceId
    this.#now = options.now ?? (() => Date.now())
    this.#maximumConcurrentLoads = maximum
  }

  async loadDirectory(
    directoryId: Uint8Array,
    options: { readonly signal?: AbortSignal; readonly explicitRetry?: boolean } = {},
  ): Promise<CommittedDirectory> {
    this.#requireOpen()
    options.signal?.throwIfAborted()
    const directoryIdText = identityText(directoryId, 'directory ID')
    const committed = await this.#store.loadDirectory(directoryIdText)
    this.#requireOpen()
    options.signal?.throwIfAborted()
    if (committed !== undefined) return committed
    const explicitRetry = options.explicitRetry ?? false
    const cachedFailure = await this.#store.loadFailure(directoryIdText)
    this.#requireOpen()
    options.signal?.throwIfAborted()
    if (cachedFailure !== undefined) this.#admitRetry(cachedFailure, explicitRetry)

    let call = this.#inflight.get(directoryIdText)
    if (call === undefined) {
      if (this.#inflight.size >= this.#maximumConcurrentLoads) {
        throw new CatalogClientError('Catalog directory-load budget is exhausted')
      }
      const controller = new AbortController()
      const promise = this.#loadExclusive(
        directoryId.slice(),
        directoryIdText,
        explicitRetry,
        controller.signal,
      )
        .finally(() => this.#inflight.delete(directoryIdText))
      call = { controller, promise, waiters: 0 }
      this.#inflight.set(directoryIdText, call)
    }
    call.waiters += 1
    try {
      return await awaitWithAbort(call.promise, options.signal)
    } finally {
      call.waiters -= 1
      if (call.waiters === 0 && this.#inflight.get(directoryIdText) === call) {
        call.controller.abort(new DOMException('Last catalog waiter left', 'AbortError'))
      }
    }
  }

  async *pages(
    directory: CommittedDirectory,
    signal?: AbortSignal,
  ): AsyncGenerator<CatalogPage> {
    for (let pageIndex = 0; pageIndex < directory.pageCount; pageIndex += 1) {
      signal?.throwIfAborted()
      const page = await this.#store.loadPage(directory, pageIndex)
      if (page === undefined) {
        throw new CatalogClientError('Committed catalog directory is missing a page')
      }
      yield page
    }
  }

  subscribeScanProgress(listener: CatalogScanProgressListener): () => void {
    this.#requireOpen()
    this.#progressListeners.add(listener)
    return () => this.#progressListeners.delete(listener)
  }

  async page(
    directory: CommittedDirectory,
    pageIndex: number,
    signal?: AbortSignal,
  ): Promise<CatalogPage> {
    this.#requireOpen()
    signal?.throwIfAborted()
    if (!Number.isSafeInteger(pageIndex) || pageIndex < 0 || pageIndex >= directory.pageCount) {
      throw new RangeError('Catalog page index is outside the committed directory')
    }
    const page = await this.#store.loadPage(directory, pageIndex)
    if (page === undefined) {
      throw new CatalogClientError('Committed catalog directory is missing a page')
    }
    return page
  }

  async hasCommittedName(
    directory: CommittedDirectory,
    name: string,
    signal?: AbortSignal,
  ): Promise<boolean> {
    this.#requireOpen()
    signal?.throwIfAborted()
    const present = await this.#store.hasCommittedName(directory, name)
    this.#requireOpen()
    signal?.throwIfAborted()
    return present
  }

  async *entries(
    directory: CommittedDirectory,
    signal?: AbortSignal,
  ): AsyncGenerator<CatalogEntry> {
    for await (const page of this.pages(directory, signal)) {
      for (const entry of page.entries) {
        signal?.throwIfAborted()
        yield entry
      }
    }
  }

  async evictCache(): Promise<void> {
    this.#requireOpen()
    if (this.#inflight.size !== 0) {
      throw new CatalogClientError('Catalog cache cannot be evicted while a directory load is active')
    }
    const evict = this.#store.evictShare
    if (evict === undefined) {
      throw new CatalogClientError('Catalog page store does not support explicit cache eviction')
    }
    await evict.call(this.#store)
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#readSecret.fill(0)
    const calls = [...this.#inflight.values()]
    for (const call of calls) {
      call.controller.abort(new DOMException('Catalog client closed', 'AbortError'))
    }
    this.#inflight.clear()
    this.#progressListeners.clear()
    if (calls.length === 0) {
      this.#store.close()
      return
    }
    // An operation is allowed to ignore AbortSignal at the transport boundary.
    // Keep storage alive until its eventual completion observes cancellation and
    // removes staged authority; closing IndexedDB first would make cleanup fail.
    Promise.allSettled(calls.map((call) => call.promise))
      .then(() => this.#store.close())
      .catch(() => undefined)
  }

  async #loadRemote(
    directoryId: Uint8Array<ArrayBuffer>,
    directoryIdText: string,
    signal: AbortSignal,
  ): Promise<CommittedDirectory> {
    await this.#store.begin(directoryIdText)
    signal.throwIfAborted()
    let generation: Uint8Array<ArrayBuffer> | undefined
    let previousCommitment = new Uint8Array(32)
    let previousName: Uint8Array<ArrayBuffer> | undefined
    let entryCount = 0
    let spoolBytes = 0
    try {
      for (let pageIndex = 0; pageIndex < MAXIMUM_DIRECTORY_PAGES; pageIndex += 1) {
        signal.throwIfAborted()
        const request: CatalogPageRequest = {
          directoryId,
          ...(generation === undefined ? {} : { generation }),
          pageIndex,
        }
        const fetched = await this.#fetchOpenedPage(request, signal, spoolBytes)
        signal.throwIfAborted()
        spoolBytes = fetched.spoolBytes
        const opened = fetched.opened
        if (opened.kind === 'failure') {
          throw new DirectoryFailureError(opened.failure)
        }
        const page = opened.page
        generation ??= page.generation.slice()
        const validated = await this.#validatePage(
          page,
          generation,
          previousCommitment,
          previousName,
          entryCount,
        )
        signal.throwIfAborted()
        previousName = validated.previousName
        entryCount = validated.entryCount
        await this.#stageAuthenticatedPage(page, signal)
        previousCommitment = page.objectCommitment.slice()
        if (!page.terminal) continue
        const committed = Object.freeze({
          directoryIdText,
          generationText: page.generationText,
          directoryId: page.directoryId.slice(),
          generation: page.generation.slice(),
          pageCount: pageIndex + 1,
          entryCount,
          omittedCount: page.omittedCount,
          terminalCommitment: page.objectCommitment.slice(),
        })
        signal.throwIfAborted()
        await this.#store.commit(committed)
        // Commit publishes the directory to every receiver sharing this store.
        // A late cancellation may stop this caller, but must not revoke pages
        // another receiver can already have observed through the committed handle.
        return committed
      }
      return this.#failProtocol(new CatalogClientError(
        'Catalog directory has no terminal page within its limit',
      ))
    } catch (error) {
      this.#captureLoadFailure(error, signal, directoryIdText, generation, entryCount, spoolBytes)
      if (error instanceof DirectoryFailureError) {
        const cached = this.#cachedFailure(error.failure)
        await this.#store.storeFailure(cached).catch((persistenceError: unknown) => {
          throw new AggregateError(
            [error, persistenceError],
            'Authenticated directory failure could not be persisted',
            { cause: error },
          )
        })
      } else {
        await this.#store.abort(directoryIdText).catch((cleanupError: unknown) => {
          throw new AggregateError([error, cleanupError], 'Catalog failure and staging cleanup failed', { cause: error })
        })
      }
      throw error
    }
  }

  #captureLoadFailure(error: unknown, signal: AbortSignal, directoryId: string,
    generation: Uint8Array | undefined, entryCount: number, spoolBytes: number): void {
    if (error !== signal.reason) captureFailureEvidence(error, { operation: 'catalog.load', context: {
      directoryId, generation: generation === undefined ? null : encodeBase64Url(generation),
      entryCount, spoolBytes, canceled: signal.aborted,
    } })
  }

  async #fetchOpenedPage(
    request: CatalogPageRequest,
    signal: AbortSignal,
    previousSpoolBytes: number,
  ): Promise<{
    readonly opened: Awaited<ReturnType<typeof openCatalogObject>>
    readonly spoolBytes: number
  }> {
    const object = await this.#operations.fetchPage(
      request,
      signal,
      (progress) => this.#publishScanProgress(progress),
    )
    signal.throwIfAborted()
    const spoolBytes = previousSpoolBytes + object.byteLength
    if (
      object.byteLength === 0 ||
      object.byteLength > CATALOG_PAGE_OBJECT_BYTES ||
      spoolBytes > MAXIMUM_CATALOG_SPOOL_BYTES
    ) {
      return this.#failProtocol(catalogFailure(new CatalogClientError(
        'Catalog object exceeded its spool admission',
      ), { invariant: 'spool-budget', actualObjectBytes: object.byteLength, maximumObjectBytes: CATALOG_PAGE_OBJECT_BYTES,
        actualSpoolBytes: spoolBytes, maximumSpoolBytes: MAXIMUM_CATALOG_SPOOL_BYTES, directoryId: encodeBase64Url(request.directoryId), pageIndex: request.pageIndex }))
    }
    try {
      const opened = await openCatalogObject(object, this.#descriptor, this.#readSecret, request)
      signal.throwIfAborted()
      return {
        opened,
        spoolBytes,
      }
    } catch (error) {
      signal.throwIfAborted()
      if (error instanceof SenderObjectError || error instanceof CborError) {
        return this.#failProtocol(error)
      }
      throw error
    }
  }

  async #stageAuthenticatedPage(page: CatalogPage, signal: AbortSignal): Promise<void> {
    try {
      await this.#store.stage(page)
    } catch (error) {
      if (
        error instanceof CatalogPageStoreError &&
        error.kind === 'authenticated-ownership'
      ) await this.#failProtocol(error)
      throw error
    }
    signal.throwIfAborted()
  }

  async #validatePage(
    page: CatalogPage,
    generation: Uint8Array,
    previousCommitment: Uint8Array,
    previousName: Uint8Array<ArrayBuffer> | undefined,
    previousEntryCount: number,
  ): Promise<{
    readonly previousName: Uint8Array<ArrayBuffer> | undefined
    readonly entryCount: number
  }> {
    if (
      !equalBytes(page.generation, generation) ||
      !equalBytes(page.previousCommitment, previousCommitment)
    ) {
      return this.#failProtocol(catalogFailure(new CatalogClientError(
        'Catalog page chain has a gap or generation splice',
      ), { invariant: 'page-chain', directoryId: page.directoryIdText, pageIndex: page.pageIndex, expectedGeneration: encodeBase64Url(generation), actualGeneration: page.generationText, expectedCommitment: encodeBase64Url(previousCommitment), actualCommitment: encodeBase64Url(page.previousCommitment) }))
    }
    if (page.entries.some((entry) => entry.idText === this.#descriptor.syntheticRootId)) {
      // Every ordinary node is claimed by exactly one parent entry. The
      // synthetic root has no parent, so accepting it as a child would create
      // the only identity cycle that page-store ownership cannot arbitrate.
      return this.#failProtocol(catalogFailure(new CatalogClientError(
        'Catalog entry reuses the synthetic-root identity',
      ), { invariant: 'synthetic-root-child', directoryId: page.directoryIdText, pageIndex: page.pageIndex, rootId: this.#descriptor.syntheticRootId }))
    }
    let nextName: Uint8Array<ArrayBuffer> | undefined
    try {
      nextName = validatePageOrder(page.entries, previousName)
    } catch (error) {
      return this.#failProtocol(error)
    }
    const entryCount = previousEntryCount + page.entries.length
    if (entryCount > CATALOG_DIRECTORY_ENTRIES) {
      return this.#failProtocol(catalogFailure(new CatalogClientError(
        'Catalog directory exceeds its entry limit',
      ), { invariant: 'directory-entries', directoryId: page.directoryIdText, expectedMaximum: CATALOG_DIRECTORY_ENTRIES, actual: entryCount }))
    }
    return { previousName: nextName, entryCount }
  }

  async #loadExclusive(
    directoryId: Uint8Array<ArrayBuffer>,
    directoryIdText: string,
    explicitRetry: boolean,
    signal: AbortSignal,
  ): Promise<CommittedDirectory> {
    const lockIdentity = JSON.stringify([
      'windshare:v2:catalog',
      this.#storageIdentity,
      directoryIdText,
    ])
    return withCatalogLoadLock(lockIdentity, signal, async () => {
      this.#requireOpen()
      signal.throwIfAborted()
      // Another receiver may have committed while this tab waited for the
      // capability-scoped directory lock.
      const committed = await this.#store.loadDirectory(directoryIdText)
      signal.throwIfAborted()
      if (committed !== undefined) return committed
      const cachedFailure = await this.#store.loadFailure(directoryIdText)
      signal.throwIfAborted()
      if (cachedFailure !== undefined) this.#admitRetry(cachedFailure, explicitRetry)
      return this.#loadRemote(directoryId, directoryIdText, signal)
    })
  }

  #admitRetry(cached: CachedDirectoryFailure, explicitRetry: boolean): void {
    if (
      !cached.failure.retryable ||
      !explicitRetry ||
      cached.retryAtMilliseconds === null ||
      this.#now() < cached.retryAtMilliseconds
    ) {
      throw new DirectoryFailureError(cached.failure)
    }
  }

  #cachedFailure(failure: DirectoryFailure): CachedDirectoryFailure {
    const retryAtMilliseconds = failure.retryable
      ? this.#now() + (failure.retryAfterMilliseconds ?? 0)
      : null
    return Object.freeze({ failure, retryAtMilliseconds })
  }

  #publishScanProgress(progress: CatalogScanProgress): void {
    for (const listener of this.#progressListeners) {
      try {
        // Typed arrays remain mutable even inside a frozen object. Each listener
        // receives fresh identities so one UI consumer cannot corrupt another,
        // and listener failures never become authenticated protocol failures.
        listener(Object.freeze({
          directoryId: progress.directoryId.slice(),
          attemptId: progress.attemptId.slice(),
          discoveredEntries: progress.discoveredEntries,
        }))
      } catch {
        // Catalog ownership and operation cleanup are independent of observers.
      }
    }
  }

  async #failProtocol(reason: unknown): Promise<never> {
    await this.#operations.failProtocol?.(reason)
    throw new CatalogClientError('Authenticated catalog traffic violated its protocol', {
      cause: reason,
    })
  }

  #requireOpen(): void {
    if (this.#closed) throw new CatalogClientError('Catalog client is closed')
  }
}

function identityText(value: Uint8Array, label: string): string {
  if (value.byteLength !== 16 || !value.some((item) => item !== 0)) {
    throw new TypeError(`${label} must be a nonzero 16-byte identity`)
  }
  return encodeBase64Url(value)
}

function validatePageOrder(
  entries: readonly CatalogEntry[],
  previous: Uint8Array<ArrayBuffer> | undefined,
): Uint8Array<ArrayBuffer> | undefined {
  let last = previous
  for (const entry of entries) {
    const encoded = new TextEncoder().encode(entry.name)
    if (last !== undefined && compareBytes(last, encoded) >= 0) {
      throw catalogFailure(new CatalogClientError('Catalog entries are not in strict canonical-name order'), { invariant: 'canonical-order', previous: new TextDecoder().decode(last), actual: entry.name, nodeId: entry.idText })
    }
    last = encoded
  }
  return last
}

function compareBytes(left: Uint8Array, right: Uint8Array): number {
  const length = Math.min(left.byteLength, right.byteLength)
  for (let index = 0; index < length; index += 1) {
    const difference = (left[index] ?? 0) - (right[index] ?? 0)
    if (difference !== 0) return difference
  }
  return left.byteLength - right.byteLength
}

function awaitWithAbort<T>(promise: Promise<T>, signal: AbortSignal | undefined): Promise<T> {
  if (signal === undefined) return promise
  signal.throwIfAborted()
  return new Promise<T>((resolve, reject) => {
    const abort = () => reject(signal.reason ?? new DOMException('Operation aborted', 'AbortError'))
    signal.addEventListener('abort', abort, { once: true })
    promise.then(
      (value) => {
        signal.removeEventListener('abort', abort)
        resolve(value)
      },
      (error: unknown) => {
        signal.removeEventListener('abort', abort)
        reject(error)
      },
    )
  })
}

const PROCESS_CATALOG_LOAD_TAILS = new Map<string, Promise<void>>()

export async function withCatalogLoadLock<T>(
  identity: string,
  signal: AbortSignal,
  operation: () => Promise<T>,
): Promise<T> {
  signal.throwIfAborted()
  if (typeof navigator !== 'undefined' && navigator.locks !== undefined) {
    return navigator.locks.request(identity, { mode: 'exclusive', signal }, operation)
  }
  const previous = PROCESS_CATALOG_LOAD_TAILS.get(identity) ?? Promise.resolve()
  let release!: () => void
  const gate = new Promise<void>((resolve) => {
    release = resolve
  })
  const tail = previous.catch(() => undefined).then(() => gate)
  PROCESS_CATALOG_LOAD_TAILS.set(identity, tail)
  tail.then(() => {
    if (PROCESS_CATALOG_LOAD_TAILS.get(identity) === tail) {
      PROCESS_CATALOG_LOAD_TAILS.delete(identity)
    }
  }).catch(() => undefined)
  try {
    await awaitWithAbort(previous.catch(() => undefined), signal)
    signal.throwIfAborted()
    return await operation()
  } finally {
    release()
  }
}
