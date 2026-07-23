import type {
  CatalogDirectoryFailure,
  CatalogDirectoryGeneration,
  CatalogDirectoryNode,
  CatalogFileNode,
  CatalogChild,
  DirectoryId,
} from '../catalog/model'
import { ProgressiveCatalogTree } from '../catalog/tree'
import { BoundedTaskPool } from './bounded-task-pool'
import {
  type BeginOutputFileResult,
  type OutputDirectory,
  type OutputFile,
  type OutputFileTransaction,
  type OutputSession,
  type OutputSessionIdentity,
  type OutputSourceIdentity,
  type VerifiedDurableRanges,
  MAXIMUM_OPEN_OUTPUT_FILES,
  outputSessionIdentity,
  snapshotOutputDirectory,
  snapshotOutputFile,
} from './output-session'
import { bindOutputFileTransaction } from './output-file-transaction'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  TransferFailureAccumulator,
  jobOutcome,
  type JobOutcome,
} from './outcome'
import {
  SelectionMeasureTracker,
  type SelectionMeasure,
} from './measure'
import { SelectionRules } from './selection-rules'

export interface DirectoryDiscoverySource {
  listChildren(
    directory: CatalogDirectoryNode,
    signal: AbortSignal,
  ): Promise<DirectoryDiscoveryResult>
}

export type DirectoryDiscoveryResult =
  | {
    readonly status: 'ready'
    readonly generation: CatalogDirectoryGeneration
  }
  | {
    readonly status: 'failed'
    readonly failure: CatalogDirectoryFailure
  }

export interface PreparedFileTransfer {
  readonly source: OutputSourceIdentity
  readonly exactSize: bigint
  transfer(
    transaction: OutputFileTransaction,
    durableRanges: VerifiedDurableRanges,
    signal: AbortSignal,
  ): Promise<void>
  release(): Promise<void>
}

export interface FileTransferService {
  open(file: CatalogFileNode, signal: AbortSignal): Promise<PreparedFileTransfer>
}

export interface TransferJobOptions {
  readonly shareInstance: string
  readonly maximumConcurrentFiles: number
  readonly onMeasure?: (measure: SelectionMeasure) => void
}

export interface TransferJobResult {
  readonly outcome: JobOutcome
  readonly measure: SelectionMeasure
}

export const MAXIMUM_CONCURRENT_TRANSFER_FILES = MAXIMUM_OPEN_OUTPUT_FILES

type TransferJobState = 'idle' | 'running' | 'settled'

interface JobRunContext {
  readonly pool: BoundedTaskPool
  readonly directories: DirectoryId[]
  readonly materialized: OutputDirectory[]
  pendingFiles: Array<PendingFileBatch | undefined>
  pendingFileHead: number
}

interface PendingFileBatch {
  readonly children: readonly CatalogChild[]
  nextIndex: number
}

type FileAttempt =
  | { readonly status: 'succeeded' }
  | { readonly status: 'failed'; readonly reason: unknown }

/**
 * Discovers selected subtrees incrementally. File work begins as soon as its
 * containing generation commits and never waits for unrelated directory scans.
 */
export class TransferJob {
  readonly #tree: ProgressiveCatalogTree
  readonly #rules: SelectionRules
  readonly #directories: DirectoryDiscoverySource
  readonly #files: FileTransferService
  readonly #output: OutputSession
  readonly #outputIdentity: OutputSessionIdentity
  readonly #options: TransferJobOptions
  readonly #measure = new SelectionMeasureTracker()
  readonly #failures = new TransferFailureAccumulator()
  readonly #lifetime = new AbortController()

  #state: TransferJobState = 'idle'
  #externalAbortCleanup: (() => void) | undefined

  constructor(
    tree: ProgressiveCatalogTree,
    rules: SelectionRules,
    directories: DirectoryDiscoverySource,
    files: FileTransferService,
    output: OutputSession,
    options: TransferJobOptions,
  ) {
    if (typeof options.shareInstance !== 'string' || options.shareInstance.length === 0) {
      throw new TypeError('transfer job share instance must not be empty')
    }
    if (!Number.isSafeInteger(options.maximumConcurrentFiles) ||
        options.maximumConcurrentFiles <= 0 ||
        options.maximumConcurrentFiles > MAXIMUM_CONCURRENT_TRANSFER_FILES) {
      throw new RangeError(
        `maximum concurrent files must be between 1 and ${MAXIMUM_CONCURRENT_TRANSFER_FILES}`,
      )
    }
    this.#tree = tree
    this.#rules = rules
    this.#directories = directories
    this.#files = files
    this.#output = output
    this.#outputIdentity = outputSessionIdentity(output.identity)
    this.#options = Object.freeze({ ...options })
  }

