import { directoryId, fileId } from '../catalog/model'
import {
  snapshotPortableCatalogPath,
  V2_CATALOG_PATH_DEPTH,
} from '../catalog/path-policy'
import { V2CatalogClient, V2DirectoryFailureError } from '../catalog/v2-client'
import type { V2CommittedDirectory } from '../catalog/v2-page-store'
import type { V2CatalogEntry, V2CatalogModifiedTime, V2ShareDescriptor } from '../catalog/v2-records'
import { V2SelectionPolicy } from '../catalog/v2-selection'
import { ByteRangeSet, byteRange } from '../content/geometry'
import {
  V2BlockLaneAttemptsError,
  type V2BlockRangeReader,
  type V2ContentLaneStatus,
} from '../content/v2-broker'
import {
  V2BlockOperationError,
  V2RemoteOperationError,
  V2RemoteRevisionError,
  V2RevisionChangedDuringRecoveryError,
  V2RevisionLeaseExpiredError,
  type V2OpenedRevision,
  type V2RevisionReader,
} from '../content/v2-session-services'
import { BoundedTaskPool } from './bounded-task-pool'
import { SelectionMeasureTracker, type SelectionMeasure } from './measure'
import { bindOutputFileTransaction } from './output-file-transaction'
import type { JobOutcome } from './outcome'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  TransferFailureAccumulator,
  jobOutcome,
} from './outcome'
import {
  OutputSessionSuspendedError,
  type OutputFile,
  type OutputSession,
} from './output-session'

export const V2_MAXIMUM_CONCURRENT_FILES = 4

export class V2CatalogTraversalError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2CatalogTraversalError'
  }
}

/** Tracks only the active recursion path; sibling width must never become retained ownership. */
export class V2DirectoryAncestry {
  readonly #active = new Set<string>()
  #maximumDepth = 0

  get depth(): number {
    return this.#active.size
  }

  get maximumDepth(): number {
    return this.#maximumDepth
  }

  enter(directoryIdText: string): () => void {
    if (this.#active.has(directoryIdText)) {
      throw new V2CatalogTraversalError('Catalog traversal revisited an ancestor identity')
    }
    this.#active.add(directoryIdText)
    this.#maximumDepth = Math.max(this.#maximumDepth, this.#active.size)
    let active = true
    return () => {
      if (!active || !this.#active.delete(directoryIdText)) {
        throw new Error('Catalog traversal ancestry ownership was released twice')
      }
      active = false
    }
  }
}

export interface V2TransferProgress {
  readonly discoveredFiles: number
  readonly discoveredBytes: bigint
  readonly writtenBytes: bigint
  readonly completedFiles: number
  readonly contentLanes: number
  readonly discoveryComplete: boolean
}

export interface V2TransferJobOptions {
  readonly descriptor: V2ShareDescriptor
  readonly catalog: V2CatalogClient
  readonly selection: V2SelectionPolicy
  readonly revisions: V2RevisionReader
  readonly broker: V2BlockRangeReader
  readonly lanes: V2ContentLaneStatus
  readonly output: OutputSession
  readonly onProgress?: (progress: V2TransferProgress) => void
  readonly onMeasure?: (measure: SelectionMeasure) => void
  readonly maximumConcurrentFiles?: number
}

interface DirectoryCursor {
  readonly id: Uint8Array<ArrayBuffer>
  readonly idText: string
  readonly path: readonly string[]
  readonly ancestry: readonly string[]
  readonly modifiedTime?: V2CatalogModifiedTime
}

export interface V2TransferJobResult {
  readonly outcome: JobOutcome
  readonly measure: SelectionMeasure
}

