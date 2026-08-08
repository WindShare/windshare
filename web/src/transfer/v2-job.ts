import { directoryId, fileId } from '../catalog/model'
import { encodeBase64Url, equalBytes } from '../crypto/bytes'
import type { V2CommittedDirectory } from '../catalog/v2-page-store'
import { V2_CATALOG_IDENTITY_BYTES } from '../catalog/v2-records'
import type {
  V2CatalogEntry,
} from '../catalog/v2-records'
import type { V2FrozenSelectionPolicy } from '../catalog/v2-selection'
import { SelectionMeasureTracker, type SelectionMeasure } from './measure'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  TransferFailureAccumulator,
  jobOutcome,
} from './outcome'
import {
  DirectorySettlementKind,
  directoryAdmissionScope,
  snapshotOutputDirectoryAdmission,
  validateDirectoryAdmissionBinding,
  type DirectoryAdmission,
  type DirectoryAdmissionScope,
  type OutputDirectoryAdmission,
} from './directory-admission'
import {
  validateOutputSessionBinding,
  JobSettlementKind,
  type OutputSession,
} from './output-session'
import {
  validateFinalTransferIntent,
  snapshotTransferRunId,
  type TransferIntent,
  type TransferIntentDraft,
  type TransferTraceEvent,
} from './intent'
import {
  V2CatalogTraversalError,
  V2OutputPausedError,
  V2_MAXIMUM_PENDING_DIRECTORIES,
  type DirectoryCursor,
  type DirectoryWork,
  type PendingFile,
  type TransferJobOptions,
  type TransferJobResult,
} from './job/contract'
import {
  isolatedDirectoryOutputFailure,
  isV2FileScopedTransferFailure,
} from './job/failures'
import { AsyncBoundedQueue, pendingFileMetadataBytes } from './job/scheduler'
import {
  V2ExplicitSelectionTargetLedger,
  V2SelectionTargetMissingError,
} from './job/selection'
import { V2CatalogTraversalGuard } from './job/traversal'
import {
  createTransferJobId,
  descriptorRootId,
  transferIntentAuthority,
} from './job/identity'
import { transferJobLimits, type TransferJobLimits } from './job/limits'
import { V2TransferObservers } from './job/observers'
import { transferV2File } from './job/file-transfer'
import {
  directorySettlementDecision,
  finalizeV2Directories,
  runV2DirectoryTransferWorker,
} from './job/directory-transfer'
import { discoverV2DirectoryGeneration } from './discovery/v2-generation-replay'
import { V2TransferProgressLedger } from './progress/v2-ledger'
import {
  outputSettlementTimeoutMilliseconds,
  pauseFailedV2Output,
} from './settlement/v2-output'

export {
  V2CatalogTraversalError,
  V2DirectoryTraversalError,
  V2DirectoryAncestry,
  V2OutputPausedError,
  V2_MAXIMUM_CATALOG_NODE_CLAIMS,
  V2_MAXIMUM_CONCURRENT_DIRECTORIES,
  V2_MAXIMUM_CONCURRENT_FILES,
  V2_MAXIMUM_DIRECTORY_ADMISSIONS,
  V2_MAXIMUM_PENDING_DIRECTORIES,
  V2_MAXIMUM_PENDING_FILES,
  V2_MAXIMUM_PENDING_FILE_METADATA_BYTES,
} from './job/contract'
export type {
  TransferJobOptions,
  TransferJobResult,
  TransferProgress,
} from './job/contract'
export { isV2FileScopedTransferFailure } from './job/failures'
export { V2RangeReaderContractError } from './job/file-transfer'
export {
  V2_DEFAULT_OUTPUT_SETTLEMENT_TIMEOUT_MILLISECONDS,
  V2_MAXIMUM_OUTPUT_SETTLEMENT_TIMEOUT_MILLISECONDS,
  V2OutputSettlementTimeoutError,
} from './settlement/v2-output'

/**
 * Incremental producer/consumer transfer scheduler. A catalog generation is
 * admitted only after its terminal page is authenticated; files from that
 * generation then flow through a bounded queue while sibling scans continue.
 */
