import type { CatalogClient } from '../catalog/client'
import type { ShareDescriptor } from '../catalog/records'
import type { FrozenSelectionPolicy } from '../catalog/selection'
import type { BlockRangeReader, ContentLaneStatus } from '../content/broker'
import type { RevisionReader } from '../content/session-services'
import {
  discoverSelection,
  type DiscoveredDirectory,
  type DiscoveredFile,
  type MissingSelectionTarget,
} from './discovery'
import type { ActiveFileWrite } from './active-file-write'
import { FaultScope, FileDestinationConflictError, TransferFaultError, catalogCapacityReason, isRecoverableTransferInterruption, storagePauseReason, transferFailureReason } from './fault'
import { transferSingleFile, type FileTransferContext, type SingleFileTransferOutcome, type TransferExit, type TransferPause } from './file-transfer'
import type { TransferOutputSession } from './output-port'
import { TransferProgressTracker, type TransferProgress } from './progress'
import { createTaskId } from './selection'
import type {
  MissingFileWarning,
  ReceiveContentWarning,
  ReceivePartialReason,
  ReceivePauseReason,
  SafeRenamedSummary,
  SourceRevisionFailure,
  SourceRevisionFailures,
} from './state'

const MAX_MISSING_WARNINGS = 20
const MAX_SOURCE_FAILURES = 20
const EMPTY_ROOT_PATH: readonly string[] = Object.freeze([])

export interface TransferJobOptions {
  readonly transferJobId?: string
  readonly descriptor: ShareDescriptor
  readonly catalog: CatalogClient
  readonly selection: FrozenSelectionPolicy
  readonly revisions: RevisionReader
  readonly broker: BlockRangeReader
  readonly lanes?: ContentLaneStatus
  readonly output: TransferOutputSession
  readonly rootPath?: readonly string[]
  readonly onUncheckpointedWrite?: () => void
  readonly onProgress?: (progress: TransferProgress) => void
}

type NonPartialSettledStatus =
  | 'completed'
  | 'paused'
  | 'stopped'
  | 'source-changed'
  | 'source-busy'
  | 'failed'

type NonPartialSettledVariant<S extends NonPartialSettledStatus = NonPartialSettledStatus> =
  S extends NonPartialSettledStatus ? Readonly<{ status: S }> : never

export interface TransferJobFacts {
  readonly pauseReason?: ReceivePauseReason
  readonly completedFiles: bigint
  readonly completedBytes: bigint
  readonly discoveredFiles: bigint
  readonly discoveredBytes: bigint
  readonly discoveryComplete: boolean
  readonly failedFiles: bigint
  readonly failedDirectories: bigint
  readonly sourceOmittedEntries?: bigint
  readonly sourceRevisionFailures?: SourceRevisionFailures
  readonly contentWarning?: ReceiveContentWarning
  readonly safeRenamed?: SafeRenamedSummary
  readonly error?: unknown
}

export type SettledTransferJobResult = TransferJobFacts & (
  | Readonly<{ status: 'partial'; partialReason: ReceivePartialReason }>
  | NonPartialSettledVariant
)
export type TransferJobResult = SettledTransferJobResult | (TransferJobFacts & Readonly<{ status: 'detached' }>)