/** Page cursors, rather than full directory arrays, bound memory during recursive discovery. */
export class V2TransferJob {
  readonly #options: V2TransferJobOptions
  readonly #lifetime = new AbortController()
  readonly #pool: BoundedTaskPool
  readonly #measure = new SelectionMeasureTracker()
  readonly #failures = new TransferFailureAccumulator()
  readonly #directoryAncestry = new V2DirectoryAncestry()
  #writtenBytes = 0n
  #completedFiles = 0
  #discoveryComplete = false
  #externalAbortCleanup: (() => void) | undefined
  #started = false

  constructor(options: V2TransferJobOptions) {
    const concurrency = options.maximumConcurrentFiles ?? V2_MAXIMUM_CONCURRENT_FILES
    if (!Number.isSafeInteger(concurrency) || concurrency <= 0 ||
        concurrency > V2_MAXIMUM_CONCURRENT_FILES) {
      throw new RangeError('v2 transfer file concurrency exceeds its output-safe limit')
    }
    this.#options = options
    this.#pool = new BoundedTaskPool(concurrency)
  }

  async run(signal?: AbortSignal): Promise<V2TransferJobResult> {
    if (this.#started) throw new Error('v2 transfer job can only run once')
    this.#started = true
    this.#observeAbort(signal)
    const root: DirectoryCursor = {
      id: this.#options.descriptor.syntheticRoot,
      idText: this.#options.descriptor.syntheticRootId,
      path: Object.freeze([]),
      ancestry: Object.freeze([this.#options.descriptor.syntheticRootId]),
    }
    try {
      await this.#discoverDirectory(root)
      this.#discoveryComplete = true
      const measure = this.#failures.hasDirectoryFailures
        ? this.#measure.fail()
        : this.#measure.complete()
      this.#options.onMeasure?.(measure)
      this.#emitProgress()
      await this.#pool.drain()
      this.#lifetime.signal.throwIfAborted()
      const outcome = this.#failures.failureCount === 0
        ? jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
        : jobOutcome('CompletedWithErrors', this.#failures.snapshot())
      await this.#options.output.finishJob(outcome, this.#lifetime.signal)
      return Object.freeze({ outcome, measure })
    } catch (error) {
      if (!this.#lifetime.signal.aborted) this.#lifetime.abort(error)
      await this.#pool.settle()
      if (this.#lifetime.signal.reason instanceof OutputSessionSuspendedError &&
          this.#options.output.suspendJob !== undefined) {
        await this.#options.output.suspendJob(error)
      } else {
        await this.#options.output.abortJob(error)
      }
      const measure = this.#measure.snapshot()
      return Object.freeze({
        outcome: jobOutcome('Aborted', this.#failures.snapshot()),
        measure: measure.discovery === 'open' ? this.#measure.fail() : measure,
      })
    } finally {
      this.#externalAbortCleanup?.()
    }
  }

  async #discoverDirectory(cursor: DirectoryCursor): Promise<void> {
    this.#lifetime.signal.throwIfAborted()
    if (cursor.path.length > V2_CATALOG_PATH_DEPTH) {
      throw new V2CatalogTraversalError('Catalog traversal exceeded the protocol path depth')
    }
    const leaveDirectory = this.#directoryAncestry.enter(cursor.idText)
    try {
      const directory = await this.#loadDirectory(cursor)
      if (directory === undefined) return
      // A failed authenticated scan must not leave a directory that falsely looks
      // complete; successful empty directories are materialized before traversal.
      if (cursor.path.length > 0) {
        await this.#options.output.ensureDirectory(this.#outputDirectory(cursor))
      }
      for await (const page of this.#options.catalog.pages(directory, this.#lifetime.signal)) {
        for (const entry of page.entries) {
          await this.#discoverEntry(cursor, entry)
        }
      }
      if (directory.omittedCount > 0n) {
        this.#recordDirectoryFailure(
          cursor.idText,
          new Error(`Directory omitted ${directory.omittedCount} entries`),
        )
      }
      await this.#pool.drain()
      if (cursor.path.length > 0) {
        await this.#options.output.finalizeDirectory(
          this.#outputDirectory(cursor),
          this.#lifetime.signal,
        )
      }
    } finally {
      leaveDirectory()
    }
  }

  async #loadDirectory(cursor: DirectoryCursor): Promise<V2CommittedDirectory | undefined> {
    try {
      return await this.#options.catalog.loadDirectory(cursor.id, {
        signal: this.#lifetime.signal,
      })
    } catch (error) {
      if (error instanceof V2DirectoryFailureError) {
        this.#recordDirectoryFailure(cursor.idText, error.failure)
        return undefined
      }
      if (error instanceof V2RemoteOperationError && error.scope === 'directory') {
        this.#recordDirectoryFailure(cursor.idText, error)
        return undefined
      }
      throw error
    }
  }

