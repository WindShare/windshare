import { directoryId, fileId } from '../catalog/model'
import type { V2CommittedDirectory } from '../catalog/v2-page-store'
import {
  V2_CATALOG_IDENTITY_BYTES,
  type V2CatalogEntry,
} from '../catalog/v2-records'
import type { V2FrozenSelectionPolicy } from '../catalog/v2-selection'
import { encodeBase64Url, equalBytes } from '../crypto/bytes'
import type { ReceiveLifecycleState } from '../output/workspace/state'
import {
  DirectorySettlementKind,
  createDirectoryAdmissionScope,
  snapshotMaterializationDirectory,
  validateDirectoryAdmissionBinding,
  type DirectoryAdmission,
  type DirectoryAdmissionScope,
} from './directory-admission'
import { discoverV2DirectoryGeneration } from './discovery/v2-generation-replay'
import {
  V2CatalogTraversalError,
  V2OutputPausedError,
  V2_MAXIMUM_PENDING_DIRECTORIES,
  type AuthenticatedDirectory,
  type DirectoryCursor,
  type DirectoryWork,
  type PendingFile,
  type TransferJobOptions,
  type TransferJobResult,
} from './job/contract'
import {
  artifactDirectoryPath,
  artifactFilePath,
  artifactLayoutClass,
} from './job/artifact-path'
import {
  finalizeV2Directories,
  runV2DirectoryTransferWorker,
} from './job/directory-transfer'
import {
  isolatedDirectoryOutputFailure,
  isV2FileScopedTransferFailure,
  materializationFailureReason,
} from './job/failures'
import { transferV2File } from './job/file-transfer'
import {
  createTransferJobId,
  descriptorRootId,
  snapshotTransferJobId,
  validateTransferJobIntent,
} from './job/identity'
import { transferJobLimits, type TransferJobLimits } from './job/limits'
import { V2TransferObservers } from './job/observers'
import { ExactPreparationCollector } from './job/preparation'
import { AsyncBoundedQueue, pendingFileMetadataBytes } from './job/scheduler'
import { collectExactSingleFileEvidence } from './job/single-file-evidence'
import {
  V2ExplicitSelectionTargetLedger,
  V2SelectionTargetMissingError,
} from './job/selection'
import { V2CatalogTraversalGuard } from './job/traversal'
import { SelectionMeasureTracker, type SelectionMeasure } from './measure'
import type {
  DirectAtomicPlan,
  DirectTreePlan,
  OriginalFileArtifact,
  PortableHandoffPlan,
  ReceiveIntent,
  WorkspaceThenPublishPlan,
  ZipArchiveArtifact,
} from './intent'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  TransferFailureAccumulator,
  transferWorkerSettlement,
  type CompletedTransferWorkerSettlement,
  type SuccessfulTransferWorkerSettlement,
  type TransferWorkerSettlement,
} from './outcome'
import {
  snapshotDirectoryMaterializationRequest,
  validatePlanExecutionBinding,
  type ExactPreparationEvidence,
  type IncrementalDirectoryOutput,
  type MaterializationSummary,
  type PlanExecution,
} from './output-session'
import { V2TransferProgressLedger } from './progress/v2-ledger'
import {
  outputSettlementTimeoutMilliseconds,
  pauseFailedV2Execution,
  withOutputSettlementTimeout,
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
  TransferTraceEvent,
  TransferTraceListener,
} from './job/contract'
export { isV2FileScopedTransferFailure } from './job/failures'
export { V2RangeReaderContractError } from './job/file-transfer'
export {
  V2_DEFAULT_OUTPUT_SETTLEMENT_TIMEOUT_MILLISECONDS,
  V2_MAXIMUM_OUTPUT_SETTLEMENT_TIMEOUT_MILLISECONDS,
  V2OutputSettlementTimeoutError,
} from './settlement/v2-output'
export {
  V2PlanRouteUnavailableError,
  createV2PlanExecutionAuthority,
  snapshotExactPreparationEvidence,
  snapshotExactSingleFileEvidence,
} from './settlement/v2-plan-authority'
export {
  createPersistentDirectTreeExecution,
  createPersistentWorkspaceExecution,
} from './settlement/persistent-execution'
export type {
  PersistentDirectorySettlementEvidence,
  PersistentDirectTreeSettlementAuthority,
  PersistentMaterializationEvidence,
  PersistentMaterializationSettlementCut,
  PersistentWorkspaceExecutionInput,
  PersistentWorkspaceSettlementAuthority,
  WorkspaceMaterializationEvidence,
} from './settlement/persistent-execution'
export type {
  V2DirectAtomicExecutionRoute,
  V2DirectTreeExecutionRoute,
  V2PlanExecutionRouteRegistry,
  V2PortableOriginalExecutionRoute,
  V2PortableZipExecutionRoute,
  V2UnopenedExecutionLifecycle,
  V2WorkspaceOriginalExecutionRoute,
  V2WorkspaceZipExecutionRoute,
} from './settlement/v2-plan-authority'

class IncompleteArtifactError extends Error {
  constructor() {
    super('complete artifact materialization has selected or discovery failures')
    this.name = 'IncompleteArtifactError'
  }
}