export interface ActiveCheckpointSummary {
  readonly allReceivedSaved: boolean
  readonly savedFiles: number
  readonly skippedFiles: number
  readonly failedFiles: number
  readonly savedBytes: bigint
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
  #exit: TransferExit | undefined
  readonly #activeWrites = new Set<ActiveFileWrite>()
  #finished = false
  #firstError: unknown
  #discoveryComplete = false
  #discoveryFailed = false
  #sourceChangedCount = 0n
  #sourceUnavailableCount = 0n
  #sourceBusyCount = 0n
  readonly #sourceFailures: SourceRevisionFailure[] = []
  readonly #missingFiles: MissingFileWarning[] = []
  #completedFiles = 0n
  #completedBytes = 0n
  #discoveredFiles = 0n
  #discoveredBytes = 0n
  #failedFiles = 0n
  #failedDirectories = 0n
  #missingSelectionTargets = 0n
  #unmatchedTargets: readonly MissingSelectionTarget[] = []
  #sourceOmittedEntries = 0n
  // Shared per-job callback identities let LaneSet ref-count remaining work once
  // across concurrent files and let BlockBroker deduplicate shared receipt callbacks.
  readonly #onObjectBytes = (bytes: number) => this.#tracker.observeObjectBytes(bytes)
  readonly #remainingWork = () => {
    const snap = this.#tracker.snapshot()
    const rem = Number(snap.discoveredBytes - snap.materializedBytes)
    return { bytes: Math.max(0, rem), known: this.#discoveryComplete && Number.isSafeInteger(rem) }
  }

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
    this.#requestPause({ kind: 'paused', reason })
  }

  #requestPause(pause: TransferPause): void {
    const previous = this.#exit
    if (previous === undefined) {
      this.#exit = pause
      this.#abort.abort(new DOMException('Transfer paused', 'AbortError'))
    } else if (previous.kind === 'paused' && previous.sharedOutputFailure === undefined) {
      // A shared object failure prevents sibling flushes. A local storage failure only refines the pause reason.
      const wasRequested = previous.reason === 'user' || previous.reason === 'interrupted'
      const needsStorage = pause.reason === 'storage-full' || pause.reason === 'permission-needed'
      if (pause.sharedOutputFailure !== undefined || (wasRequested && needsStorage)) this.#exit = pause
    }
  }

  stop(): void {
    if (this.#abort.signal.aborted) return
    this.#exit = { kind: 'stopped' }
    this.#abort.abort(new DOMException('Transfer stopped', 'AbortError'))
  }

  detach(): void {
    if (this.#abort.signal.aborted) return
    this.#exit = { kind: 'detached' }
    this.#abort.abort(new DOMException('Transfer detached', 'AbortError'))
  }

  get allReceivedSaved(): boolean {
    return this.#options.output.capabilities.resumable && !this.#finished && !this.#abort.signal.aborted &&
      this.#failedFiles === 0n &&
      this.#tracker.snapshot().materializedBytes > 0n && [...this.#activeWrites].every(file => file.durable)
  }

  async checkpointActive(signal: AbortSignal): Promise<ActiveCheckpointSummary> {
    const round = AbortSignal.any([signal, this.#abort.signal])
    let savedFiles = 0, skippedFiles = 0, failedFiles = 0
    let savedBytes = 0n
    for (const file of [...this.#activeWrites]) {
      if (round.aborted || this.#finished) break
      try {
        const cut = await file.checkpointOnDisconnect(round)
        if (cut?.outcome === 'saved') {
          savedFiles += 1
          savedBytes += cut.savedBytes
        } else if (cut?.outcome === 'skipped') {
          skippedFiles += 1
        }
      } catch (error) {
        failedFiles += 1
        this.#handleOutputFailure(error)
      }
    }
    return {
      allReceivedSaved: !round.aborted && skippedFiles === 0 && failedFiles === 0 && this.allReceivedSaved,
      savedFiles, skippedFiles, failedFiles, savedBytes,
    }
  }

  #handleOutputFailure(error: unknown): boolean {
    if (this.#exit !== undefined && this.#exit.kind !== 'paused') return true
    const reason = storagePauseReason(error)
    const sharedFailure = error instanceof TransferFaultError && error.fault.scope === FaultScope.Job ? error : undefined
    if (reason !== undefined && this.#options.output.capabilities.resumable) {
      this.#firstError ??= error
      this.#requestPause(sharedFailure === undefined
        ? { kind: 'paused', reason }
        : { kind: 'paused', reason, sharedOutputFailure: sharedFailure })
      return true
    }
    if (reason !== undefined || sharedFailure !== undefined) {
      this.#failJob(error)
      return true
    }
    return false
  }

  #failJob(error: unknown): void {
    this.#firstError = error
    this.#exit = { kind: 'failed', error }
    this.#abort.abort(error)
  }

  #abortAllOrNothing(): void {
    if (this.#options.output.capabilities.partialOutput || this.#exit !== undefined) return
    // All-or-nothing outputs discard all buffered bytes once any member fails, so stop
    // remaining discovery and workers immediately while keeping the originating status.
    this.#exit = { kind: 'aborted' }
    this.#abort.abort(new DOMException('All-or-nothing output aborted after failure', 'AbortError'))
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
      this.#finalizeDiscovery()
      if (this.#discoveryComplete && this.#unmatchedTargets.length > 0) {
        await this.#verifyUnmatchedTargets(this.#unmatchedTargets)
      }
    } catch (error) {
      this.#handleDiscoveryError(error)
    } finally {
      await Promise.allSettled(active)
      this.#finished = true
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
      this.#options.rootPath ?? EMPTY_ROOT_PATH,
      this.#abort.signal,
      {
        onOmittedEntries: count => {
          if (count <= 0n) return
          this.#sourceOmittedEntries += count
          this.#tracker.observeOmittedEntries(count)
        },
        onDirectoryFailed: (_id, error) => this.#recordDirectoryFailure(error),
        onFileDiscovered: file => {
          this.#discoveredFiles += 1n
          this.#discoveredBytes += file.entry.expectedSize
          this.#tracker.observeFileDiscovered(file.entry.expectedSize)
        },
        onUnmatchedTargets: targets => {
          this.#unmatchedTargets = targets
        },
      },
    )

    for await (const item of items) {
      if (this.#abort.signal.aborted) break
      if (item.kind === 'directory') {
        await this.#ensureDirectory(item)
        continue
      }
      while (active.size >= maxWorkers && !this.#abort.signal.aborted) {
        await Promise.race(active)
      }
      if (this.#abort.signal.aborted) break
      const task = this.#runFile(item).finally(() => active.delete(task))
      active.add(task)
    }
  }

  async #ensureDirectory(item: DiscoveredDirectory): Promise<void> {
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
      this.#recordDirectoryFailure(error)
    }
  }

  #recordDirectoryFailure(error: unknown): void {
    if (this.#discoveryInterrupted(error)) return
    this.#discoveryFailed = true
    this.#failedDirectories += 1n
    this.#recordFailure(error)
    this.#tracker.observeDirectoryFailed()
    this.#abortAllOrNothing()
  }

  #finalizeDiscovery(): void {
    if (!this.#abort.signal.aborted && !this.#discoveryFailed) {
      this.#discoveryComplete = true
      this.#tracker.observeDiscoverySettled('complete')
    } else if (this.#discoveryFailed) {
      this.#tracker.observeDiscoverySettled('failed')
    }
  }

  #discoveryInterrupted(error: unknown): boolean {
    if (this.#handleOutputFailure(error) || this.#abort.signal.aborted) return true
    if (!this.#options.output.capabilities.resumable || !isRecoverableTransferInterruption(error)) return false
    this.pause('interrupted')
    return true
  }

  #handleDiscoveryError(error: unknown): void {
    if (this.#discoveryInterrupted(error)) return
    this.#discoveryFailed = true
    this.#recordFailure(error)
    this.#tracker.observeDiscoverySettled('failed')
    this.#abortAllOrNothing()
  }

  async #verifyUnmatchedTargets(targets: readonly MissingSelectionTarget[]): Promise<void> {
    const checkCompleted = this.#options.output.checkCompleted
    for (const target of targets) {
      if (this.#abort.signal.aborted) return
      if (target.kind === 'file' && checkCompleted !== undefined) {
        const resolved = await this.#verifyUnmatchedCompletedFile(target.idText, checkCompleted)
        if (resolved || this.#abort.signal.aborted) continue
      }
      this.#recordMissingSelectionTarget(target)
    }
  }

  async #verifyUnmatchedCompletedFile(
    fileId: string,
    checkCompleted: NonNullable<TransferOutputSession['checkCompleted']>,
  ): Promise<boolean> {
    try {
      const completed = await checkCompleted(fileId, this.#abort.signal)
      if (completed === undefined) return false
      // A resumed file already verified complete locally satisfies its selection rule
      // even if a subsequent catalog snapshot no longer lists it.
      this.#discoveredFiles += 1n
      this.#discoveredBytes += completed.size
      this.#completedFiles += 1n
      this.#completedBytes += completed.size
      this.#tracker.observeFileDiscovered(completed.size)
      this.#tracker.observeInitialCoverage(completed.size)
      this.#tracker.observeFileCompleted(completed.size)
      return true
    } catch (error) {
      if (this.#discoveryInterrupted(error)) return true
      this.#failedFiles += 1n
      this.#recordFailure(error)
      this.#tracker.observeFileFailed()
      this.#abortAllOrNothing()
      return true
    }
  }

  #recordMissingSelectionTarget(target: MissingSelectionTarget): void {
    // Absence proven by a complete catalog is a closed selection mismatch rather
    // than a successful empty transfer. Keep it separate from `#sourceChangedCount`
    // because undiscovered targets lack a sourcePath for replacement lookup.
    if (target.kind === 'file') this.#failedFiles += 1n
    else this.#failedDirectories += 1n
    this.#missingSelectionTargets += 1n
    this.#tracker.observeSelectionError()
    this.#abortAllOrNothing()
  }

  async #runFile(file: DiscoveredFile): Promise<void> {
    let current: ActiveFileWrite | undefined
    const trackWrite = (next: ActiveFileWrite | undefined) => {
      if (current !== undefined) this.#activeWrites.delete(current)
      current = next
      if (next !== undefined) this.#activeWrites.add(next)
    }
    const context: FileTransferContext = {
      revisions: this.#options.revisions,
      broker: this.#options.broker,
      output: this.#options.output,
      signal: this.#abort.signal,
      getExit: () => this.#exit,
      onOutputFailure: error => this.#handleOutputFailure(error),
      onMemberFailure: () => this.#abortAllOrNothing(),
      ...(this.#options.onUncheckpointedWrite === undefined ? {} : { onUncheckpointedWrite: this.#options.onUncheckpointedWrite }),
      ...(this.#options.output.capabilities.resumable ? { trackWrite } : {}),
      onInitialCoverage: bytes => this.#tracker.observeInitialCoverage(bytes),
      onObjectBytes: this.#onObjectBytes,
      onWriteCoverageChange: delta => this.#tracker.observeWriteCoverageChange(delta),
      onCapacityWait: (delta, ms, attempt) => this.#tracker.observeCapacityWait(delta, ms, attempt),
      remainingWork: this.#remainingWork,
    }
    try {
      this.#applyFileOutcome(file, await transferSingleFile(context, file))
    } finally {
      trackWrite(undefined)
    }
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
      case 'detached':
      case 'aborted':
        return
      case 'stopped':
        this.stop()
        return
      case 'source-changed':
        this.#failedFiles += 1n
        this.#sourceChangedCount += 1n
        this.#tracker.observeFileFailed()
        this.#recordSourceFailure(file, 'source-changed')
        this.#recordMissing({ path: file.relativePath, reason: 'source-changed' })
        this.#abortAllOrNothing()
        return
      case 'source-unavailable':
        this.#failedFiles += 1n
        this.#sourceUnavailableCount += 1n
        this.#firstError ??= outcome.error
        this.#tracker.observeFileFailed()
        this.#recordSourceFailure(file, 'source-unavailable')
        this.#recordMissing({ path: file.relativePath, reason: 'source-unavailable' })
        this.#abortAllOrNothing()
        return
      case 'source-busy':
        this.#failedFiles += 1n
        this.#sourceBusyCount += 1n
        this.#firstError ??= outcome.error
        this.#tracker.observeFileFailed()
        this.#recordSourceFailure(file, 'source-busy')
        this.#recordMissing({ path: file.relativePath, reason: 'source-busy' })
        this.#abortAllOrNothing()
        return
      case 'failed':
        this.#failedFiles += 1n
        this.#recordFailure(outcome.error)
        this.#tracker.observeFileFailed()
        this.#recordMissing(outcome.error instanceof FileDestinationConflictError
          ? { path: file.relativePath, reason: 'destination-conflict', destinationPath: outcome.error.destinationPath }
          : { path: file.relativePath, reason: 'file-failed' })
        this.#abortAllOrNothing()
        return
    }
  }

  #recordFailure(error: unknown): void {
    const reason = transferFailureReason(error)
    if (this.#exit !== undefined || reason === 'error') {
      this.#firstError ??= error
    } else if (reason === 'relay-access-denied') {
      // A confirmed relay denial applies to every remaining file and directory.
      this.#failJob(error)
    } else {
      this.#firstError = error
    }
  }

  #recordSourceFailure(file: DiscoveredFile, reason: SourceRevisionFailure['reason']): void {
    if (this.#sourceFailures.length >= MAX_SOURCE_FAILURES) return
    this.#sourceFailures.push(Object.freeze({
      entryId: file.entry.idText,
      path: file.relativePath,
      sourcePath: file.sourcePath,
      reason,
    }))
  }

  #recordMissing(warning: MissingFileWarning): void {
    if (this.#missingFiles.length >= MAX_MISSING_WARNINGS) return
    this.#missingFiles.push(Object.freeze(warning))
  }

  #buildResult(): TransferJobResult {
    const safeRenamed = this.#options.output.safeRenamedSummary?.()
    const sourceFailureCount = this.#sourceChangedCount + this.#sourceUnavailableCount + this.#sourceBusyCount
    const sourceRevisionFailures: SourceRevisionFailures | undefined = sourceFailureCount === 0n
      ? undefined
      : Object.freeze({
          shareInstance: this.#options.descriptor.shareInstanceId,
          count: sourceFailureCount,
          files: Object.freeze([...this.#sourceFailures]),
        })
    const contentWarning: ReceiveContentWarning | undefined = this.#missingFiles.length === 0
      ? undefined
      : Object.freeze({
          selectedFileCount: this.#discoveredFiles,
          completedFileCount: this.#completedFiles,
          missingFiles: Object.freeze([...this.#missingFiles]),
        })

    const base: TransferJobFacts = {
      ...(this.#exit?.kind === 'paused' ? { pauseReason: this.#exit.reason } : {}),
      completedFiles: this.#completedFiles,
      completedBytes: this.#completedBytes,
      discoveredFiles: this.#discoveredFiles,
      discoveredBytes: this.#discoveredBytes,
      discoveryComplete: this.#discoveryComplete,
      failedFiles: this.#failedFiles,
      failedDirectories: this.#failedDirectories,
      ...(this.#sourceOmittedEntries > 0n ? { sourceOmittedEntries: this.#sourceOmittedEntries } : {}),
      ...(sourceRevisionFailures === undefined ? {} : { sourceRevisionFailures }),
      ...(contentWarning === undefined ? {} : { contentWarning }),
      ...(safeRenamed === undefined ? {} : { safeRenamed }),
      ...(this.#firstError === undefined ? {} : { error: this.#firstError }),
    }
    const status = this.#resolveStatus()
    if (status === 'partial') {
      return Object.freeze({ ...base, status, partialReason: this.#resolvePartialReason() })
    }
    return Object.freeze({ ...base, status })
  }

  #resolvePartialReason(): ReceivePartialReason {
    if (this.#exit?.kind === 'stopped') return 'stopped'
    return catalogCapacityReason(this.#firstError) ?? 'failures'
  }

  #resolveStatus(): TransferJobResult['status'] {
    if (this.#exit?.kind === 'detached' || this.#exit?.kind === 'paused') return this.#exit.kind
    if (this.#exit?.kind === 'failed' || this.#exit?.kind === 'stopped') {
      return this.#canProducePartial() ? 'partial' : this.#exit.kind
    }
    if (
      this.#failedFiles === 0n &&
      this.#failedDirectories === 0n &&
      this.#missingSelectionTargets === 0n &&
      !this.#discoveryFailed
    ) {
      return 'completed'
    }
    if (this.#canProducePartial()) return 'partial'
    if (this.#sourceChangedCount > 0n || this.#missingSelectionTargets > 0n) return 'source-changed'
    return this.#allFailuresSourceBusy() ? 'source-busy' : 'failed'
  }

  #canProducePartial(): boolean {
    return this.#options.output.capabilities.partialOutput && this.#completedFiles > 0n
  }

  #allFailuresSourceBusy(): boolean {
    // Only promote to job-level source-busy when every failure was a busy lock;
    // mixed failures stay generic failed with per-file contentWarning details.
    return (
      this.#sourceBusyCount > 0n &&
      this.#sourceBusyCount === this.#failedFiles &&
      this.#failedDirectories === 0n &&
      !this.#discoveryFailed
    )
  }
}