  async #discoverEntry(cursor: DirectoryCursor, entry: V2CatalogEntry): Promise<void> {
    this.#lifetime.signal.throwIfAborted()
    let path: readonly string[]
    try {
      path = snapshotPortableCatalogPath([...cursor.path, entry.name])
    } catch (cause) {
      throw new V2CatalogTraversalError('Catalog entry exceeded the protocol path policy', { cause })
    }
    if (entry.kind === 'file') {
      if (this.#options.selection.selected(entry, cursor.ancestry)) {
        await this.#scheduleFile(entry, path)
      }
      return
    }
    if (this.#options.selection.shouldDiscover(entry.idText, cursor.ancestry)) {
      await this.#discoverDirectory({
        id: entry.id,
        idText: entry.idText,
        path,
        ancestry: Object.freeze([...cursor.ancestry, entry.idText]),
        ...(entry.modifiedTime === undefined ? {} : { modifiedTime: entry.modifiedTime }),
      })
    }
  }

  #outputDirectory(cursor: DirectoryCursor) {
    return {
      path: cursor.path,
      ...(cursor.modifiedTime === undefined || !this.#options.output.capabilities.modificationTime
        ? {}
        : { modifiedTimeMilliseconds: cursor.modifiedTime.milliseconds }),
    }
  }

  #recordDirectoryFailure(id: string, reason: unknown): void {
    this.#failures.record(Object.freeze({
      kind: 'directory',
      directoryId: directoryId(id),
      reason,
    }))
  }

  async #scheduleFile(
    entry: Extract<V2CatalogEntry, { kind: 'file' }>,
    path: readonly string[],
  ): Promise<void> {
    await this.#pool.waitForCapacity()
    const measure = this.#measure.observeUniqueFile(entry.expectedSize)
    this.#options.onMeasure?.(measure)
    this.#emitProgress()
    this.#pool.run(async () => {
      try {
        await this.#transferFile(entry, path)
      } catch (error) {
        if (!this.#lifetime.signal.aborted) this.#lifetime.abort(error)
        throw error
      }
    })
  }

  async #transferFile(
    entry: Extract<V2CatalogEntry, { kind: 'file' }>,
    path: readonly string[],
  ): Promise<void> {
    let opened: V2OpenedRevision | undefined
    let transaction: ReturnType<typeof bindOutputFileTransaction>['transaction'] | undefined
    try {
      opened = await this.#options.revisions.open(entry.id, this.#lifetime.signal)
      if (opened.descriptor.exactSize !== entry.expectedSize) {
        throw new V2FileRevisionChangedError(
          'Opened revision size changed from its committed catalog entry',
        )
      }
      const outputFile: OutputFile = {
        source: {
          shareInstance: opened.descriptor.shareInstanceId,
          fileId: opened.descriptor.fileIdText,
          fileRevision: opened.descriptor.fileRevisionText,
        },
        path,
        exactSize: opened.descriptor.exactSize,
        ...(entry.modifiedTime === undefined || !this.#options.output.capabilities.modificationTime
          ? {}
          : { modifiedTimeMilliseconds: entry.modifiedTime.milliseconds }),
      }
      const bound = bindOutputFileTransaction(
        await this.#fileOutputOperation(
          'Unable to begin the output file transaction',
          () => this.#options.output.beginFile(outputFile),
        ),
        outputFile,
        this.#options.output.identity,
      )
      transaction = bound.transaction
      const wanted = new ByteRangeSet(outputFile.exactSize, [byteRange(0n, outputFile.exactSize)])
      const missing = bound.durableRanges.asRangeSet().missingFrom(wanted)
      for (const range of missing.ranges) {
        for await (const slice of this.#options.broker.readRange(
          opened.descriptor,
          opened.leaseId,
          range,
          { signal: this.#lifetime.signal, priority: 'download' },
        )) {
          await this.#fileOutputOperation(
            'Unable to write the output file range',
            () => transaction?.writeRange(slice.offset, slice.data) ?? Promise.resolve(),
          )
          await this.#fileOutputOperation(
            'Unable to checkpoint the output file',
            () => transaction?.checkpoint() ?? Promise.resolve(undefined),
          )
          this.#writtenBytes += BigInt(slice.data.byteLength)
          this.#emitProgress()
        }
      }
      await this.#fileOutputOperation(
        'Unable to commit the output file',
        () => transaction?.commit() ?? Promise.resolve(),
      )
      this.#completedFiles += 1
      this.#emitProgress()
    } catch (error) {
      const disposition = await transaction?.abort(error).catch(() => 'JobOutputCompromised' as const)
      if (disposition === 'JobOutputCompromised' ||
          !isV2FileScopedTransferFailure(error)) throw error
      this.#failures.record(Object.freeze({
        kind: 'file',
        fileId: fileId(entry.idText),
        reason: error,
      }))
    } finally {
      await opened?.release().catch((error: unknown) => {
        if (!this.#lifetime.signal.aborted) this.#lifetime.abort(error)
      })
    }
  }

  async #fileOutputOperation<T>(message: string, operation: () => Promise<T>): Promise<T> {
    try {
      return await operation()
    } catch (cause) {
      // Output failures are file-local only when they originate at the output
      // boundary. Protocol/authentication failures must retain their session scope.
      if (cause instanceof OutputSessionSuspendedError || this.#lifetime.signal.aborted) throw cause
      throw new V2FileOutputError(message, { cause })
    }
  }

  #observeAbort(signal?: AbortSignal): void {
    if (signal === undefined) return
    const abort = () => this.#lifetime.abort(
      signal.reason ?? new DOMException('Transfer aborted', 'AbortError'),
    )
    signal.addEventListener('abort', abort, { once: true })
    this.#externalAbortCleanup = () => signal.removeEventListener('abort', abort)
    if (signal.aborted) abort()
  }

  #emitProgress(): void {
    const measure = this.#measure.snapshot()
    this.#options.onProgress?.(Object.freeze({
      discoveredFiles: measure.discoveredFiles,
      discoveredBytes: measure.discoveredBytes,
      writtenBytes: this.#writtenBytes,
      completedFiles: this.#completedFiles,
      contentLanes: this.#options.lanes.size,
      discoveryComplete: this.#discoveryComplete,
    }))
  }
}

export function isV2FileScopedTransferFailure(error: unknown): boolean {
  if (error instanceof V2RemoteOperationError) {
    return error.scope === 'revision' || error.scope === 'block'
  }
  return error instanceof V2RemoteRevisionError ||
    error instanceof V2RevisionLeaseExpiredError ||
    error instanceof V2BlockOperationError ||
    error instanceof V2BlockLaneAttemptsError ||
    error instanceof V2RevisionChangedDuringRecoveryError ||
    error instanceof V2FileRevisionChangedError ||
    error instanceof V2FileOutputError
}

class V2FileRevisionChangedError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'V2FileRevisionChangedError'
  }
}

class V2FileOutputError extends Error {
  constructor(message: string, options: ErrorOptions) {
    super(message, options)
    this.name = 'V2FileOutputError'
  }
}