  async run(signal?: AbortSignal): Promise<TransferJobResult> {
    if (this.#state !== 'idle') {
      throw new Error('transfer job can only be run once')
    }
    this.#state = 'running'
    this.#observeAbort(signal)
    const context: JobRunContext = {
      pool: new BoundedTaskPool(this.#options.maximumConcurrentFiles),
      directories: [this.#tree.rootId],
      materialized: [],
      pendingFiles: [],
      pendingFileHead: 0,
    }

    try {
      return await this.#runActive(context)
    } catch (error) {
      return await this.#abortRun(context.pool, error)
    } finally {
      this.#externalAbortCleanup?.()
      this.#externalAbortCleanup = undefined
      this.#state = 'settled'
    }
  }

  async #runActive(context: JobRunContext): Promise<TransferJobResult> {
    while (context.directories.length > 0) {
      this.#lifetime.signal.throwIfAborted()
      const directoryId = context.directories.pop()
      if (directoryId !== undefined) {
        await this.#processDirectory(context, directoryId)
        this.#scheduleAvailableFiles(context)
      }
    }

    this.#emitMeasure(this.#terminalMeasure())
    await this.#scheduleRemainingFiles(context)
    await context.pool.drain()
    this.#lifetime.signal.throwIfAborted()
    await this.#finalizeDirectories(context.materialized, this.#lifetime.signal)
    const outcome = this.#completedOutcome()
    await this.#output.finishJob(outcome, this.#lifetime.signal)
    return this.#settle(outcome)
  }

  async #processDirectory(
    context: JobRunContext,
    directoryId: DirectoryId,
  ): Promise<void> {
    if (directoryId !== this.#tree.rootId &&
        !this.#rules.shouldDiscoverDirectory(this.#tree, directoryId)) {
      return
    }
    const directory = this.#tree.requireDirectory(directoryId)
    const materialized = await this.#materializeDirectory(directory)
    if (materialized !== undefined) {
      context.materialized.push(materialized)
    }
    const generation = await this.#discoverGeneration(directory)
    if (generation !== undefined) {
      this.#enqueueChildren(context, generation)
    }
  }

