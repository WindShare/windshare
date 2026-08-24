import type { V2CommittedDirectory } from '../catalog/v2-page-store'
import type { V2CatalogEntry } from '../catalog/v2-records'
import type { V2FrozenSelectionPolicy } from '../catalog/v2-selection'
import { encodeBase64Url } from '../crypto/bytes'
import {
  DirectorySettlementKind,
  snapshotMaterializationDirectory,
  validateDirectoryAdmissionBinding,
  type DirectoryAdmission,
  type DirectoryAdmissionScope,
} from './directory-admission'
import {
  V2OutputPausedError,
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
import { finalizeV2Directories } from './job/directory-transfer'
import {
  isolatedDirectoryOutputFailure,
  normalizeV2FileTransferFailure,
} from './job/failures'
import { V2TransferAdmissionFailureError } from './job/admission-error'
import {
  createAuthenticatedLogicalSiblingMembership,
  V2JobDiscovery,
} from './job/discovery'
import { transferV2File } from './job/file-transfer'
import {
  createDirectTreeCoordinateContract,
  sameMaterializationRootRelativePath,
  snapshotMaterializationRootRelativePath,
  snapshotSourceAuthenticationPath,
  type DirectTreeCoordinateContract,
  type DirectTreeDirectoryProjection,
  type DirectTreeFileProjection,
} from './job/coordinate/direct-tree'
import {
  createTransferJobId,
  snapshotTransferJobId,
  validateTransferJobIntent,
} from './job/identity'
import { transferJobLimits, type TransferJobLimits } from './job/limits'
import { V2TransferObservers } from './job/observers'
import { ExactPreparationCollector } from './job/preparation'
import { AsyncBoundedQueue } from './job/scheduler'
import { collectExactSingleFileEvidence } from './job/single-file-evidence'
import { V2JobRootAuthority } from './job/root'
import { V2CatalogTraversalGuard } from './job/traversal'
import {
  newFileQueue,
  runDiscoveryWorkers,
  runPreparedFileWorkers,
} from './job/workers'
import { SelectionMeasureTracker, type SelectionMeasure } from './measure'
import {
  snapshotDirectoryMaterializationRequest,
  type DirectTreeExecution,
  type DirectResumableZipExecution,
  type ExactPreparationEvidence,
  type IncrementalDirectoryOutput,
  type MaterializationSummary,
  type PlanExecution,
} from './output-session'
import type { DirectZipIntent } from './direct-zip'
import { V2TransferProgressLedger } from './progress/v2-ledger'
import { runDirectZipJob } from './v2-job-direct-zip'
import { V2JobFailureAuthority } from './v2-job-failure-authority'
import { TransferJobMaterialization } from './v2-job-materialization'
import { outputSettlementTimeoutMilliseconds } from './settlement/v2-output'
import { TransferJobSettlement } from './v2-job-settlement'
import { V2RevisionCapacityCoordinator } from './revision-capacity/public'

export * from './job/public'
export * from './revision-capacity/public'


/**
 * Tree and atomic plans retain bounded discovery/content overlap. Direct ZIP uses
 * a separate serial coordinator, while prepared plans freeze exact authenticated
 * catalog evidence before their output port becomes reachable.
 */
export class TransferJob {
  readonly #options: TransferJobOptions
  readonly #selection: V2FrozenSelectionPolicy
  readonly #limits: TransferJobLimits
  readonly #lifetime = new AbortController()
  readonly #measure = new SelectionMeasureTracker()
  readonly #failures: V2JobFailureAuthority
  readonly #traversal: V2CatalogTraversalGuard
  readonly #discovery: V2JobDiscovery
  readonly #root: V2JobRootAuthority
  readonly #materialization: TransferJobMaterialization
  readonly #settlement: TransferJobSettlement
  readonly #finalizableDirectories: DirectoryAdmission[] = []
  readonly #materializedDirectoryPaths = new Set<string>()
  readonly #transferJobId: string
  readonly #outputSettlementTimeoutMilliseconds: number
  readonly #progress = new V2TransferProgressLedger()
  readonly #capacity: V2RevisionCapacityCoordinator
  #directoryAdmissionClaims = 0
  #externalAbortCleanup: (() => void) | undefined
  #directoryScope: DirectoryAdmissionScope | undefined
  #directoryOutput: IncrementalDirectoryOutput | undefined
  #directTreeCoordinates: DirectTreeCoordinateContract | undefined
  #execution: PlanExecution | undefined
  #intent: TransferJobOptions['intent'] | undefined
  #observers: V2TransferObservers | undefined
  #preparation: ExactPreparationEvidence | undefined
  #externalCancellationRequested = false
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
    this.#capacity = new V2RevisionCapacityCoordinator({
      revisions: options.revisions,
      broker: options.broker,
    }, {
      ...options.revisionCapacity,
      onProgress: snapshot => {
        this.#progress.observeCapacityWait(snapshot)
        this.#emitProgress()
      },
      onTrace: event => this.#observers?.capacityWait(event),
    })
    this.#failures = new V2JobFailureAuthority({
      selection: this.#selection,
      signal: this.#lifetime.signal,
      measure: this.#measure,
      progress: this.#progress,
      observers: () => this.#observers,
      emitProgress: () => this.#emitProgress(),
      ...(options.incidentScope === undefined ? {} : { incidentScope: options.incidentScope }),
    })
    this.#root = new V2JobRootAuthority(options, this.#selection, this.#lifetime.signal)
    this.#traversal = new V2CatalogTraversalGuard(
      options.descriptor.shareInstance,
      this.#limits.catalogNodeClaims,
    )
    this.#discovery = new V2JobDiscovery({
      catalog: options.catalog,
      selection: this.#selection,
      traversal: this.#traversal,
      explicitTargets: this.#failures.explicitTargets,
      signal: this.#lifetime.signal,
      rootCommitted: () => this.#root.committed,
      observeSelectedFile: (entry) => {
        const measure = this.#measure.observeUniqueFile(entry.expectedSize)
        this.#observers?.measure(measure)
        this.#emitProgress()
      },
      recordDirectoryFailure: (identity, error) => this.#recordDirectoryFailure(identity, error),
      authenticateDirectory: (cursor, committed, parent) =>
        this.#authenticateDirectory(cursor, committed, parent),
      projectFile: sourcePath => this.#projectFile(sourcePath),
      prepareDirectory: (collector, cursor, committed, role) =>
        this.#preparedDirectory(collector, cursor, committed, role),
    })
    this.#settlement = new TransferJobSettlement({
      options,
      lifetime: this.#lifetime,
      measure: this.#measure,
      failures: this.#failures,
      progress: this.#progress,
      transferJobId: this.#transferJobId,
      outputSettlementTimeoutMilliseconds: this.#outputSettlementTimeoutMilliseconds,
      intent: () => this.#requireIntent(),
      execution: () => this.#execution,
      preparation: () => this.#preparation,
      observers: () => this.#observers,
      externalCancellationRequested: () => this.#externalCancellationRequested,
      materializationSummary: () => this.#materializationSummary(),
      emitProgress: () => this.#emitProgress(),
    })
    this.#materialization = new TransferJobMaterialization({
      signal: this.#lifetime.signal,
      admission: options.plans,
      root: {
        load: () => this.#root.load(),
        cursor: () => this.#root.cursor(),
        authenticated: committed => this.#root.authenticated(committed),
        direct: () => this.#root.direct(
          (cursor, committed) => this.#authenticateDirectory(cursor, committed),
        ),
        singleFileEvidence: (intent, committed) => collectExactSingleFileEvidence({
          catalog: this.#options.catalog,
          descriptor: this.#options.descriptor,
          selection: this.#selection,
          artifact: intent.artifact,
          signal: this.#lifetime.signal,
          root: committed,
        }),
      },
      discovery: {
        createDirectFileQueue: () => newFileQueue(this.#limits),
        run: (root, directFiles, collector) => this.#runDiscovery(root, directFiles, collector),
        finish: () => this.#finishDiscovery(),
        hasFailures: () => this.#failures.failureCount !== 0,
        prepareDirectory: (collector, cursor, committed, role) =>
          this.#preparedDirectory(collector, cursor, committed, role),
      },
      execution: {
        bind: execution => {
          this.#execution = this.#stabilizeDirectTreeLifecycle(execution)
        },
        bindDirectoryOutput: output => {
          this.#directoryOutput = output
        },
        bindDirectoryScope: scope => {
          this.#directoryScope = scope
        },
        recordPreparation: evidence => {
          this.#preparation = evidence
        },
        requireBound: () => this.#requireExecution(),
        materializationStarted: () => this.#observers?.materializationStarted(),
        finalizeDirectories: () => this.#finalizeDirectories(),
        transferPreparedFiles: files => this.#transferPreparedFiles(files),
        completeWorkers: measure => this.#settlement.completeWorkers(measure),
        settleIncompletePreparation: measure => this.#settlement.settleIncompletePreparation(measure),
        preparationRejected: state => this.#settlement.preparationRejected(state),
      },
      directZip: {
        run: execution => this.#runDirectZip(execution),
      },
    })
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
      this.#directTreeCoordinates = intent.plan.kind === 'direct-tree'
        ? await createDirectTreeCoordinateContract(intent)
        : undefined
      this.#observers = new V2TransferObservers({
        intent,
        transferJobId: this.#transferJobId,
        ...(this.#options.protocol === undefined ? {} : { protocol: this.#options.protocol }),
        lanes: this.#options.lanes,
        ...(this.#options.incidentScope === undefined
          ? {}
          : { incidentScope: this.#options.incidentScope }),
        ...(this.#options.onProgress === undefined ? {} : { onProgress: this.#options.onProgress }),
        ...(this.#options.onMeasure === undefined ? {} : { onMeasure: this.#options.onMeasure }),
        ...(this.#options.trace === undefined ? {} : { trace: this.#options.trace }),
      })
      this.#observers.intentFrozen(artifactLayoutClass(intent))
      this.#emitProgress()
      return await this.#materialization.run(intent)
    } catch (error) {
      if (this.#intent === undefined) throw this.#admissionFailure(error)
      if (this.#execution?.planKind === 'direct-tree' &&
          this.#execution.terminalSettlementInitiated?.() === true) {
        // The DirectTree lifecycle owner already crossed its terminal cut. Re-entering
        // Pause would replace the initiating projector/finalization failure and could
        // publish a second ordinary lifecycle over a retained pending outcome.
        throw error
      }
      return this.#settlement.settleRunFailure(error)
    } finally {
      this.#externalAbortCleanup?.()
    }
  }

  async #runDiscovery(
    root: DirectoryWork,
    directFiles?: AsyncBoundedQueue<PendingFile>,
    collector?: ExactPreparationCollector,
  ): Promise<void> {
    try {
      await runDiscoveryWorkers({
        root,
        ...(directFiles === undefined ? {} : { directFiles }),
        limits: this.#limits,
        signal: this.#lifetime.signal,
        abort: (error) => {
          if (!this.#lifetime.signal.aborted) this.#lifetime.abort(error)
        },
        claimRoot: ({ cursor }) => this.#traversal.claimNode(cursor.id),
        discoverDirectory: (work, files) => this.#discovery.discoverDirectory(work, files, collector),
        recordDirectoryFailure: (identity, error) => this.#recordDirectoryFailure(identity, error),
        transferFile: (file) => this.#transferFile(file),
        recordFileFailure: (file, error) => this.#recordFileFailure(file.entry, error),
        observeConsequenceFailure: failure => this.#observers?.workerConsequence(
          'discovery',
          failure,
          this.#execution?.output.identity.outputSessionId,
        ),
      })
    } catch (error) {
      if (!this.#lifetime.signal.aborted) this.#lifetime.abort(error)
      throw error
    }
  }

  #stabilizeDirectTreeLifecycle(execution: PlanExecution): PlanExecution {
    if (execution.planKind !== 'direct-tree') return execution
    const directExecution: DirectTreeExecution = execution
    return Object.freeze({
      ...directExecution,
      pause: async (
        request: Parameters<DirectTreeExecution['pause']>[0],
        signal: AbortSignal,
      ) => {
        if (this.#externalCancellationRequested) await this.#finalizeDirectories(signal)
        return directExecution.pause(request, signal)
      },
      ...(directExecution.stop === undefined ? {} : {
        stop: async (
          request: Parameters<NonNullable<DirectTreeExecution['stop']>>[0],
          signal: AbortSignal,
        ) => {
          if (this.#externalCancellationRequested) await this.#finalizeDirectories(signal)
          return directExecution.stop!(request, signal)
        },
      }),
    })
  }

  async #runDirectZip(execution: DirectResumableZipExecution): Promise<SelectionMeasure> {
    const intent = this.#requireIntent()
    if (intent.plan.kind !== 'direct-resumable-zip' || intent.artifact.kind !== 'zip-archive') {
      throw new TypeError('ordered direct ZIP run lost its frozen operation binding')
    }
    return runDirectZipJob({
      catalog: this.#options.catalog,
      descriptor: this.#options.descriptor,
      selection: this.#selection,
      intent: intent as DirectZipIntent,
      revisions: this.#capacity.revisions,
      broker: this.#capacity.broker,
      execution,
      maximumNodeClaims: this.#limits.catalogNodeClaims,
      signal: this.#lifetime.signal,
      observeSelectedFile: exactSize => {
        const measure = this.#measure.observeUniqueFile(exactSize)
        this.#observers?.measure(measure)
        this.#emitProgress()
      },
      observeReplayedFile: exactSize => {
        this.#progress.completeFile(exactSize)
        this.#emitProgress()
      },
      acknowledgeWrite: bytes => {
        this.#progress.acknowledgeWrite(bytes)
        this.#emitProgress()
      },
      completeFile: exactSize => {
        this.#progress.completeFile(exactSize)
        this.#emitProgress()
      },
      finishMeasure: () => {
        const measure = this.#measure.complete()
        this.#observers?.measure(measure)
        this.#emitProgress()
        return measure
      },
    })
  }

  async #authenticateDirectory(
    cursor: DirectoryCursor,
    committed: V2CommittedDirectory,
    parent?: AuthenticatedDirectory,
  ): Promise<AuthenticatedDirectory> {
    const coordinates = this.#directTreeCoordinates
    if (coordinates === undefined) return this.#authenticatedReference(cursor, committed)
    const projection = coordinates.projectDirectory(cursor.path)
    return projection.kind === 'reference'
      ? this.#authenticatedReference(cursor, committed, projection)
      : this.#admitDirectory(cursor, committed, projection, parent)
  }

  async #admitDirectory(
    cursor: DirectoryCursor,
    committed: V2CommittedDirectory,
    projection: Extract<DirectTreeDirectoryProjection, { kind: 'materialize' }>,
    parent?: AuthenticatedDirectory,
  ): Promise<AuthenticatedDirectory> {
    this.#reserveDirectoryAdmission()
    const output = this.#directoryOutput
    const scope = this.#directoryScope
    const execution = this.#execution
    if (output === undefined || scope === undefined || execution === undefined) {
      throw new V2OutputPausedError('incremental directory authority is unavailable')
    }
    const root = this.#requireDirectTreeCoordinates().rootExpectation
    const isExpectedRoot = root.kind === 'materialized-directory' &&
      root.directoryId === cursor.idText &&
      sameMaterializationRootRelativePath(root.relativePath, projection.relativePath)
    if (!isExpectedRoot && parent?.kind !== 'materialized') {
      throw new V2OutputPausedError('child directory lacks its authenticated parent receipt')
    }
    if (isExpectedRoot && parent?.kind === 'materialized') {
      throw new V2OutputPausedError('materialized root unexpectedly inherited a parent receipt')
    }
    const directory = snapshotMaterializationDirectory({
      directoryId: cursor.idText,
      generation: encodeBase64Url(committed.generation),
      path: projection.relativePath,
      ...(!isExpectedRoot && parent?.kind === 'materialized'
        ? { parentAdmission: parent.admission }
        : {}),
      ...(cursor.modifiedTime === undefined ? {} : { modifiedTime: cursor.modifiedTime }),
    })
    const request = snapshotDirectoryMaterializationRequest({
      directory,
      sourceAuthenticationPath: projection.sourceAuthenticationPath,
      logicalArtifactPath: projection.logicalArtifactPath,
      logicalSiblingMembership: createAuthenticatedLogicalSiblingMembership(
        this.#options.catalog,
        committed,
        this.#lifetime.signal,
      ),
    })
    let returned: DirectoryAdmission
    try {
      returned = await output.admitDirectory(request, this.#lifetime.signal)
    } catch (error) {
      const isolated = isolatedDirectoryOutputFailure(
        error,
        execution.output.capabilities.fileFailureIsolation,
        cursor.idText,
        this.#options.incidentScope,
      )
      throw isolated ?? error
    }
    const admission = validateDirectoryAdmissionBinding(scope, directory, returned)
    this.#finalizableDirectories.push(admission)
    if (projection.relativePath.length > 0) {
      this.#materializedDirectoryPaths.add(projection.relativePath.join('/'))
    }
    this.#observers?.directoryAdmitted({
      admittedDirectoryCount: BigInt(this.#directoryAdmissionClaims),
      layoutClass: scope.layout,
    })
    return Object.freeze({
      kind: 'materialized',
      directoryId: cursor.idText,
      generation: encodeBase64Url(committed.generation),
      sourceAuthenticationPath: projection.sourceAuthenticationPath,
      logicalArtifactPath: projection.logicalArtifactPath,
      materializationRelativePath: projection.relativePath,
      admission,
      ...(directory.modifiedTime === undefined ? {} : { modifiedTime: directory.modifiedTime }),
    })
  }

  #authenticatedReference(
    cursor: DirectoryCursor,
    committed: V2CommittedDirectory,
    projection?: Extract<DirectTreeDirectoryProjection, { kind: 'reference' }>,
  ): AuthenticatedDirectory {
    const sourceAuthenticationPath = projection?.sourceAuthenticationPath ??
      snapshotSourceAuthenticationPath(cursor.path)
    const logicalArtifactPath = projection?.logicalArtifactPath ??
      artifactDirectoryPath(this.#requireIntent(), cursor.path)
    return Object.freeze({
      kind: 'reference',
      directoryId: cursor.idText,
      generation: encodeBase64Url(committed.generation),
      sourceAuthenticationPath,
      logicalArtifactPath,
      ...(cursor.modifiedTime === undefined ? {} : { modifiedTime: cursor.modifiedTime }),
    })
  }

  #projectFile(sourcePath: readonly string[]): DirectTreeFileProjection {
    const coordinates = this.#directTreeCoordinates
    if (coordinates !== undefined) return coordinates.projectFile(sourcePath)
    const sourceAuthenticationPath = snapshotSourceAuthenticationPath(sourcePath)
    const logicalArtifactPath = artifactFilePath(this.#requireIntent(), sourceAuthenticationPath)
    return Object.freeze({
      sourceAuthenticationPath,
      logicalArtifactPath,
      relativePath: snapshotMaterializationRootRelativePath(logicalArtifactPath),
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
      ? collector.referenceDirectory(cursor, committed, artifactPath)
      : collector.materializeDirectory(cursor, committed, artifactPath, role)
  }

  async #finalizeDirectories(signal: AbortSignal = this.#lifetime.signal): Promise<void> {
    const output = this.#directoryOutput
    if (output === undefined) return
    await finalizeV2Directories({
      admissions: this.#finalizableDirectories,
      output,
      signal,
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
              this.#options.incidentScope,
            )
        if (isolated === undefined) throw error
        this.#recordDirectoryFailure(admission.directoryId, isolated)
      },
    })
  }

  async #transferPreparedFiles(files: readonly PendingFile[]): Promise<void> {
    try {
      await runPreparedFileWorkers({
        files,
        limits: this.#limits,
        signal: this.#lifetime.signal,
        abort: (error) => {
          if (!this.#lifetime.signal.aborted) this.#lifetime.abort(error)
        },
        transferFile: (file) => this.#transferFile(file),
        recordFileFailure: (file, error) => this.#recordFileFailure(file.entry, error),
        observeConsequenceFailure: failure => this.#observers?.workerConsequence(
          'prepared-files',
          failure,
          this.#execution?.output.identity.outputSessionId,
        ),
      })
    } catch (error) {
      if (!this.#lifetime.signal.aborted) this.#lifetime.abort(error)
      throw error
    }
  }

  async #transferFile(file: PendingFile): Promise<void> {
    const execution = this.#requireExecution()
    if (execution.planKind === 'direct-resumable-zip') {
      throw new TypeError('direct ZIP content must pass through its ordered coordinator')
    }
    await transferV2File({
      descriptor: this.#options.descriptor,
      revisions: this.#capacity.revisions,
      broker: this.#capacity.broker,
      output: execution.output,
      ...(execution.planKind === 'direct-tree'
        ? { directTreeCoordinates: this.#requireDirectTreeCoordinates() }
        : {}),
      signal: this.#lifetime.signal,
      outputSettlementTimeoutMilliseconds: this.#outputSettlementTimeoutMilliseconds,
      ...(this.#options.incidentScope === undefined
        ? {}
        : { incidentScope: this.#options.incidentScope }),
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

  #admissionFailure(error: unknown): V2TransferAdmissionFailureError {
    const normalized = normalizeV2FileTransferFailure(error, {
      stage: 'authority_selection',
      ...(this.#externalCancellationRequested ? { signal: this.#lifetime.signal } : {}),
      ...(this.#options.incidentScope === undefined
        ? {}
        : { incidentScope: this.#options.incidentScope }),
    })
    return new V2TransferAdmissionFailureError(normalized.kind === 'canceled'
      ? Object.freeze({ kind: 'canceled' })
      : Object.freeze({
          kind: 'fault',
          classification: normalized.diagnostic.classification,
        }))
  }

  #reserveDirectoryAdmission(): void {
    if (this.#directoryAdmissionClaims >= this.#limits.directoryAdmissions) {
      throw new V2OutputPausedError('Directory admission budget was exhausted')
    }
    this.#directoryAdmissionClaims += 1
  }

  #recordDirectoryFailure(identity: string, reason: unknown): void {
    this.#failures.recordDirectory(identity, reason)
  }

  #recordFileFailure(entry: Extract<V2CatalogEntry, { kind: 'file' }>, reason: unknown): void {
    this.#failures.recordFile(entry, reason)
  }

  #finishDiscovery(): SelectionMeasure {
    return this.#failures.finishDiscovery()
  }

  #materializationSummary(): MaterializationSummary {
    if (this.#execution?.planKind === 'direct-resumable-zip') {
      return this.#execution.ordered.materializationSummary()
    }
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

  #requireDirectTreeCoordinates(): DirectTreeCoordinateContract {
    if (this.#directTreeCoordinates === undefined) {
      throw new Error('DirectTree coordinate contract is unavailable')
    }
    return this.#directTreeCoordinates
  }

  #requireExecution(): PlanExecution {
    if (this.#execution === undefined) throw new Error('plan execution is unavailable')
    return this.#execution
  }

  #observeAbort(signal?: AbortSignal): void {
    if (signal === undefined) return
    const abort = () => {
      if (this.#lifetime.signal.aborted) return
      this.#externalCancellationRequested = true
      this.#lifetime.abort(signal.reason ?? new DOMException('Transfer aborted', 'AbortError'))
    }
    signal.addEventListener('abort', abort, { once: true })
    this.#externalAbortCleanup = () => signal.removeEventListener('abort', abort)
    if (signal.aborted) abort()
  }
}
