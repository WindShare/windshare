import type { CatalogClient } from '../catalog/client'
import type { ShareDescriptor } from '../catalog/records'
import type { FrozenSelectionPolicy } from '../catalog/selection'
import type { BlockRangeReader, ContentLaneStatus } from '../content/broker'
import type { RevisionReader } from '../content/session-services'
import { discoverSelection, type DiscoveredFile } from './discovery'
import { transferSingleFile, type SingleFileTransferOutcome } from './file-transfer'
import type { TransferOutputSession } from './output-port'
import { TransferProgressTracker, type TransferProgress } from './progress'
import { createTaskId } from './selection'
import type {
  MissingFileWarning,
  ReceiveContentWarning,
  ReceivePauseReason,
  SafeRenamedSummary,
  SourceRevisionFailure,
  SourceRevisionFailures,
} from './state'

const MAX_MISSING_WARNINGS = 20
const MAX_SOURCE_FAILURES = 20

export interface TransferJobOptions {
  readonly transferJobId?: string
  readonly descriptor: ShareDescriptor
  readonly catalog: CatalogClient
  readonly selection: FrozenSelectionPolicy
  readonly revisions: RevisionReader
  readonly broker: BlockRangeReader
  readonly lanes?: ContentLaneStatus
  readonly output: TransferOutputSession
  readonly stripSingleFilePrefix?: boolean
  readonly onProgress?: (progress: TransferProgress) => void
}

export type TransferJobStatus =
  | 'completed'
  | 'paused'
  | 'stopped'
  | 'partial'
  | 'source-changed'
  | 'failed'

export interface TransferJobResult {
  readonly status: TransferJobStatus
  readonly pauseReason?: ReceivePauseReason
  readonly completedFiles: bigint
  readonly completedBytes: bigint
  readonly discoveredFiles: bigint
  readonly discoveredBytes: bigint
  readonly discoveryComplete: boolean
  readonly failedFiles: bigint
  readonly failedDirectories: bigint
  readonly sourceRevisionFailures?: SourceRevisionFailures
  readonly contentWarning?: ReceiveContentWarning
  readonly safeRenamed?: SafeRenamedSummary
  readonly error?: unknown
}

/**
 * Coordinates progressive catalog discovery and bounded concurrent file transfers
 * against any `TransferOutputSession` backend.
 */
export class TransferJob {
  readonly transferJobId: string
  readonly #options: TransferJobOptions
  readonly #abort = new AbortController()
  readonly #tracker: TransferProgressTracker
  #paused = false
  #pauseReason: ReceivePauseReason = 'user'
  #stopped = false
  #firstError: unknown
  #discoveryComplete = false
  #discoveryFailed = false
  #sourceChangedCount = 0n
  readonly #sourceFailures: SourceRevisionFailure[] = []
  readonly #missingFiles: MissingFileWarning[] = []
  #completedFiles = 0n
  #completedBytes = 0n
  #discoveredFiles = 0n
  #discoveredBytes = 0n
  #failedFiles = 0n
  #failedDirectories = 0n