  async #materializeDirectory(
    directory: CatalogDirectoryNode,
  ): Promise<OutputDirectory | undefined> {
    if (directory.id === this.#tree.rootId) {
      return undefined
    }
    const outputDirectory = snapshotOutputDirectory({
      path: this.#tree.outputPath(directory.id),
      ...(directory.modifiedTime === undefined ||
          !this.#output.capabilities.modificationTime
        ? {}
        : { modifiedTimeMilliseconds: directory.modifiedTime.milliseconds }),
    })
    await this.#output.ensureDirectory(outputDirectory)
    return outputDirectory
  }

  async #discoverGeneration(
    directory: CatalogDirectoryNode,
  ): Promise<CatalogDirectoryGeneration | undefined> {
    const result = await this.#loadDirectory(directory)
    this.#lifetime.signal.throwIfAborted()
    if (result.status === 'ready') {
      return result.generation
    }
    this.#failures.record(Object.freeze({
      kind: 'directory',
      directoryId: directory.id,
      reason: result.failure,
    }))
    return undefined
  }

  #enqueueChildren(
    context: JobRunContext,
    generation: CatalogDirectoryGeneration,
  ): void {
    let containsSelectedFile = false
    for (const child of generation.children) {
      if (child.kind === 'file' && this.#rules.selected(this.#tree, child.id)) {
        containsSelectedFile = true
        const file = this.#tree.requireFile(child.id)
        this.#emitMeasure(this.#measure.observeUniqueFile(file.expectedSize))
      }
    }
    if (containsSelectedFile) {
      context.pendingFiles.push({ children: generation.children, nextIndex: 0 })
    }
    for (let index = generation.children.length - 1; index >= 0; index -= 1) {
      const child = generation.children[index]
      if (child?.kind === 'directory' &&
          this.#rules.shouldDiscoverDirectory(this.#tree, child.id)) {
        context.directories.push(child.id)
      }
    }
  }

  #scheduleFile(pool: BoundedTaskPool, file: CatalogFileNode): void {
    pool.run(async () => {
      try {
        await this.#transferFile(file)
      } catch (error) {
        if (!this.#lifetime.signal.aborted) {
          this.#lifetime.abort(error)
        }
        throw error
      }
    })
  }

  #scheduleAvailableFiles(context: JobRunContext): void {
    while (context.pool.hasCapacity) {
      const file = this.#nextPendingFile(context)
      if (file === undefined) {
        return
      }
      this.#scheduleFile(context.pool, file)
    }
  }

  async #scheduleRemainingFiles(context: JobRunContext): Promise<void> {
    while (true) {
      this.#lifetime.signal.throwIfAborted()
      await context.pool.waitForCapacity()
      this.#lifetime.signal.throwIfAborted()
      const file = this.#nextPendingFile(context)
      if (file === undefined) {
        return
      }
      this.#scheduleFile(context.pool, file)
    }
  }

  #nextPendingFile(context: JobRunContext): CatalogFileNode | undefined {
    while (context.pendingFileHead < context.pendingFiles.length) {
      const batch = context.pendingFiles[context.pendingFileHead]
      if (batch === undefined || batch.nextIndex >= batch.children.length) {
        context.pendingFiles[context.pendingFileHead] = undefined
        context.pendingFileHead += 1
        this.#compactPendingFiles(context)
        continue
      }
      const child = batch.children[batch.nextIndex]
      batch.nextIndex += 1
      if (child?.kind === 'file' && this.#rules.selected(this.#tree, child.id)) {
        return this.#tree.requireFile(child.id)
      }
    }
    context.pendingFiles = []
    context.pendingFileHead = 0
    return undefined
  }

  #compactPendingFiles(context: JobRunContext): void {
    const pendingCount = context.pendingFiles.length - context.pendingFileHead
    if (context.pendingFileHead > pendingCount) {
      context.pendingFiles = context.pendingFiles.slice(context.pendingFileHead)
      context.pendingFileHead = 0
    }
  }

  #terminalMeasure(): SelectionMeasure {
    return this.#failures.hasDirectoryFailures
      ? this.#measure.fail()
      : this.#measure.complete()
  }

  async #finalizeDirectories(
    directories: OutputDirectory[],
    signal: AbortSignal,
  ): Promise<void> {
    directories.sort((left, right) => right.path.length - left.path.length)
    for (const directory of directories) {
      signal.throwIfAborted()
      await this.#output.finalizeDirectory(directory, signal)
    }
    signal.throwIfAborted()
  }

  #completedOutcome(): JobOutcome {
    return this.#failures.failureCount === 0
      ? jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
      : jobOutcome('CompletedWithErrors', this.#failures.snapshot())
  }

  async #abortRun(
    pool: BoundedTaskPool,
    error: unknown,
  ): Promise<TransferJobResult> {
    if (!this.#lifetime.signal.aborted) {
      this.#lifetime.abort(error)
    }
    await pool.settle()
    const outcome = jobOutcome('Aborted', this.#failures.snapshot())
    try {
      await this.#output.abortJob(error)
    } catch (cleanupError) {
      throw new AggregateError(
        [error, cleanupError],
        'Transfer job aborted and output cleanup also failed',
        { cause: cleanupError },
      )
    }
    return this.#settle(outcome)
  }

  async #loadDirectory(
    directory: CatalogDirectoryNode,
  ): Promise<DirectoryDiscoveryResult> {
    const state = this.#tree.directoryState(directory.id)
    if (state.status === 'ready') {
      return Object.freeze({ status: 'ready', generation: state.generation })
    }
    if (state.status === 'failed') {
      return Object.freeze({ status: 'failed', failure: state.failure })
    }
    const ownsLoadingState = this.#tree.beginDirectoryLoad(directory.id)
    try {
      const result = await this.#directories.listChildren(
        directory,
        this.#lifetime.signal,
      )
      this.#lifetime.signal.throwIfAborted()
      if (result.status === 'failed') {
        this.#tree.failDirectory(directory.id, result.failure)
        const committed = this.#tree.directoryState(directory.id)
        if (committed.status !== 'failed') {
          throw new Error('catalog directory failure did not become terminal')
        }
        return Object.freeze({ status: 'failed', failure: committed.failure })
      }
      if (result.generation.directoryId !== directory.id) {
        throw new Error('catalog response generation belongs to another directory')
      }
      return Object.freeze({
        status: 'ready',
        generation: this.#tree.publishDirectory(result.generation),
      })
    } catch (error) {
      if (ownsLoadingState) {
        this.#tree.abandonDirectoryLoad(directory.id)
      }
      throw error
    }
  }

  async #transferFile(file: CatalogFileNode): Promise<void> {
    let prepared: PreparedFileTransfer
    try {
      this.#lifetime.signal.throwIfAborted()
      prepared = await this.#files.open(file, this.#lifetime.signal)
      this.#lifetime.signal.throwIfAborted()
    } catch (error) {
      if (this.#lifetime.signal.aborted) {
        throw error
      }
      this.#recordFileFailure(file, error)
      return
    }

    let transfer: FileAttempt
    try {
      transfer = await this.#transferPrepared(file, prepared)
    } catch (isolationFailure) {
      const released = await releaseAttempt(prepared)
      const fatalFailure = released.status === 'failed'
        ? combinedFailure(isolationFailure, released.reason)
        : isolationFailure
      this.#recordFileFailure(file, fatalFailure)
      throw fatalFailure
    }
    const released = await releaseAttempt(prepared)
    const attempt = combineAttempts(transfer, released)
    if (attempt.status === 'failed') {
      this.#recordFileFailure(file, attempt.reason)
    }
  }

  async #transferPrepared(
    file: CatalogFileNode,
    prepared: PreparedFileTransfer,
  ): Promise<FileAttempt> {
    let begun: BeginOutputFileResult | undefined
    try {
      const outputFile = this.#outputFile(file, prepared)
      begun = await this.#output.beginFile(outputFile)
      const { transaction, durableRanges } = bindOutputFileTransaction(
        begun,
        outputFile,
        this.#outputIdentity,
      )
      await prepared.transfer(
        transaction,
        durableRanges,
        this.#lifetime.signal,
      )
      this.#lifetime.signal.throwIfAborted()
      await transaction.commit()
      return succeededAttempt()
    } catch (error) {
      if (begun === undefined) {
        return failedAttempt(error)
      }
      await abortTransaction(begun.transaction, error)
      return failedAttempt(error)
    }
  }

  #outputFile(file: CatalogFileNode, prepared: PreparedFileTransfer): OutputFile {
    if (prepared.source.shareInstance !== this.#options.shareInstance ||
        prepared.source.fileId !== file.id ||
        prepared.exactSize !== file.expectedSize) {
      throw new Error('opened file revision does not match its committed catalog candidate')
    }
    return snapshotOutputFile({
      source: prepared.source,
      path: this.#tree.outputPath(file.id),
      exactSize: prepared.exactSize,
      ...(file.modifiedTime === undefined ||
          !this.#output.capabilities.modificationTime
        ? {}
        : { modifiedTimeMilliseconds: file.modifiedTime.milliseconds }),
    })
  }

  #recordFileFailure(file: CatalogFileNode, reason: unknown): void {
    this.#failures.record(Object.freeze({ kind: 'file', fileId: file.id, reason }))
  }

  #observeAbort(signal: AbortSignal | undefined): void {
    if (signal === undefined) {
      return
    }
    const abort = () => this.#lifetime.abort(
      signal.reason ?? new DOMException('Transfer job aborted', 'AbortError'),
    )
    if (signal.aborted) {
      abort()
      return
    }
    signal.addEventListener('abort', abort, { once: true })
    this.#externalAbortCleanup = () => signal.removeEventListener('abort', abort)
  }

  #emitMeasure(measure: SelectionMeasure): void {
    this.#options.onMeasure?.(measure)
  }

  #settle(outcome: JobOutcome): TransferJobResult {
    return Object.freeze({ outcome, measure: this.#measure.snapshot() })
  }
}