type DirectTreeIntent = ReceiveIntent & Readonly<{ plan: DirectTreePlan }>
type DirectAtomicIntent = ReceiveIntent & Readonly<{ plan: DirectAtomicPlan }>
type WorkspaceIntent = ReceiveIntent & Readonly<{ plan: WorkspaceThenPublishPlan }>
type WorkspaceOriginalIntent = WorkspaceIntent & Readonly<{ artifact: OriginalFileArtifact }>
type WorkspaceZipIntent = WorkspaceIntent & Readonly<{ artifact: ZipArchiveArtifact }>
type PortableIntent = ReceiveIntent & Readonly<{ plan: PortableHandoffPlan }>

/**
 * Direct plans retain bounded discovery/content overlap. Prepared plans first
 * freeze exact authenticated catalog evidence; their output port is unreachable
 * until the plan adapter accepts that evidence.
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
  readonly #failedDirectoryIds = new Set<string>()
  readonly #finalizableDirectories: DirectoryAdmission[] = []
  readonly #materializedDirectoryPaths = new Set<string>()
  readonly #transferJobId: string
  readonly #outputSettlementTimeoutMilliseconds: number
  readonly #progress = new V2TransferProgressLedger()
  #directoryAdmissionClaims = 0
  #externalAbortCleanup: (() => void) | undefined
  #directoryScope: DirectoryAdmissionScope | undefined
  #directoryOutput: IncrementalDirectoryOutput | undefined
  #execution: PlanExecution | undefined
  #intent: TransferJobOptions['intent'] | undefined
  #observers: V2TransferObservers | undefined
  #rootCommitted: V2CommittedDirectory | undefined
  #preparation: ExactPreparationEvidence | undefined
  #lastWorker: TransferWorkerSettlement | undefined
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
    this.#transferJobId = snapshotTransferJobId(options.transferJobId ?? createTransferJobId())
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
      const intent = await validateTransferJobIntent(
        this.#options.intent,
        this.#options.descriptor,
        this.#selection,
      )
      this.#intent = intent
      this.#observers = new V2TransferObservers({
        intent,
        transferJobId: this.#transferJobId,
        lanes: this.#options.lanes,
        ...(this.#options.protocolSessionId === undefined
          ? {}
          : { protocolSessionId: this.#options.protocolSessionId }),
        ...(this.#options.onProgress === undefined ? {} : { onProgress: this.#options.onProgress }),
        ...(this.#options.onMeasure === undefined ? {} : { onMeasure: this.#options.onMeasure }),
        ...(this.#options.onTrace === undefined ? {} : { onTrace: this.#options.onTrace }),
      })
      this.#observers.intentFrozen(artifactLayoutClass(intent))
      this.#emitProgress()
      switch (intent.plan.kind) {
        case 'direct-tree': return await this.#runDirectTree(intent as DirectTreeIntent)
        case 'direct-atomic': return await this.#runDirectAtomic(intent as DirectAtomicIntent)
        case 'workspace-then-publish': return await this.#runWorkspace(intent as WorkspaceIntent)
        case 'portable-handoff': return await this.#runPreparedPortable(intent as PortableIntent)
      }
      throw new TypeError('receive intent has an unknown materialization plan')
    } catch (error) {
      if (this.#intent === undefined) throw error
      return this.#settleRunFailure(error)
    } finally {
      this.#externalAbortCleanup?.()
    }
  }

  async #runDirectTree(
    intent: DirectTreeIntent,
  ): Promise<TransferJobResult> {
    const execution = validatePlanExecutionBinding(
      intent,
      await this.#options.plans.openDirectTree(intent, this.#lifetime.signal),
    )
    this.#execution = execution
    this.#directoryOutput = execution.directories
    this.#directoryScope = await createDirectoryAdmissionScope(intent)
    this.#observers?.materializationStarted(execution.output.identity.outputSessionId)
    const root = await this.#directRoot()
    await this.#runDiscovery(root, newFileQueue(this.#limits))
    const measure = this.#finishDiscovery()
    await this.#finalizeDirectories()
    return this.#completeWorkers(measure)
  }

  async #runDirectAtomic(
    intent: DirectAtomicIntent,
  ): Promise<TransferJobResult> {
    const execution = validatePlanExecutionBinding(
      intent,
      await this.#options.plans.openDirectAtomic(intent, this.#lifetime.signal),
    )
    this.#execution = execution
    this.#directoryOutput = execution.directories
    if (execution.directories !== undefined) {
      this.#directoryScope = await createDirectoryAdmissionScope(intent)
    }
    this.#observers?.materializationStarted(execution.output.identity.outputSessionId)
    const root = execution.directories === undefined
      ? this.#authenticatedRoot(await this.#loadRoot())
      : await this.#directRoot()
    await this.#runDiscovery(root, newFileQueue(this.#limits))
    const measure = this.#finishDiscovery()
    if (execution.directories !== undefined) await this.#finalizeDirectories()
    return this.#completeWorkers(measure)
  }

  async #runWorkspace(intent: WorkspaceIntent): Promise<TransferJobResult> {
    switch (intent.artifact.kind) {
      case 'original-file': return this.#runWorkspaceOriginal(intent as WorkspaceOriginalIntent)
      case 'zip-archive': return this.#runPreparedWorkspaceZip(intent as WorkspaceZipIntent)
      case 'directory-tree':
        throw new TypeError('WorkspaceThenPublish does not support DirectoryTree artifacts')
    }
  }

  async #runWorkspaceOriginal(
    intent: WorkspaceOriginalIntent,
  ): Promise<TransferJobResult> {
    const committed = await this.#loadRoot()
    const evidence = await collectExactSingleFileEvidence({
      catalog: this.#options.catalog,
      descriptor: this.#options.descriptor,
      selection: this.#selection,
      artifact: intent.artifact,
      signal: this.#lifetime.signal,
      root: committed,
    })
    const admitted = await this.#options.plans.openWorkspaceOriginal(
      intent,
      evidence,
      this.#lifetime.signal,
    )
    if (admitted.kind === 'rejected') return this.#preparationRejected(admitted.state)
    const execution = validatePlanExecutionBinding(intent, admitted.execution)
    this.#execution = execution
    this.#observers?.materializationStarted(execution.output.identity.outputSessionId)
    const root = this.#authenticatedRoot(committed)
    await this.#runDiscovery(root, newFileQueue(this.#limits))
    const measure = this.#finishDiscovery()
    return this.#completeWorkers(measure)
  }

  async #runPreparedWorkspaceZip(
    intent: WorkspaceZipIntent,
  ): Promise<TransferJobResult> {
    const collector = await this.#collectExactPreparation(intent)
    if (this.#failures.failureCount !== 0) {
      return this.#settleIncompletePreparation(this.#measure.snapshot())
    }
    const evidence = collector.evidence()
    this.#preparation = evidence
    const prepared = await this.#options.plans.prepareWorkspaceZip(intent, evidence, this.#lifetime.signal)
    if (prepared.kind === 'rejected') return this.#preparationRejected(prepared.state)
    this.#execution = validatePlanExecutionBinding(intent, prepared.execution)
    return this.#runPreparedContent(collector, this.#measure.snapshot())
  }

  async #runPreparedPortable(
    intent: PortableIntent,
  ): Promise<TransferJobResult> {
    const collector = await this.#collectExactPreparation(intent)
    if (this.#failures.failureCount !== 0) {
      return this.#settleIncompletePreparation(this.#measure.snapshot())
    }
    const evidence = collector.evidence()
    this.#preparation = evidence
    const prepared = await this.#options.plans.preparePortable(intent, evidence, this.#lifetime.signal)
    if (prepared.kind === 'rejected') return this.#preparationRejected(prepared.state)
    this.#execution = validatePlanExecutionBinding(intent, prepared.execution)
    return this.#runPreparedContent(collector, this.#measure.snapshot())
  }

  async #collectExactPreparation(
    intent: TransferJobOptions['intent'],
  ): Promise<ExactPreparationCollector> {
    const collector = new ExactPreparationCollector(intent)
    const committed = await this.#loadRoot()
    const cursor = this.#rootCursor()
    const root: DirectoryWork = {
      cursor,
      materializeParent: async (role = 'ancestor') => this.#preparedDirectory(
        collector,
        cursor,
        committed,
        role,
      ),
    }
    await this.#runDiscovery(root, undefined, collector)
    this.#finishDiscovery()
    return collector
  }

  async #runPreparedContent(
    collector: ExactPreparationCollector,
    measure: SelectionMeasure,
  ): Promise<TransferJobResult> {
    const execution = this.#requireExecution()
    this.#observers?.materializationStarted(execution.output.identity.outputSessionId)
    await this.#transferPreparedFiles(collector.pendingFiles())
    return this.#completeWorkers(measure)
  }

  async #runDiscovery(
    root: DirectoryWork,
    directFiles?: AsyncBoundedQueue<PendingFile>,
    collector?: ExactPreparationCollector,
  ): Promise<void> {
    const directoryQueue = new AsyncBoundedQueue<DirectoryWork>(
      V2_MAXIMUM_PENDING_DIRECTORIES,
      BigInt(V2_MAXIMUM_PENDING_DIRECTORIES),
      () => 1n,
      true,
    )
    const sinkFiles = directFiles ?? newFileQueue(this.#limits)
    this.#traversal.claimNode(root.cursor.id)
    await directoryQueue.push(root, this.#lifetime.signal)

    const directoryWorkers = Array.from({ length: this.#limits.concurrentDirectories }, () =>
      runV2DirectoryTransferWorker(directoryQueue, sinkFiles, {
        signal: this.#lifetime.signal,
        discoverDirectory: (work, files) => this.#discoverDirectory(work, files, collector),
        isolateDirectory: (directoryIdentity, error) => {
          this.#recordDirectoryFailure(directoryIdentity, error)
        },
      }),
    )
    const fileWorkers = directFiles === undefined
      ? []
      : Array.from({ length: this.#limits.concurrentFiles }, () => this.#fileWorker(directFiles))
    const fileCompletion = Promise.all(fileWorkers).catch((error: unknown) => {
      if (!this.#lifetime.signal.aborted) this.#lifetime.abort(error)
      directoryQueue.abort(error)
      sinkFiles.abort(error)
      throw error
    })
    const directoryCompletion = Promise.all(directoryWorkers)
    try {
      // Attach to both producer and consumer completion immediately. A direct
      // file failure may abort discovery while a directory worker is still
      // unwinding; racing the two prevents that rejection from escaping as an
      // unobserved promise before the coordinated cleanup path can await it.
      await Promise.race([directoryCompletion, fileCompletion])
      await directoryCompletion
      directoryQueue.close()
      sinkFiles.close()
      await fileCompletion
    } catch (error) {
      if (!this.#lifetime.signal.aborted) this.#lifetime.abort(error)
      directoryQueue.abort(error)
      sinkFiles.abort(error)
      await Promise.allSettled([directoryCompletion, fileCompletion])
      throw error
    }
  }

  async *#discoverDirectory(
    work: DirectoryWork,
    files: AsyncBoundedQueue<PendingFile>,
    collector?: ExactPreparationCollector,
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
      observeDirectory: (identity) => this.#explicitTargets.observeDirectory(identity),
      observeEntry: (entry) => this.#observeCatalogEntry(cursor, entry),
      generationCommitted: (committed) => collector?.observeGeneration(cursor, committed),
      recordDirectoryFailure: (identity, error) => this.#recordDirectoryFailure(identity, error),
      replayConsumer: (committed) => {
        const materialize = this.#directoryMaterializer(work, cursor, committed, collector)
        return {
          materializeSelectedDirectory: async () => { await materialize('selected') },
          prepare: (entry) => this.#prepareCatalogEntry(
            cursor,
            materialize,
            entry,
            files,
            collector,
          ),
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
      this.#observers?.measure(measure)
      this.#emitProgress()
    }
    return selected
  }

  async #prepareCatalogEntry(
    cursor: DirectoryCursor,
    materialize: (role?: 'selected' | 'ancestor') => Promise<AuthenticatedDirectory>,
    entry: V2CatalogEntry,
    files: AsyncBoundedQueue<PendingFile>,
    collector?: ExactPreparationCollector,
  ): Promise<DirectoryWork | undefined> {
    this.#lifetime.signal.throwIfAborted()
    const sourcePath = this.#traversal.entryPath(cursor, entry)
    if (entry.kind === 'file') {
      if (!this.#selection.selected(entry, cursor.ancestry)) return undefined
      const parent = await materialize('ancestor')
      const artifactPath = artifactFilePath(this.#requireIntent(), sourcePath)
      if (collector !== undefined) {
        collector.addFile(entry, sourcePath, artifactPath, parent)
      } else {
        await this.#enqueueFile(entry, sourcePath, artifactPath, parent, files)
      }
      return undefined
    }
    const selected = this.#selection.selected(entry, cursor.ancestry)
    if (!selected && !this.#explicitTargets.hasPendingTargets) return undefined
    if (!this.#selection.shouldDiscover(entry.idText, cursor.ancestry)) return undefined
    return {
      cursor: {
        id: entry.id.slice(),
        idText: entry.idText,
        path: sourcePath,
        ancestry: Object.freeze([...cursor.ancestry, entry.idText]),
        selected,
        ...(entry.modifiedTime === undefined ? {} : { modifiedTime: entry.modifiedTime }),
      },
      materializeParent: materialize,
    }
  }

  async #enqueueFile(
    entry: Extract<V2CatalogEntry, { kind: 'file' }>,
    sourcePath: readonly string[],
    artifactPath: readonly string[],
    parent: AuthenticatedDirectory,
    files: AsyncBoundedQueue<PendingFile>,
  ): Promise<void> {
    let release: () => void = () => undefined
    const ready = new Promise<void>((resolve) => { release = resolve })
    try {
      await files.push(Object.freeze({
        entry,
        sourcePath,
        artifactPath,
        parent,
        ready,
        ...(entry.modifiedTime === undefined ? {} : { modifiedTime: entry.modifiedTime }),
      }), this.#lifetime.signal)
    } finally {
      release()
    }
  }

  #directoryMaterializer(
    work: DirectoryWork,
    cursor: DirectoryCursor,
    committed: V2CommittedDirectory,
    collector?: ExactPreparationCollector,
  ): (role?: 'selected' | 'ancestor') => Promise<AuthenticatedDirectory> {
    if (cursor.path.length === 0) return work.materializeParent
    let materialized: Promise<AuthenticatedDirectory> | undefined
    let initialRole: 'selected' | 'ancestor' | undefined
    return async (role = 'ancestor') => {
      if (materialized === undefined) {
        initialRole = role
        materialized = (async () => {
          const parent = await work.materializeParent('ancestor')
          return collector === undefined
            ? this.#admitDirectory(cursor, committed, parent)
            : this.#preparedDirectory(collector, cursor, committed, role)
        })()
      }
      const result = await materialized
      if (collector !== undefined && initialRole === 'selected' && role === 'ancestor') {
        return this.#preparedDirectory(collector, cursor, committed, role)
      }
      return result
    }
  }

  async #directRoot(): Promise<DirectoryWork> {
    const committed = await this.#loadRoot()
    const cursor = this.#rootCursor()
    const root = await this.#admitDirectory(cursor, committed)
    return Object.freeze({
      cursor,
      materializeParent: async () => root,
    })
  }

  async #admitDirectory(
    cursor: DirectoryCursor,
    committed: V2CommittedDirectory,
    parent?: AuthenticatedDirectory,
  ): Promise<AuthenticatedDirectory> {
    this.#reserveDirectoryAdmission()
    const output = this.#directoryOutput
    const scope = this.#directoryScope
    const execution = this.#execution
    if (output === undefined || scope === undefined || execution === undefined) {
      throw new V2OutputPausedError('incremental directory authority is unavailable')
    }
    if (cursor.path.length > 0 && parent?.admission === undefined) {
      throw new V2OutputPausedError('child directory lacks its authenticated parent receipt')
    }
    const source = snapshotMaterializationDirectory({
      directoryId: cursor.idText,
      generation: encodeBase64Url(committed.generation),
      path: cursor.path,
      ...(parent?.admission === undefined ? {} : { parentAdmission: parent.admission }),
      ...(cursor.modifiedTime === undefined ? {} : { modifiedTime: cursor.modifiedTime }),
    })
    const artifactPath = artifactDirectoryPath(this.#requireIntent(), cursor.path)
    const request = snapshotDirectoryMaterializationRequest({ source, artifactPath })
    let returned: DirectoryAdmission
    try {
      returned = await output.admitDirectory(request, this.#lifetime.signal)
    } catch (error) {
      const isolated = isolatedDirectoryOutputFailure(
        error,
        execution.output.capabilities.fileFailureIsolation,
        cursor.idText,
      )
      throw isolated ?? error
    }
    const admission = validateDirectoryAdmissionBinding(scope, source, returned)
    this.#finalizableDirectories.push(admission)
    if (artifactPath.length > 0) this.#materializedDirectoryPaths.add(artifactPath.join('/'))
    this.#observers?.directoryAdmitted({
      outputSessionId: execution.output.identity.outputSessionId,
      admittedDirectoryCount: BigInt(this.#directoryAdmissionClaims),
      layoutClass: scope.layout,
    })
    return Object.freeze({
      directoryId: cursor.idText,
      generation: encodeBase64Url(committed.generation),
      sourcePath: source.path,
      artifactPath,
      admission,
      ...(cursor.modifiedTime === undefined ? {} : { modifiedTime: cursor.modifiedTime }),
    })
  }

  #preparedDirectory(
    collector: ExactPreparationCollector,
    cursor: DirectoryCursor,
    committed: V2CommittedDirectory,
    role: 'selected' | 'ancestor',
  ): AuthenticatedDirectory {
    const artifactPath = artifactDirectoryPath(this.#requireIntent(), cursor.path)
    return artifactPath.length === 0
      ? collector.referenceDirectory(cursor, committed)
      : collector.materializeDirectory(cursor, committed, artifactPath, role)
  }

  async #finalizeDirectories(): Promise<void> {
    const output = this.#directoryOutput
    if (output === undefined) return
    await finalizeV2Directories({
      admissions: this.#finalizableDirectories,
      output,
      signal: this.#lifetime.signal,
      settled: (admission, settlement) => {
        if (settlement.kind === DirectorySettlementKind.IsolatedFailure) {
          this.#recordDirectoryFailure(admission.directoryId, settlement.fault)
        }
      },
      failed: (admission, error) => {
        const isolated = admission.path.length === 0
          ? undefined
          : isolatedDirectoryOutputFailure(
              error,
              this.#requireExecution().output.capabilities.fileFailureIsolation,
              admission.directoryId,
            )
        if (isolated === undefined) throw error
        this.#recordDirectoryFailure(admission.directoryId, isolated)
      },
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

  async #transferPreparedFiles(files: readonly PendingFile[]): Promise<void> {
    const queue = newFileQueue(this.#limits)
    const workers = Array.from({ length: this.#limits.concurrentFiles }, () => this.#fileWorker(queue))
    const completion = Promise.all(workers).catch((error: unknown) => {
      if (!this.#lifetime.signal.aborted) this.#lifetime.abort(error)
      queue.abort(error)
      throw error
    })
    const enqueue = (async () => {
      for (const file of files) await queue.push(file, this.#lifetime.signal)
      queue.close()
    })()
    try {
      // The bounded producer can be waiting for space when a consumer fails.
      // Observe both promises from creation so cancellation is one coordinated
      // failure, never a late unhandled rejection.
      await Promise.all([enqueue, completion])
    } catch (error) {
      if (!this.#lifetime.signal.aborted) this.#lifetime.abort(error)
      queue.abort(error)
      await Promise.allSettled([enqueue, completion])
      throw error
    }
  }

  async #transferFile(file: PendingFile): Promise<void> {
    const execution = this.#requireExecution()
    await transferV2File({
      descriptor: this.#options.descriptor,
      revisions: this.#options.revisions,
      broker: this.#options.broker,
      output: execution.output,
      signal: this.#lifetime.signal,
      outputSettlementTimeoutMilliseconds: this.#outputSettlementTimeoutMilliseconds,
      onWriteAcknowledged: (bytes) => {
        this.#progress.acknowledgeWrite(bytes)
        this.#emitProgress()
      },
      onComplete: (exactSize) => {
        this.#progress.completeFile(exactSize)
        this.#emitProgress()
      },
    }, file)
  }

  async #completeWorkers(measure: SelectionMeasure): Promise<TransferJobResult> {
    const worker: CompletedTransferWorkerSettlement = this.#failures.failureCount === 0
      ? transferWorkerSettlement('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
      : transferWorkerSettlement('CompletedWithErrors', this.#failures.snapshot())
    this.#lastWorker = worker
    const summary = this.#materializationSummary()
    const execution = this.#requireExecution()
    if (execution.planKind !== 'direct-tree' && worker.status !== 'Succeeded') {
      this.#observers?.materializationFailed(
        materializationFailureReason(worker.failures.at(0)?.reason),
        BigInt(this.#progress.completedFiles),
        this.#progress.completedBytes,
      )
      const lifecycle = await this.#pause(worker, new IncompleteArtifactError())
      return this.#result(worker, lifecycle, measure, new IncompleteArtifactError())
    }
    this.#observers?.materializationCompleted(summary)
    const lifecycle = await this.#settleCompleted(execution, worker, summary)
    if (execution.planKind === 'direct-tree') {
      this.#observers?.treeFinalized({
        outcome: lifecycle.kind === 'partial-directory' ? 'partial-directory' : 'published',
        successCount: BigInt(this.#progress.completedFiles),
        failureCount: BigInt(worker.failureCount),
      })
    }
    return this.#result(worker, lifecycle, measure)
  }

  async #settleCompleted(
    execution: PlanExecution,
    worker: CompletedTransferWorkerSettlement,
    summary: MaterializationSummary,
  ): Promise<ReceiveLifecycleState> {
    const controller = new AbortController()
    try {
      const state = await withOutputSettlementTimeout(
        'settle completed plan execution',
        this.#outputSettlementTimeoutMilliseconds,
        async () => {
          switch (execution.planKind) {
            case 'direct-tree': return execution.settle({
              transferJobId: this.#transferJobId,
              worker,
              materialization: summary,
            }, controller.signal)
            case 'direct-atomic':
            case 'workspace-then-publish':
            case 'portable-handoff': return execution.settle({
              transferJobId: this.#transferJobId,
              worker: requireSuccessfulWorker(worker),
              materialization: summary,
            }, controller.signal)
          }
        },
      )
      return validateCompletionLifecycle(this.#requireIntent(), worker, state)
    } finally {
      controller.abort(new DOMException('Plan settlement boundary closed', 'AbortError'))
    }
  }

  async #settleIncompletePreparation(measure: SelectionMeasure): Promise<TransferJobResult> {
    const worker = transferWorkerSettlement('CompletedWithErrors', this.#failures.snapshot())
    this.#lastWorker = worker
    const reason = new IncompleteArtifactError()
    const lifecycle = await this.#pause(worker, reason)
    return this.#result(worker, lifecycle, measure, reason)
  }

  #preparationRejected(state: ReceiveLifecycleState): TransferJobResult {
    const lifecycle = validatePreparationRejectionLifecycle(this.#requireIntent(), state)
    const worker = transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY)
    this.#lastWorker = worker
    return this.#result(worker, lifecycle, this.#measure.snapshot())
  }

  async #settleRunFailure(error: unknown): Promise<TransferJobResult> {
    if (!this.#lifetime.signal.aborted) this.#lifetime.abort(error)
    let measure = this.#measure.snapshot()
    if (measure.discovery === 'open') {
      measure = this.#measure.fail()
      this.#observers?.measure(measure)
      this.#emitProgress()
    }
    const worker = this.#lastWorker ?? transferWorkerSettlement('Paused', this.#failures.snapshot())
    this.#observers?.materializationFailed(
      materializationFailureReason(error),
      BigInt(this.#progress.completedFiles),
      this.#progress.completedBytes,
    )
    const lifecycle = await this.#pause(worker, error)
    if (this.#execution?.planKind === 'direct-tree') {
      const outcome = failedTreeOutcome(lifecycle)
      if (outcome !== undefined) {
        this.#observers?.treeFinalized({
          outcome,
          successCount: BigInt(this.#progress.completedFiles),
          failureCount: BigInt(worker.failureCount),
        })
      }
    }
    return this.#result(worker, lifecycle, measure, error)
  }

  async #pause(
    worker: TransferWorkerSettlement,
    reason: unknown,
  ): Promise<ReceiveLifecycleState> {
    const state = await pauseFailedV2Execution({
      intent: this.#requireIntent(),
      ...(this.#execution === undefined ? {} : { execution: this.#execution }),
      authority: this.#options.plans,
      worker,
      materialization: this.#materializationSummary(),
      reason,
      timeoutMilliseconds: this.#outputSettlementTimeoutMilliseconds,
      validateState: state => validatePauseLifecycle(this.#requireIntent(), worker, state),
    })
    return state
  }

  #result(
    worker: TransferWorkerSettlement,
    lifecycle: ReceiveLifecycleState,
    measure: SelectionMeasure,
    abortReason?: unknown,
  ): TransferJobResult {
    const execution = this.#execution
    return Object.freeze({
      worker,
      lifecycle,
      measure,
      transferJobId: this.#transferJobId,
      intent: this.#requireIntent(),
      ...(abortReason === undefined ? {} : { abortReason }),
      ...(execution === undefined ? {} : { outputDurability: execution.output.capabilities.durability }),
      ...(this.#preparation === undefined ? {} : { preparation: this.#preparation }),
    })
  }

  async #loadRoot(): Promise<V2CommittedDirectory> {
    if (this.#rootCommitted !== undefined) return this.#rootCommitted
    const root = await this.#options.catalog.loadDirectory(
      this.#options.descriptor.syntheticRoot,
      { signal: this.#lifetime.signal },
    )
    if (root === undefined || !equalBytes(root.directoryId, this.#options.descriptor.syntheticRoot) ||
        root.generation.byteLength !== V2_CATALOG_IDENTITY_BYTES ||
        root.generation.every(byte => byte === 0) || root.omittedCount !== 0n) {
      throw new V2CatalogTraversalError('Synthetic root committed generation is unavailable')
    }
    this.#rootCommitted = root
    return root
  }

  #rootCursor(): DirectoryCursor {
    const rootId = descriptorRootId(this.#options.descriptor)
    return Object.freeze({
      id: this.#options.descriptor.syntheticRoot.slice(),
      idText: rootId,
      path: Object.freeze([]),
      ancestry: Object.freeze([rootId]),
      selected: this.#selection.directorySelected(rootId, []),
    })
  }

  #authenticatedRoot(committed: V2CommittedDirectory): DirectoryWork {
    const cursor = this.#rootCursor()
    const root: AuthenticatedDirectory = Object.freeze({
      directoryId: cursor.idText,
      generation: encodeBase64Url(committed.generation),
      sourcePath: Object.freeze([]),
      artifactPath: Object.freeze([]),
    })
    return Object.freeze({ cursor, materializeParent: async () => root })
  }

  #reserveDirectoryAdmission(): void {
    if (this.#directoryAdmissionClaims >= this.#limits.directoryAdmissions) {
      throw new V2OutputPausedError('Directory admission budget was exhausted')
    }
    this.#directoryAdmissionClaims += 1
  }

  #recordDirectoryFailure(identity: string, reason: unknown): void {
    if (this.#failedDirectoryIds.has(identity)) return
    this.#failedDirectoryIds.add(identity)
    this.#progress.failDirectory()
    this.#failures.record(Object.freeze({ kind: 'directory', directoryId: directoryId(identity), reason }))
    this.#emitProgress()
  }

  #recordFileFailure(entry: Extract<V2CatalogEntry, { kind: 'file' }>, reason: unknown): void {
    this.#failures.record(Object.freeze({ kind: 'file', fileId: fileId(entry.idText), reason }))
    this.#progress.recordFileError()
    this.#emitProgress()
  }

  #finishDiscovery(): SelectionMeasure {
    const missing = this.#explicitTargets.missing()
    if (this.#progress.failedDirectories === 0) {
      for (const target of missing) {
        const reason = new V2SelectionTargetMissingError(target)
        this.#failures.recordRepresentative(target.kind === 'directory'
          ? Object.freeze({ kind: 'directory', directoryId: directoryId(target.idText), reason })
          : Object.freeze({ kind: 'file', fileId: fileId(target.idText), reason }))
        this.#progress.recordSelectionError()
      }
    }
    const complete = this.#progress.failedDirectories === 0 && missing.length === 0
    const measure = complete ? this.#measure.complete() : this.#measure.fail()
    this.#observers?.measure(measure)
    this.#emitProgress()
    return measure
  }

  #materializationSummary(): MaterializationSummary {
    const directoryCount = this.#preparation?.directoryCount ??
      BigInt(this.#materializedDirectoryPaths.size)
    const fileCount = BigInt(this.#progress.completedFiles)
    return Object.freeze({
      entryCount: directoryCount + fileCount,
      fileCount,
      directoryCount,
      rawBytes: this.#progress.completedBytes,
    })
  }

  #emitProgress(): void {
    const execution = this.#execution
    this.#observers?.progress(this.#progress.snapshot(
      this.#measure.snapshot(),
      execution?.output.identity.outputSessionId,
    ))
  }

  #requireIntent(): TransferJobOptions['intent'] {
    if (this.#intent === undefined) throw new Error('validated receive intent is unavailable')
    return this.#intent
  }

  #requireExecution(): PlanExecution {
    if (this.#execution === undefined) throw new Error('plan execution is unavailable')
    return this.#execution
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

function newFileQueue(limits: TransferJobLimits): AsyncBoundedQueue<PendingFile> {
  return new AsyncBoundedQueue<PendingFile>(
    limits.pendingFiles,
    limits.pendingFileMetadataBytes,
    pendingFileMetadataBytes,
  )
}

function requireSuccessfulWorker(
  worker: CompletedTransferWorkerSettlement,
): SuccessfulTransferWorkerSettlement {
  if (worker.status !== 'Succeeded') {
    throw new TypeError('complete artifact settlement requires successful workers')
  }
  return worker
}

function validateCompletionLifecycle(
  intent: TransferJobOptions['intent'],
  worker: CompletedTransferWorkerSettlement,
  state: ReceiveLifecycleState,
): ReceiveLifecycleState {
  validateLifecycleIdentity(intent, state)
  switch (intent.plan.kind) {
    case 'direct-tree':
      if (worker.status === 'Succeeded' && state.kind !== 'published') {
        throw new TypeError('successful DirectTree settlement must be published')
      }
      if (worker.status === 'CompletedWithErrors' && state.kind !== 'partial-directory' &&
          state.kind !== 'resumable-receive') {
        throw new TypeError('partial DirectTree settlement must remain partial or resumable')
      }
      break
    case 'direct-atomic':
      if (state.kind !== 'published') throw new TypeError('DirectAtomic settlement must be published')
      break
    case 'portable-handoff':
      if (state.kind !== 'download-started' || state.attemptKind !== 'portable') {
        throw new TypeError('PortableHandoff settlement can prove only portable DownloadStarted')
      }
      break
    case 'workspace-then-publish':
      if (!WORKSPACE_POST_MATERIALIZATION_STATES.has(state.kind)) {
        throw new TypeError('workspace settlement regressed before materialization completion')
      }
      break
  }
  return state
}

function validatePauseLifecycle(
  intent: TransferJobOptions['intent'],
  worker: TransferWorkerSettlement,
  state: ReceiveLifecycleState,
): ReceiveLifecycleState {
  validateLifecycleIdentity(intent, state)
  let allowed: ReadonlySet<ReceiveLifecycleState['kind']>
  switch (intent.plan.kind) {
    case 'direct-tree':
      allowed = DIRECT_TREE_PAUSE_STATES
      break
    case 'direct-atomic':
      allowed = DIRECT_ATOMIC_PAUSE_STATES
      break
    case 'workspace-then-publish':
      allowed = worker.status === 'Succeeded'
        ? WORKSPACE_POST_MATERIALIZATION_PAUSE_STATES
        : WORKSPACE_RECEIVE_PAUSE_STATES
      break
    case 'portable-handoff':
      allowed = worker.status === 'Succeeded'
        ? PORTABLE_POST_MATERIALIZATION_PAUSE_STATES
        : PORTABLE_RECEIVE_PAUSE_STATES
      break
  }
  if (!allowed.has(state.kind)) {
    throw new TypeError('plan pause returned a lifecycle state unavailable to that plan stage')
  }
  return state
}

function validatePreparationRejectionLifecycle(
  intent: TransferJobOptions['intent'],
  state: ReceiveLifecycleState,
): ReceiveLifecycleState {
  validateLifecycleIdentity(intent, state)
  if ((intent.plan.kind !== 'workspace-then-publish' && intent.plan.kind !== 'portable-handoff') ||
      !PREPARATION_REJECTION_STATES.has(state.kind)) {
    throw new TypeError('preparation rejection did not prove cleanup or uncertain ownership')
  }
  return state
}

function validateLifecycleIdentity(
  intent: TransferJobOptions['intent'],
  state: ReceiveLifecycleState,
): void {
  if (state.operationId !== intent.operationId || state.receiveIntentDigest !== intent.digest) {
    throw new TypeError('lifecycle settlement belongs to another receive operation')
  }
}

const DIRECT_TREE_PAUSE_STATES: ReadonlySet<ReceiveLifecycleState['kind']> = new Set([
  'resumable-receive',
  'partial-directory',
  'discarded',
  'needs-attention',
])

const DIRECT_ATOMIC_PAUSE_STATES: ReadonlySet<ReceiveLifecycleState['kind']> = new Set([
  'restart-required',
  'discarded',
  'needs-attention',
])

const WORKSPACE_RECEIVE_PAUSE_STATES: ReadonlySet<ReceiveLifecycleState['kind']> = new Set([
  'resumable-receive',
  'discarded',
  'needs-attention',
])

const WORKSPACE_POST_MATERIALIZATION_PAUSE_STATES: ReadonlySet<ReceiveLifecycleState['kind']> = new Set([
  ...WORKSPACE_RECEIVE_PAUSE_STATES,
  'resumable-package',
  'waiting-to-save',
  'download-started',
])

const PORTABLE_RECEIVE_PAUSE_STATES: ReadonlySet<ReceiveLifecycleState['kind']> = new Set([
  'restart-required',
  'discarded',
  'needs-attention',
])

const PORTABLE_POST_MATERIALIZATION_PAUSE_STATES: ReadonlySet<ReceiveLifecycleState['kind']> = new Set([
  ...PORTABLE_RECEIVE_PAUSE_STATES,
  'download-started',
])

const PREPARATION_REJECTION_STATES: ReadonlySet<ReceiveLifecycleState['kind']> = new Set([
  'discarded',
  'needs-attention',
])

const WORKSPACE_POST_MATERIALIZATION_STATES: ReadonlySet<ReceiveLifecycleState['kind']> = new Set([
  'materialization-sealed',
  'packaging',
  'resumable-package',
  'artifact-sealed',
  'waiting-to-save',
  'publishing-managed',
  'handing-off',
  'published',
  'download-started',
  'restart-required',
  'discarded',
  'expired',
  'needs-attention',
])

function failedTreeOutcome(
  state: ReceiveLifecycleState,
): 'partial-directory' | 'discarded' | undefined {
  if (state.kind === 'partial-directory' || state.kind === 'resumable-receive') {
    return 'partial-directory'
  }
  return state.kind === 'discarded' ? 'discarded' : undefined
}