export class TransferJob {
  readonly #options: TransferJobOptions
  readonly #selection: V2FrozenSelectionPolicy
  readonly #limits: TransferJobLimits
  readonly #lifetime = new AbortController()
  readonly #measure = new SelectionMeasureTracker()
  readonly #failures = new TransferFailureAccumulator()
  readonly #traversal: V2CatalogTraversalGuard
  readonly #explicitTargets: V2ExplicitSelectionTargetLedger
  readonly #observers: V2TransferObservers
  readonly #finalizableDirectories: DirectoryAdmission[] = []
  readonly #failedDirectoryIds = new Set<string>()
  readonly #transferJobId: string
  readonly #outputSettlementTimeoutMilliseconds: number
  readonly #progress = new V2TransferProgressLedger()
  #directoryAdmissionClaims = 0
  #externalAbortCleanup: (() => void) | undefined
  #output: OutputSession | undefined
  #directoryAdmissionScope: DirectoryAdmissionScope | undefined
  #rootAdmission: DirectoryAdmission | undefined
  #rootCommitted: V2CommittedDirectory | undefined
  #intent: TransferIntent | undefined
  #started = false

  constructor(options: TransferJobOptions) {
    this.#options = options
    this.#selection = 'snapshot' in options.selection
      ? options.selection.snapshot()
      : options.selection
    this.#limits = transferJobLimits(options)
    this.#outputSettlementTimeoutMilliseconds = outputSettlementTimeoutMilliseconds(
      options.outputSettlementTimeoutMilliseconds,
    )
    this.#transferJobId = snapshotTransferRunId(options.transferJobId ?? createTransferJobId())
    this.#observers = new V2TransferObservers({
      descriptor: options.descriptor,
      transferJobId: this.#transferJobId,
      lanes: options.lanes,
      ...(options.protocolSessionId === undefined ? {} : { protocolSessionId: options.protocolSessionId }),
      ...(options.onProgress === undefined ? {} : { onProgress: options.onProgress }),
      ...(options.onMeasure === undefined ? {} : { onMeasure: options.onMeasure }),
      ...(options.onTrace === undefined ? {} : { onTrace: options.onTrace }),
    })
    this.#explicitTargets = new V2ExplicitSelectionTargetLedger(this.#selection, this.#lifetime.signal)
    this.#traversal = new V2CatalogTraversalGuard(
      options.descriptor.shareInstance,
      this.#limits.catalogNodeClaims,
    )
  }

  async run(signal?: AbortSignal): Promise<TransferJobResult> {
    if (this.#started) throw new Error('v2 transfer job can only run once')
    this.#started = true
    this.#observeAbort(signal)
    try {
      const authority = await transferIntentAuthority(this.#options, this.#selection)
      const opened = await this.#openRunOutput(authority.input, authority.expected)
      this.#trace('intent-frozen', { decision: 'picker-confirmed' })
      this.#output = opened.session
      this.#intent = opened.intent
      this.#directoryAdmissionScope = directoryAdmissionScope(opened.intent)
      this.#trace('output-open', { outputSessionId: this.#output.identity.outputSessionId })
      this.#rootAdmission = await this.#admitRoot(this.#output, opened.intent)
      this.#finalizableDirectories.push(this.#rootAdmission)
      this.#emitProgress()
      const result = await this.#runIncremental()
      return result
    } catch (error) {
      return this.#settleRunFailure(error)
    } finally {
      this.#externalAbortCleanup?.()
    }
  }

  async #openRunOutput(
    input: TransferIntent | TransferIntentDraft,
    expected: TransferIntentDraft,
  ): Promise<{ readonly intent: TransferIntent; readonly session: OutputSession }> {
    const opened = 'output' in input
      ? {
          intent: input,
          session: await this.#options.output.openOutput(input, this.#lifetime.signal),
        }
      : await this.#options.output.confirmOutput(input, this.#lifetime.signal)
    // From the moment an authority returns a session, this job owns its
    // settlement even if the authority also returned a mismatched intent.
    this.#output = opened.session
    const intent = await validateFinalTransferIntent(opened.intent, expected)
    const session = validateOutputSessionBinding(intent, opened.session)
    return Object.freeze({ intent, session })
  }

  async #settleRunFailure(error: unknown): Promise<TransferJobResult> {
    if (!this.#lifetime.signal.aborted) this.#lifetime.abort(error)
    const output = this.#output
    const settlement = await pauseFailedV2Output({
      ...(output === undefined ? {} : { output }),
      authority: this.#options.output,
      reason: error,
      timeoutMilliseconds: this.#outputSettlementTimeoutMilliseconds,
    })
    const discoveryWasOpen = this.#measure.snapshot().discovery === 'open'
    let measure = this.#measure.snapshot()
    if (discoveryWasOpen) {
      measure = this.#measure.fail()
      this.#observers.measure(measure)
      this.#emitProgress()
    }
    this.#trace(
      settlement.kind === JobSettlementKind.NeedsAttention ? 'job-needs-attention' : 'job-paused',
      { decision: settlement.kind },
    )
    return Object.freeze({
      outcome: jobOutcome('Paused', this.#failures.snapshot()),
      settlement,
      measure,
      abortReason: error,
      transferJobId: this.#transferJobId,
      ...(this.#intent === undefined ? {} : { intent: this.#intent }),
      ...(output === undefined ? {} : { outputDurability: output.capabilities.durability }),
    })
  }

  async #runIncremental(): Promise<TransferJobResult> {
    const rootAdmission = this.#rootAdmission
    if (rootAdmission === undefined) throw new V2CatalogTraversalError('Synthetic root was not admitted')
    const directoryQueue = new AsyncBoundedQueue<DirectoryWork>(
      V2_MAXIMUM_PENDING_DIRECTORIES,
      BigInt(V2_MAXIMUM_PENDING_DIRECTORIES),
      () => 1n,
      true,
    )
    const fileQueue = new AsyncBoundedQueue<PendingFile>(
      this.#limits.pendingFiles,
      this.#limits.pendingFileMetadataBytes,
      pendingFileMetadataBytes,
    )
    const root: DirectoryCursor = {
      id: this.#options.descriptor.syntheticRoot.slice(),
      idText: descriptorRootId(this.#options.descriptor),
      path: Object.freeze([]),
      ancestry: Object.freeze([descriptorRootId(this.#options.descriptor)]),
      selected: this.#selection.directorySelected(descriptorRootId(this.#options.descriptor), []),
    }
    this.#traversal.claimNode(root.id)
    await directoryQueue.push({
      cursor: root,
      materializeParent: async () => rootAdmission,
    }, this.#lifetime.signal)

    const directoryWorkers = Array.from({ length: this.#limits.concurrentDirectories }, () =>
      runV2DirectoryTransferWorker(directoryQueue, fileQueue, {
        signal: this.#lifetime.signal,
        discoverDirectory: (work, files) => this.#discoverDirectory(work, files),
        isolateDirectory: (directoryId, error) => {
          this.#recordDirectoryFailure(directoryId, error)
          this.#trace('discovery-failed', { decision: 'directory-isolated' })
        },
      }),
    )
    const fileWorkers = Array.from({ length: this.#limits.concurrentFiles }, () =>
      this.#fileWorker(fileQueue),
    )
    try {
      await Promise.all(directoryWorkers)
      directoryQueue.close()
      fileQueue.close()
      this.#finishDiscovery()
      await Promise.all(fileWorkers)
    } catch (error) {
      // A queue failure is a job-wide failure. Abort the shared lifetime before
      // draining workers so in-flight catalog, content, and output operations
      // cannot outlive the authority whose failure made their work invalid.
      if (!this.#lifetime.signal.aborted) this.#lifetime.abort(error)
      directoryQueue.abort(error)
      fileQueue.abort(error)
      await Promise.allSettled([...directoryWorkers, ...fileWorkers])
      throw error
    }

    const measure = this.#measure.snapshot()
    const output = this.#output
    if (output === undefined) throw new Error('output session disappeared before finalization')
    this.#lifetime.signal.throwIfAborted()
    await finalizeV2Directories({
      admissions: this.#finalizableDirectories,
      output,
      signal: this.#lifetime.signal,
      settled: (admission, settlement) => {
        if (settlement.kind === DirectorySettlementKind.IsolatedFailure) {
          this.#recordDirectoryFailure(admission.directoryId, settlement.fault)
        }
        this.#trace('directory-finalized', {
          directoryId: admission.directoryId,
          generation: admission.generation,
          outputSessionId: output.identity.outputSessionId,
          decision: directorySettlementDecision(settlement),
        })
      },
    })
    const outcome = this.#failures.failureCount === 0
      ? jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
      : jobOutcome('CompletedWithErrors', this.#failures.snapshot())
    const settlement = await output.completeJob(outcome, this.#lifetime.signal)
    if (settlement.kind === JobSettlementKind.Paused) {
      throw new Error('Output completion returned a paused settlement')
    }
    this.#trace('output-finalized', { outputSessionId: output.identity.outputSessionId })
    return Object.freeze({
      outcome,
      settlement,
      measure,
      transferJobId: this.#transferJobId,
      ...(this.#intent === undefined ? {} : { intent: this.#intent }),
      outputDurability: output.capabilities.durability,
    })
  }

  async #fileWorker(queue: AsyncBoundedQueue<PendingFile>): Promise<void> {
    while (true) {
      const file = await queue.pop(this.#lifetime.signal)
      if (file === undefined) return
      try {
        await file.ready
        this.#lifetime.signal.throwIfAborted()
        await this.#transferFile(file)
      } catch (error) {
        if (this.#lifetime.signal.aborted) throw error
        if (isV2FileScopedTransferFailure(error)) {
          this.#recordFileFailure(file.entry, error)
          continue
        }
        throw error
      }
    }
  }

  async *#discoverDirectory(
    work: DirectoryWork,
    files: AsyncBoundedQueue<PendingFile>,
  ): AsyncGenerator<DirectoryWork, void> {
    const { cursor } = work
    const validateEntireGeneration = cursor.selected ||
      (cursor.path.length === 0 && !this.#explicitTargets.hasPendingTargets)
    const discoverySignal = this.#explicitTargets.discoverySignal(validateEntireGeneration)
    yield* discoverV2DirectoryGeneration({
      cursor,
      catalog: this.#options.catalog,
      traversal: this.#traversal,
      lifetimeSignal: this.#lifetime.signal,
      discoverySignal,
      validateEntireGeneration,
      ...(this.#rootCommitted === undefined ? {} : { rootCommitted: this.#rootCommitted }),
      opaqueSearchSatisfied: () => this.#explicitTargets.opaqueSearchSatisfied(validateEntireGeneration),
      observeDirectory: (id) => this.#explicitTargets.observeDirectory(id),
      observeEntry: (entry) => this.#observeCatalogEntry(cursor, entry),
      generationCommitted: (committed) => {
        if (cursor.path.length === 0) return
        this.#trace('directory-generation-committed', {
          directoryId: cursor.idText,
          generation: encodeBase64Url(committed.generation),
          decision: 'authenticated',
        })
      },
      recordDirectoryFailure: (id, error) => this.#recordDirectoryFailure(id, error),
      replayConsumer: (committed) => {
        const materialize = this.#directoryMaterializer(work, cursor, committed)
        return {
          materializeSelectedDirectory: async () => { await materialize() },
          prepare: (entry) => this.#prepareCatalogEntry(cursor, materialize, entry, files),
        }
      },
    })
  }

  #observeCatalogEntry(cursor: DirectoryCursor, entry: V2CatalogEntry): boolean {
    this.#traversal.entryPath(cursor, entry)
    this.#traversal.claimNode(entry.id)
    this.#explicitTargets.observe(entry)
    const selected = this.#selection.selected(entry, cursor.ancestry)
    if (entry.kind === 'file' && selected) {
      const measure = this.#measure.observeUniqueFile(entry.expectedSize)
      this.#observers.measure(measure)
      this.#emitProgress()
    }
    return selected
  }

  async #prepareCatalogEntry(
    cursor: DirectoryCursor,
    materialize: () => Promise<DirectoryAdmission>,
    entry: V2CatalogEntry,
    files: AsyncBoundedQueue<PendingFile>,
  ): Promise<DirectoryWork | undefined> {
    this.#lifetime.signal.throwIfAborted()
    const path = this.#traversal.entryPath(cursor, entry)
    if (entry.kind === 'file') {
      await this.#enqueueFile(cursor, materialize, entry, path, files)
      return undefined
    }
    const selected = this.#selection.selected(entry, cursor.ancestry)
    if (!selected && !this.#explicitTargets.hasPendingTargets) return undefined
    if (!this.#selection.shouldDiscover(entry.idText, cursor.ancestry)) return undefined
    return {
      cursor: {
        id: entry.id.slice(),
        idText: entry.idText,
        path,
        ancestry: Object.freeze([...cursor.ancestry, entry.idText]),
        selected,
        ...(entry.modifiedTime === undefined ? {} : { modifiedTime: entry.modifiedTime }),
      },
      materializeParent: materialize,
    }
  }

  async #enqueueFile(
    cursor: DirectoryCursor,
    materialize: () => Promise<DirectoryAdmission>,
    entry: Extract<V2CatalogEntry, { kind: 'file' }>,
    path: readonly string[],
    files: AsyncBoundedQueue<PendingFile>,
  ): Promise<void> {
    if (!this.#selection.selected(entry, cursor.ancestry)) return
    const admission = await materialize()
    let release: () => void = () => undefined
    const ready = new Promise<void>((resolve) => { release = resolve })
    try {
      await files.push({
        entry,
        path,
        parentAdmission: admission,
        ready,
        ...(entry.modifiedTime === undefined ? {} : { modifiedTime: entry.modifiedTime }),
      }, this.#lifetime.signal)
      this.#trace('file-enqueued', {
        fileId: entry.idText,
        selectionDecision: this.#selection.decision(entry, cursor.ancestry),
      })
    } finally {
      release()
    }
  }

  async #admitRoot(output: OutputSession, intent: TransferIntent): Promise<DirectoryAdmission> {
    const rootCommitted = await this.#options.catalog.loadDirectory(
      this.#options.descriptor.syntheticRoot,
      { signal: this.#lifetime.signal },
    )
    if (rootCommitted === undefined || !equalBytes(rootCommitted.directoryId, this.#options.descriptor.syntheticRoot) ||
        rootCommitted.generation.byteLength !== V2_CATALOG_IDENTITY_BYTES ||
        rootCommitted.generation.every((byte) => byte === 0)) {
      throw new V2CatalogTraversalError('Synthetic root committed generation is unavailable')
    }
    if (rootCommitted.omittedCount !== 0n) {
      throw new V2CatalogTraversalError('Synthetic root committed generation omitted catalog entries')
    }
    this.#rootCommitted = rootCommitted
    const request = snapshotOutputDirectoryAdmission({
      directoryId: intent.syntheticRoot,
      generation: encodeBase64Url(rootCommitted.generation),
      path: Object.freeze([]),
    })
    this.#trace('directory-generation-committed', {
      directoryId: request.directoryId,
      generation: request.generation,
      decision: 'authenticated',
    })
    this.#reserveDirectoryAdmission()
    const returned = await output.admitDirectory(request, this.#lifetime.signal)
    const admission = validateDirectoryAdmissionBinding(
      this.#requireDirectoryAdmissionScope(),
      request,
      returned,
    )
    this.#trace('directory-admitted', {
      directoryId: request.directoryId,
      generation: request.generation,
      outputSessionId: output.identity.outputSessionId,
      decision: 'accepted',
    })
    return admission
  }

  #directoryMaterializer(
    work: DirectoryWork,
    cursor: DirectoryCursor,
    committed: V2CommittedDirectory,
  ): () => Promise<DirectoryAdmission> {
    if (cursor.path.length === 0) return work.materializeParent
    let materialized: Promise<DirectoryAdmission> | undefined
    return () => {
      materialized ??= (async () => {
        const admission = await this.#admitDirectory(
          cursor,
          committed,
          await work.materializeParent(),
        )
        this.#finalizableDirectories.push(admission)
        const outputSessionId = this.#output?.identity.outputSessionId
        if (outputSessionId === undefined) {
          throw new V2OutputPausedError('Output session disappeared after directory admission')
        }
        this.#trace('directory-admitted', {
          directoryId: cursor.idText,
          generation: encodeBase64Url(committed.generation),
          outputSessionId,
          decision: 'accepted',
        })
        return admission
      })()
      return materialized
    }
  }

  async #admitDirectory(
    cursor: DirectoryCursor,
    committed: V2CommittedDirectory,
    parentAdmission: DirectoryAdmission,
  ): Promise<DirectoryAdmission> {
    this.#reserveDirectoryAdmission()
    const output = this.#output
    if (output === undefined) throw new V2OutputPausedError('Output admission is unavailable')
    const request: OutputDirectoryAdmission = snapshotOutputDirectoryAdmission({
      directoryId: encodeBase64Url(cursor.id),
      generation: encodeBase64Url(committed.generation),
      path: cursor.path,
      parentAdmission,
      ...(cursor.modifiedTime === undefined ? {} : {
        modifiedTime: cursor.modifiedTime,
      }),
    })
    let returned: DirectoryAdmission
    try {
      returned = await output.admitDirectory(request, this.#lifetime.signal)
    } catch (error) {
      const isolated = isolatedDirectoryOutputFailure(
        error,
        output.capabilities.fileFailureIsolation,
        cursor.idText,
      )
      throw isolated ?? error
    }
    return validateDirectoryAdmissionBinding(
      this.#requireDirectoryAdmissionScope(),
      request,
      returned,
    )
  }

  #reserveDirectoryAdmission(): void {
    if (this.#directoryAdmissionClaims >= this.#limits.directoryAdmissions) {
      throw new V2OutputPausedError('Directory admission budget was exhausted')
    }
    this.#directoryAdmissionClaims += 1
  }

  async #transferFile(file: PendingFile): Promise<void> {
    const output = this.#output
    if (output === undefined) throw new Error('output session is unavailable')
    this.#trace('file-started', { fileId: file.entry.idText, outputSessionId: output.identity.outputSessionId })
    await transferV2File({
      descriptor: this.#options.descriptor,
      revisions: this.#options.revisions,
      broker: this.#options.broker,
      output,
      signal: this.#lifetime.signal,
      outputSettlementTimeoutMilliseconds: this.#outputSettlementTimeoutMilliseconds,
      onWriteAcknowledged: (bytes, firstWrite) => {
        this.#progress.acknowledgeWrite(bytes)
        if (firstWrite) this.#trace('file-written', {
          fileId: file.entry.idText,
          outputSessionId: output.identity.outputSessionId,
          decision: 'first-write',
        })
        this.#emitProgress()
      },
      onComplete: (exactSize) => {
        this.#progress.completeFile(exactSize)
        this.#trace('file-completed', {
          fileId: file.entry.idText,
          outputSessionId: output.identity.outputSessionId,
        })
        this.#emitProgress()
      },
    }, file)
  }

  #requireDirectoryAdmissionScope(): DirectoryAdmissionScope {
    if (this.#directoryAdmissionScope === undefined) {
      throw new V2OutputPausedError('Directory admission scope is unavailable')
    }
    return this.#directoryAdmissionScope
  }

  #recordDirectoryFailure(idText: string, reason: unknown): void {
    if (this.#failedDirectoryIds.has(idText)) return
    this.#failedDirectoryIds.add(idText)
    this.#progress.failDirectory()
    this.#failures.record(Object.freeze({ kind: 'directory', directoryId: directoryId(idText), reason }))
  }

  #recordFileFailure(entry: Extract<V2CatalogEntry, { kind: 'file' }>, reason: unknown): void {
    this.#failures.record(Object.freeze({ kind: 'file', fileId: fileId(entry.idText), reason }))
    this.#progress.recordFileError()
    this.#emitProgress()
  }

  #finishDiscovery(): SelectionMeasure {
    if (this.#progress.failedDirectories === 0) {
      for (const target of this.#explicitTargets.missing()) {
        const reason = new V2SelectionTargetMissingError(target)
        this.#failures.recordRepresentative(target.kind === 'directory'
          ? Object.freeze({ kind: 'directory', directoryId: directoryId(target.idText), reason })
          : Object.freeze({ kind: 'file', fileId: fileId(target.idText), reason }))
        this.#progress.recordSelectionError()
      }
    }
    const complete = this.#progress.failedDirectories === 0
    const measure = complete ? this.#measure.complete() : this.#measure.fail()
    this.#observers.measure(measure)
    this.#emitProgress()
    this.#trace(measure.discovery === 'failed' ? 'discovery-failed' : 'discovery-complete', {
      decision: measure.discovery,
    })
    return measure
  }

  #emitProgress(): void {
    const measure = this.#measure.snapshot()
    this.#observers.progress(this.#progress.snapshot(measure, this.#output?.identity.outputSessionId))
  }

  #trace(
    name: TransferTraceEvent['name'],
    extra: Omit<
      TransferTraceEvent['context'],
      'shareInstance' | 'transferJobId' | 'protocolSessionId'
    > = {},
  ): void {
    this.#observers.trace(name, extra)
  }

  #observeAbort(signal?: AbortSignal): void {
    if (signal === undefined) return
    const abort = () => {
      if (this.#lifetime.signal.aborted) return
      this.#lifetime.abort(signal.reason ?? new DOMException('Transfer aborted', 'AbortError'))
    }
    signal.addEventListener('abort', abort, { once: true })
    this.#externalAbortCleanup = () => signal.removeEventListener('abort', abort)
    if (signal.aborted) abort()
  }
}