async function releaseAttempt(
  prepared: PreparedFileTransfer,
): Promise<FileAttempt> {
  try {
    await prepared.release()
    return succeededAttempt()
  } catch (error) {
    return failedAttempt(error)
  }
}

async function abortTransaction(
  transaction: OutputFileTransaction,
  transferFailure: unknown,
): Promise<void> {
  let disposition
  try {
    disposition = await transaction.abort(transferFailure)
  } catch (abortFailure) {
    throw new AggregateError(
      [transferFailure, abortFailure],
      'File transfer failed and output rollback also failed',
      { cause: abortFailure },
    )
  }
  if (disposition !== 'FileIsolated') {
    throw new AggregateError(
      [transferFailure],
      'File failure compromised the surrounding output job',
      { cause: transferFailure },
    )
  }
}

function combineAttempts(
  transfer: FileAttempt,
  release: FileAttempt,
): FileAttempt {
  if (transfer.status === 'succeeded') {
    return release
  }
  if (release.status === 'succeeded') {
    return transfer
  }
  return failedAttempt(combinedFailure(transfer.reason, release.reason))
}

function combinedFailure(primary: unknown, secondary: unknown): AggregateError {
  return new AggregateError(
    [primary, secondary],
    'File transfer failed and revision release also failed',
    { cause: secondary },
  )
}

function succeededAttempt(): FileAttempt {
  return Object.freeze({ status: 'succeeded' })
}

function failedAttempt(reason: unknown): FileAttempt {
  return Object.freeze({ status: 'failed', reason })
}