  constructor(options: TransferJobOptions) {
    this.transferJobId = options.transferJobId ?? createTaskId()
    this.#options = options
    this.#tracker = new TransferProgressTracker(
      this.transferJobId,
      progress => options.onProgress?.(progress),
      options.output.sessionId,
    )
  }

  pause(reason: ReceivePauseReason = 'user'): void {
    if (this.#paused || this.#stopped) return
    this.#paused = true
    this.#pauseReason = reason
    this.#abort.abort(new DOMException('Transfer paused', 'AbortError'))
  }

  stop(): void {
    if (this.#stopped) return
    this.#stopped = true
    this.#abort.abort(new DOMException('Transfer stopped', 'AbortError'))
  }

  progress(): TransferProgress {
    return this.#tracker.snapshot()
  }

  async run(externalSignal?: AbortSignal): Promise<TransferJobResult> {
    const onExternalAbort = () => this.stop()
    if (externalSignal?.aborted) this.stop()
    externalSignal?.addEventListener('abort', onExternalAbort, { once: true })
    const unsubscribeBandwidth = this.#observeLanes()
    const maxWorkers = Math.max(1, this.#options.output.capabilities.maxConcurrentFiles)
    const active = new Set<Promise<void>>()

    try {
      await this.#runDiscoveryLoop(active, maxWorkers)
      await this.#finalizeDiscovery()
    } catch (error) {
      this.#handleDiscoveryError(error)
    } finally {
      await Promise.allSettled(active)
      unsubscribeBandwidth?.()
      externalSignal?.removeEventListener('abort', onExternalAbort)
    }

    return this.#buildResult()
  }

  #observeLanes(): (() => void) | undefined {
    const lanes = this.#options.lanes
    if (lanes === undefined) return undefined
    this.#tracker.observeContentLanes(lanes.size)
    return lanes.subscribeBandwidthWait?.(waiting => {
      this.#tracker.observeBandwidthWait(waiting)
    })
  }

  async #runDiscoveryLoop(active: Set<Promise<void>>, maxWorkers: number): Promise<void> {
    const items = discoverSelection(
      this.#options.descriptor,
      this.#options.catalog,
      this.#options.selection,
      this.#options.stripSingleFilePrefix ?? false,
      this.#abort.signal,
      {
        onOmittedEntries: count => this.#tracker.observeOmittedEntries(count),
        onDirectoryFailed: (_id, error) => this.#recordDirectoryFailure(error),
        onFileDiscovered: file => {
          this.#discoveredFiles += 1n
          this.#discoveredBytes += file.entry.expectedSize
          this.#tracker.observeFileDiscovered(file.entry.expectedSize)
        },
      },
    )

    for await (const item of items) {
      if (this.#paused || this.#stopped) break
      if (item.kind === 'directory') {
        await this.#ensureDirectory(item)
        continue
      }
      while (active.size >= maxWorkers && !this.#paused && !this.#stopped) {
        await Promise.race(active)
      }
      if (this.#paused || this.#stopped) break
      const task = this.#runFile(item).finally(() => active.delete(task))
      active.add(task)
    }
  }

  async #ensureDirectory(item: Extract<Awaited<ReturnType<typeof discoverSelection>> extends AsyncGenerator<infer T> ? T : never, { kind: 'directory' }>): Promise<void> {
    if (this.#options.output.ensureDirectory === undefined) return
    try {
      await this.#options.output.ensureDirectory({
        directoryId: item.entry.idText,
        path: item.relativePath,
        ...(item.entry.modifiedTime === undefined ? {} : {
          modifiedTimeMilliseconds: item.entry.modifiedTime.seconds * 1_000n,
        }),
      }, this.#abort.signal)
    } catch (error) {
      if (this.#paused || this.#stopped) return
      this.#recordDirectoryFailure(error)
    }
  }

  #recordDirectoryFailure(error: unknown): void {
    this.#discoveryFailed = true
    this.#failedDirectories += 1n
    this.#firstError ??= error
    this.#tracker.observeDirectoryFailed()
  }

  async #finalizeDiscovery(): Promise<void> {
    if (!this.#paused && !this.#stopped && !this.#discoveryFailed) {
      this.#discoveryComplete = true
      this.#tracker.observeDiscoverySettled('complete')
      await this.#options.output.markDiscoveryComplete?.()
      return
    }
    if (this.#discoveryFailed) {
      this.#tracker.observeDiscoverySettled('failed')
    }
  }

  #handleDiscoveryError(error: unknown): void {
    if (this.#paused || this.#stopped) return
    this.#discoveryFailed = true
    this.#firstError ??= error
    this.#tracker.observeDiscoverySettled('failed')
  }

  async #runFile(file: DiscoveredFile): Promise<void> {
    const outcome = await transferSingleFile({
      revisions: this.#options.revisions,
      broker: this.#options.broker,
      output: this.#options.output,
      signal: this.#abort.signal,
      isPaused: () => this.#paused,
      isStopped: () => this.#stopped,
      onInitialCoverage: bytes => this.#tracker.observeInitialCoverage(bytes),
      onObjectBytes: bytes => this.#tracker.observeObjectBytes(bytes),
      onWritten: bytes => this.#tracker.observeWritten(bytes),
      onCapacityWait: (delta, ms, attempt) => this.#tracker.observeCapacityWait(delta, ms, attempt),
      remainingWork: () => {
        const snap = this.#tracker.snapshot()
        const rem = Number(snap.discoveredBytes - snap.materializedBytes)
        return { bytes: Math.max(0, rem), known: this.#discoveryComplete && Number.isSafeInteger(rem) }
      },
    }, file)

    this.#applyFileOutcome(file, outcome)
  }

  #applyFileOutcome(file: DiscoveredFile, outcome: SingleFileTransferOutcome): void {
    switch (outcome.kind) {
      case 'completed':
        this.#completedFiles += 1n
        this.#completedBytes += outcome.size
        this.#tracker.observeFileCompleted(outcome.size)
        return
      case 'paused':
        this.pause(outcome.reason)
        return
      case 'stopped':
        this.stop()
        return
      case 'source-changed':
        this.#failedFiles += 1n
        this.#sourceChangedCount += 1n
        this.#tracker.observeFileFailed()
        this.#recordSourceFailure(file, 'source-changed')
        this.#recordMissing(file.relativePath, 'source-changed')
        return
      case 'source-unavailable':
        this.#failedFiles += 1n
        this.#firstError ??= outcome.error
        this.#tracker.observeFileFailed()
        this.#recordSourceFailure(file, 'source-unavailable')
        this.#recordMissing(file.relativePath, 'source-unavailable')
        return
      case 'failed':
        this.#failedFiles += 1n
        this.#firstError ??= outcome.error
        this.#tracker.observeFileFailed()
        this.#recordMissing(file.relativePath, 'file-failed')
        return
    }
  }

  #recordSourceFailure(file: DiscoveredFile, reason: 'source-changed' | 'source-unavailable'): void {
    if (this.#sourceFailures.length >= MAX_SOURCE_FAILURES) return
    this.#sourceFailures.push(Object.freeze({
      entryId: file.entry.idText,
      path: file.relativePath,
      sourcePath: file.sourcePath,
      reason,
    }))
  }

  #recordMissing(path: readonly string[], reason: MissingFileWarning['reason']): void {
    if (this.#missingFiles.length >= MAX_MISSING_WARNINGS) return
    this.#missingFiles.push(Object.freeze({ path, reason }))
  }

  #buildResult(): TransferJobResult {
    const safeRenamed = this.#options.output.safeRenamedSummary?.()
    const sourceRevisionFailures: SourceRevisionFailures | undefined = this.#sourceFailures.length === 0
      ? undefined
      : Object.freeze({
          shareInstance: this.#options.descriptor.shareInstanceId,
          count: this.#sourceChangedCount > 0n ? this.#sourceChangedCount : BigInt(this.#sourceFailures.length),
          files: Object.freeze([...this.#sourceFailures]),
        })
    const contentWarning: ReceiveContentWarning | undefined = this.#missingFiles.length === 0
      ? undefined
      : Object.freeze({
          selectedFileCount: this.#discoveredFiles,
          completedFileCount: this.#completedFiles,
          missingFiles: Object.freeze([...this.#missingFiles]),
        })

    const status = this.#resolveStatus()
    return Object.freeze({
      status,
      ...(status === 'paused' ? { pauseReason: this.#pauseReason } : {}),
      completedFiles: this.#completedFiles,
      completedBytes: this.#completedBytes,
      discoveredFiles: this.#discoveredFiles,
      discoveredBytes: this.#discoveredBytes,
      discoveryComplete: this.#discoveryComplete,
      failedFiles: this.#failedFiles,
      failedDirectories: this.#failedDirectories,
      ...(sourceRevisionFailures === undefined ? {} : { sourceRevisionFailures }),
      ...(contentWarning === undefined ? {} : { contentWarning }),
      ...(safeRenamed === undefined ? {} : { safeRenamed }),
      ...(this.#firstError === undefined ? {} : { error: this.#firstError }),
    })
  }

  #resolveStatus(): TransferJobStatus {
    if (this.#paused) return 'paused'
    if (this.#stopped) return this.#completedFiles > 0n ? 'partial' : 'stopped'
    if (this.#failedFiles === 0n && this.#failedDirectories === 0n && !this.#discoveryFailed) {
      return 'completed'
    }
    if (this.#completedFiles === 0n && this.#sourceChangedCount > 0n) {
      return 'source-changed'
    }
    return this.#completedFiles > 0n ? 'partial' : 'failed'
  }
}
