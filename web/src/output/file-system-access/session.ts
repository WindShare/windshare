import {
  createDestinationReservationID,
  createFSANamedEntryReservation,
  createOperationID,
  validateArtifactSpec,
  type DirectoryTreeArtifact,
  type FSANamedContainerEntryReservation,
  type ReceiveIntent,
} from '../../transfer/intent'
import {
  snapshotMaterializationRootRelativePath,
} from '../../transfer/job/coordinate/direct-tree'
import {
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../diagnostics'
import type { AcquiredFSAParentAuthority } from '../capability/contract'
import {
  PathComponentRejectedError,
  inspectFileSystemComponent,
} from '../browser/filesystem-component-inspection'
import { BrowserFileSystemTree } from '../browser/filesystem-tree'
import { IndexedDbCompatibleNameLedger } from '../browser/indexeddb-compatible-name-ledger'
import {
  verifyFSAOperationBinding,
  type FSAOperationBindingRepository,
  type PersistedFSAOperationBinding,
} from '../browser/indexeddb-root-binding'
import {
  type FSARootMutationLease,
} from '../browser/namespace-mutation'
import {
  fileCheckpointIsComplete,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import type { FinalFileCheckpointProof } from '../persistence/journal'
import { decideCollisionName } from '../planning'
import type {
  PersistentDirectoryMaterialization,
  PersistentFileRequest,
  PersistentFileTransactionPort,
  ActivatablePersistentMaterializationPort,
  PersistentDirectoryNamespaceClaim,
  PersistentOutputNamespaceClaimPort,
  PersistentTreeTraceEvent,
} from '../persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import { PersistentTreeOutputSession } from '../persistent-tree/session'
import {
  createPersistentOutputStageAuthority,
  type PersistentOutputStageAuthority,
  type PersistentOutputStageDiagnostics,
} from '../persistent-tree/stage-diagnostics'
import {
  openFSAFileCheckpointRepository,
  scanAllFSAFileCheckpoints,
  type FSAFileCheckpointRepository,
  type FSAFileCheckpointRepositoryFactory,
} from './checkpoint-repository'
import type {
  CompatibleNameActivationLedger,
  CompatibleNameCoordinator,
  CompatibleNameEmptyRepairRemoval,
  CompatibleNameRootRepairPreparationOptions,
  CompatibleNameRootRepairFactory,
  PreparedCompatibleNameRootRepair,
  CompatibleNameRepairProjectionSource,
} from './compatible-name/coordinator'
import { CompatibleNamePathAuthority } from './compatible-name/coordinator'
import type {
  CompatibleNamePendingTerminalOutcomeV1,
  CompatibleNameRepairSummary,
  CompatibleNameTerminalFooterState,
} from './compatible-name/model'
import {
  canonicalAuthorityRef,
  createFSAAuthorityReference,
  defaultCompatibleNamePreparation,
  emitFSAOutputTrace,
  outputTrace,
  reservationCreated,
} from './session-diagnostics'


export type {
  FSAFileCheckpointRepository,
  FSAFileCheckpointRepositoryFactory,
} from './checkpoint-repository'
export {
  FreshPageFileSystemAccessDiscardSession,
  openFreshPageFileSystemAccessDiscard,
  type FSAFreshPageDiscardCut,
  type OpenFreshPageFileSystemAccessDiscardOptions,
} from './fresh-page-discard-session'

const MAX_COLLISION_INDEX = 0xffff_ffff

export type FSAReservationTraceEvent =
  | Readonly<{
      name: 'receive.reservation.created'
      operation_id: string
      reservation_kind: 'named-container-entry'
      collision_index: number
      name_authority: 'application-chosen' | 'user-chosen'
      replacement_guarantee: 'coordinated-no-replace'
      delivery_mode: 'managed-target'
      commit_visibility: 'prefix-visible'
      rollback_guarantee: 'none'
    }>
  | Readonly<{
      name: 'receive.reservation.reopened'
      operation_id: string
      receive_intent_digest: string
      reservation_kind: 'named-container-entry'
    }>

export type FSAOutputTraceEvent = FSAReservationTraceEvent | PersistentTreeTraceEvent
export type FSAOutputTrace = (event: FSAOutputTraceEvent) => void

/**
 * The settlement observer deliberately exposes proofs instead of browser handles.
 * Its callback is serialized by, and cannot outlive, the operation's root mutation
 * lease, so lifecycle code can make one final ownership decision without a reopen gap.
 */
export interface FSAFinalSettlementObservation {
  persistCompatibleNamePendingOutcome(outcome: CompatibleNamePendingTerminalOutcomeV1): Promise<void>
  drainCompatibleNameProjector(
    state: CompatibleNameTerminalFooterState,
  ): Promise<CompatibleNameRepairSummary | undefined>
  clearCompatibleNamePendingOutcome(): Promise<void>
  removeVerifiedEmptyCompatibleNameRepair(): Promise<CompatibleNameEmptyRepairRemoval>
  beginEvidenceObservation(): void
  verifyOperationBinding(): Promise<void>
  verifyDirectory(path: readonly string[], ownedObjectId: string): Promise<void>
  verifyCheckpointFile(checkpoint: FileCheckpointV2): Promise<void>
  committedCheckpoints(): Promise<readonly FileCheckpointV2[]>
  candidateCheckpoints(): Promise<readonly FileCheckpointV2[]>
  finalCheckpointProof(recordId: string, generation: bigint): Promise<FinalFileCheckpointProof>
  retireCheckpoints(): Promise<void>
}

export interface ReserveNewFileSystemAccessOutputOptions {
  readonly authority: AcquiredFSAParentAuthority
  readonly artifact: DirectoryTreeArtifact
  readonly rootLease: FSARootMutationLease
  readonly operationId?: string
  readonly reservationId?: string
  readonly authorityRef?: string
  readonly prepareCompatibleNameRootRepair?: CompatibleNameRootRepairFactory
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly trace?: FSAOutputTrace
}

export interface ReservedFileSystemAccessOutput {
  readonly authority: AcquiredFSAParentAuthority
  readonly artifact: DirectoryTreeArtifact
  readonly operationId: string
  readonly reservation: FSANamedContainerEntryReservation
  readonly rootLease: FSARootMutationLease
  readonly compatibleNameRepair?: PreparedCompatibleNameRootRepair
}

export interface AssembleNewFileSystemAccessOutputOptions {
  readonly binding: PersistedFSAOperationBinding
  readonly operationRepository: FSAOperationBindingRepository
  readonly rootLease: FSARootMutationLease
  readonly compatibleNameCoordinator?: CompatibleNameCoordinator
  readonly openCompatibleNameLedger?: () => Promise<CompatibleNameActivationLedger>
  readonly compatibleNamePreparation?: CompatibleNameRootRepairPreparationOptions
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly stageDiagnostics?: PersistentOutputStageDiagnostics
  readonly trace?: FSAOutputTrace
}


export {
  openFileSystemAccessPendingOutcomeCatchUp,
  reopenFileSystemAccessOutput,
} from './session-reopen'
export type {
  FileSystemAccessPendingOutcomeCatchUpSession,
  OpenFileSystemAccessPendingOutcomeCatchUpOptions,
  ReopenFileSystemAccessOutputOptions,
} from './session-reopen'
export { createFSAAuthorityReference } from './session-diagnostics'

export class FileSystemAccessOutputSession implements
  ActivatablePersistentMaterializationPort, PersistentOutputNamespaceClaimPort {
  readonly intent: ReceiveIntent
  readonly reservation: FSANamedContainerEntryReservation
  readonly #materialization: PersistentTreeOutputSession
  readonly #tree: BrowserFileSystemTree
  readonly #binding: PersistedFSAOperationBinding
  readonly #operationRepository: FSAOperationBindingRepository
  readonly #checkpoints: FSAFileCheckpointRepository
  readonly #rootLease: FSARootMutationLease
  readonly #compatibleNames: CompatibleNamePathAuthority
  readonly #stageAuthority: PersistentOutputStageAuthority | undefined
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  #settlementStarted = false
  #evidenceObservationStarted = false
  #settlementObservationActive = false
  #activationPromise: Promise<void> | undefined
  #closePromise: Promise<void> | undefined

  constructor(input: Readonly<{
    intent: ReceiveIntent
    reservation: FSANamedContainerEntryReservation
    materialization: PersistentTreeOutputSession
    tree: BrowserFileSystemTree
    binding: PersistedFSAOperationBinding
    operationRepository: FSAOperationBindingRepository
    checkpoints: FSAFileCheckpointRepository
    rootLease: FSARootMutationLease
    compatibleNames: CompatibleNamePathAuthority
    stageAuthority?: PersistentOutputStageAuthority
    diagnostics?: OutputDiagnosticsPorts
  }>) {
    this.intent = input.intent
    this.reservation = input.reservation
    this.#materialization = input.materialization
    this.#tree = input.tree
    this.#binding = input.binding
    this.#operationRepository = input.operationRepository
    this.#checkpoints = input.checkpoints
    this.#rootLease = input.rootLease
    this.#compatibleNames = input.compatibleNames
    this.#stageAuthority = input.stageAuthority
    this.#diagnostics = input.diagnostics
  }

  async beginFile(request: PersistentFileRequest): Promise<PersistentFileTransactionPort> {
    this.#requireMaterializing()
    const transaction = await this.#materialization.beginFile(request)
    return compatibleNameFileTransaction(
      transaction,
      () => this.#compatibleNames.commitFinalFile(
        request.materializationRelativePath,
        transaction.ownedObjectId,
      ),
    )
  }

  ensureDirectory(
    path: readonly string[],
  ): Promise<PersistentDirectoryMaterialization> {
    this.#requireMaterializing()
    return this.#materialization.ensureDirectory(path)
  }

  bindDirectoryNamespace(claim: PersistentDirectoryNamespaceClaim): void {
    this.#requireMaterializing()
    this.#compatibleNames.bindDirectoryNamespace(claim)
  }

  activate(): Promise<void> {
    this.#requireMaterializing()
    this.#activationPromise ??= this.#activate()
    return this.#activationPromise
  }

  async #activate(): Promise<void> {
    await this.#materialization.activate()
    if (this.#compatibleNames.rootEntryKind !== 'directory') return
    const root = await this.#materialization.ensureDirectory(
      snapshotMaterializationRootRelativePath([]),
    )
    await this.#compatibleNames.commitVerifiedRootDirectory(root.ownedObjectId)
  }

  usesOperationRepository(repository: FSAOperationBindingRepository): boolean {
    return repository === this.#operationRepository
  }

  get repairProjection(): CompatibleNameRepairProjectionSource | undefined {
    return this.#compatibleNames.repairProjection
  }

  repairSummary(): CompatibleNameRepairSummary | undefined {
    return this.#compatibleNames.repairSummary()
  }

  subscribeRepairProjectionActivation(
    listener: (source: CompatibleNameRepairProjectionSource) => void,
  ): () => void {
    return this.#compatibleNames.subscribeRepairProjectionActivation(listener)
  }

  get compatibleNameRepairActive(): boolean {
    return this.#compatibleNames.active
  }

  async runFinalSettlement<T>(
    observe: (authority: FSAFinalSettlementObservation) => Promise<T>,
  ): Promise<T> {
    if (this.#settlementStarted || this.#closePromise !== undefined) {
      throw new DOMException('FSA materialization settlement already started', 'InvalidStateError')
    }
    this.#settlementStarted = true
    // Quiescing writers precedes the mutation barrier, while repository and Web Lock
    // resources remain live until the settlement cut explicitly closes the session.
    await this.#materialization.close()
    this.#settlementObservationActive = true
    try {
      const result = await this.#rootLease.authority.run(
        'settle-operation',
        () => observe(this.#observation()),
      )
      outputTrace(this.#diagnostics, { eventName: 'settlement', transition: 'completed' })
      return result
    } catch (error) {
      recordOutputException(this.#diagnostics?.failures?.settlement, error)
      outputTrace(this.#diagnostics, { eventName: 'settlement', transition: 'failed' })
      throw error
    } finally {
      this.#settlementObservationActive = false
    }
  }

  close(): Promise<void> {
    if (this.#settlementObservationActive) {
      return Promise.reject(new DOMException(
        'FSA materialization cannot close during final observation',
        'InvalidStateError',
      ))
    }
    this.#closePromise ??= this.#close()
    return this.#closePromise
  }

  async #close(): Promise<void> {
    const failures: unknown[] = []
    let outerFailureObserved = false
    try {
      await this.#materialization.close()
    } catch (error) {
      // File-transaction cleanup owns its native classification; this layer only
      // preserves that failure while releasing the remaining FSA authorities.
      failures.push(error)
    }
    try {
      this.#checkpoints.close()
    } catch (error) {
      failures.push(error)
      outerFailureObserved = true
      recordOutputException(this.#diagnostics?.failures?.cleanup, error)
    }
    try {
      this.#compatibleNames.close()
    } catch (error) {
      failures.push(error)
      outerFailureObserved = true
      recordOutputException(this.#diagnostics?.failures?.cleanup, error)
    }
    try {
      await this.#rootLease.release()
    } catch (error) {
      failures.push(error)
      outerFailureObserved = true
      recordOutputException(this.#diagnostics?.failures?.cleanup, error)
    }
    if (failures.length !== 0) {
      if (outerFailureObserved) {
        outputTrace(this.#diagnostics, { eventName: 'cleanup', transition: 'failed' })
      }
      if (failures.length === 1) throw failures[0]
      throw new AggregateError(failures, 'FSA output authorities did not close cleanly')
    }
    outputTrace(this.#diagnostics, { eventName: 'cleanup', transition: 'completed' })
  }

  #observation(): FSAFinalSettlementObservation {
    return Object.freeze({
      persistCompatibleNamePendingOutcome: (outcome: CompatibleNamePendingTerminalOutcomeV1) =>
        this.#compatibleNames.persistPendingTerminalOutcome(outcome),
      drainCompatibleNameProjector: (state: CompatibleNameTerminalFooterState) =>
        this.#compatibleNames.drainTerminalProjector(state),
      clearCompatibleNamePendingOutcome: () =>
        this.#compatibleNames.clearPendingTerminalOutcome(),
      removeVerifiedEmptyCompatibleNameRepair: () =>
        this.#compatibleNames.removeVerifiedEmptyRepairWithinMutation(this.#tree.ownedRoot()),
      beginEvidenceObservation: () => {
        if (this.#evidenceObservationStarted) {
          throw new DOMException('FSA settlement evidence observation already started', 'InvalidStateError')
        }
        this.#evidenceObservationStarted = true
        outputTrace(this.#diagnostics, { eventName: 'settlement', transition: 'started' })
      },
      verifyOperationBinding: async () => {
        await verifyFSAOperationBinding({
          repository: this.#operationRepository,
          intent: this.intent,
          expectedParent: this.#binding.parent,
          ...(this.#stageAuthority === undefined
            ? {}
            : { stageScope: this.#stageAuthority.bindingScope() }),
        })
      },
      verifyDirectory: async (path: readonly string[], ownedObjectId: string) => {
        if (!await this.#tree.validateDirectory(path, ownedObjectId)) {
          throw new TargetOwnershipUnknownError('settlement', this.intent.operationId)
        }
      },
      verifyCheckpointFile: (checkpoint: FileCheckpointV2) =>
        this.#verifyCheckpointFile(checkpoint),
      committedCheckpoints: () => scanAllFSAFileCheckpoints(this.#checkpoints, 'committed'),
      candidateCheckpoints: () => scanAllFSAFileCheckpoints(this.#checkpoints, 'candidates'),
      finalCheckpointProof: (recordId: string, generation: bigint) =>
        this.#checkpoints.finalCheckpointProof(recordId, generation),
      retireCheckpoints: () => this.#checkpoints.retireOperation(),
    })
  }

  async #verifyCheckpointFile(checkpoint: FileCheckpointV2): Promise<void> {
    const file = await this.#tree.openFile(checkpoint.canonicalPath, checkpoint.ownedObjectId)
    if (file === undefined) {
      throw new TargetOwnershipUnknownError('settlement', this.intent.operationId)
    }
    try {
      const size = await file.size()
      const durableEnd = checkpoint.verifiedRanges.at(-1)?.end ?? 0n
      const expectedSize = fileCheckpointIsComplete(checkpoint)
        ? checkpoint.exactSize
        : undefined
      if (size < durableEnd || size > checkpoint.exactSize ||
          (expectedSize !== undefined && size !== expectedSize)) {
        throw new TargetOwnershipUnknownError('settlement', this.intent.operationId)
      }
      await file.verify(fileCheckpointIsComplete(checkpoint) ? 'commit' : 'checkpoint')
    } finally {
      await file.close()
    }
  }

  #requireMaterializing(): void {
    if (this.#settlementStarted || this.#closePromise !== undefined) {
      throw new DOMException('FSA materialization is no longer mutable', 'InvalidStateError')
    }
  }
}

function compatibleNameFileTransaction(
  transaction: PersistentFileTransactionPort,
  commitMapping: () => Promise<void>,
): PersistentFileTransactionPort {
  return Object.freeze({
    revision: transaction.revision,
    ownedObjectId: transaction.ownedObjectId,
    get verifiedRanges() { return transaction.verifiedRanges },
    writeRange: (offset: bigint, data: Uint8Array, signal?: AbortSignal) =>
      transaction.writeRange(offset, data, signal),
    checkpoint: (signal?: AbortSignal) => transaction.checkpoint(signal),
    commit: async (signal?: AbortSignal) => {
      const proof = await transaction.commit(signal)
      // The final checkpoint proof is durable before the physical mapping becomes publishable.
      await commitMapping()
      return proof
    },
    close: () => transaction.close(),
  })
}

export async function reserveNewFileSystemAccessOutput(
  options: ReserveNewFileSystemAccessOutputOptions,
): Promise<ReservedFileSystemAccessOutput> {
  const artifact = await requireDirectoryTreeArtifact(options.artifact)
  const operationId = options.operationId ?? createOperationID()
  const reservationId = options.reservationId ?? createDestinationReservationID()
  const authorityRef = canonicalAuthorityRef(options.authorityRef ?? createFSAAuthorityReference())
  const selected = await options.rootLease.authority.run('reserve-name', async () => {
    const decision = await firstAvailableName(
      options.authority.parent,
      operationId,
      artifact,
      authorityRef,
      options.prepareCompatibleNameRootRepair,
    )
    const reservation = await createFSANamedEntryReservation({
      operationId,
      reservationId,
      artifact,
      authorityRef,
      logicalReservedName: decision.reservedName,
      physicalName: decision.physicalName,
      collisionIndex: decision.collisionIndex,
    })
    return Object.freeze({ reservation, compatibleNameRepair: decision.compatibleNameRepair })
  })
  return Object.freeze({
    authority: options.authority,
    artifact,
    operationId,
    reservation: selected.reservation,
    rootLease: options.rootLease,
    ...(selected.compatibleNameRepair === undefined
      ? {}
      : { compatibleNameRepair: selected.compatibleNameRepair }),
  })
}

export async function assembleNewFileSystemAccessOutput(
  options: AssembleNewFileSystemAccessOutputOptions,
): Promise<FileSystemAccessOutputSession> {
  const stageAuthority = createPersistentOutputStageAuthority(
    options.stageDiagnostics,
    {
      operationId: options.binding.intent.operationId,
      artifactId: options.binding.intent.artifact.digest,
    },
  )
  const binding = await verifyFSAOperationBinding({
    repository: options.operationRepository,
    intent: options.binding.intent,
    expectedParent: options.binding.parent,
    ...(stageAuthority === undefined
      ? {}
      : { stageScope: stageAuthority.bindingScope() }),
  })
  const compatibleNames = CompatibleNamePathAuthority.create({
    binding,
    mutations: options.rootLease.authority,
    pairHandles: options.operationRepository,
    openLedger: options.openCompatibleNameLedger ??
      (() => IndexedDbCompatibleNameLedger.open(options.databaseName)),
    preparation: options.compatibleNamePreparation ?? defaultCompatibleNamePreparation(),
    ...(options.compatibleNameCoordinator === undefined
      ? {}
      : { coordinator: options.compatibleNameCoordinator }),
  })
  let checkpoints: FSAFileCheckpointRepository | undefined
  try {
    checkpoints = await openFSAFileCheckpointRepository(
      options,
      binding.intent,
      binding.reservation,
    )
    emitFSAOutputTrace(options.trace, reservationCreated(binding.reservation))
    outputTrace(options.diagnostics, { eventName: 'output_reservation', transition: 'acquired' })
    const tree = new BrowserFileSystemTree({
      binding,
      operationRepository: options.operationRepository,
      fileHandles: checkpoints,
      mutations: options.rootLease.authority,
      compatibleNames,
      ...(stageAuthority === undefined ? {} : { stageAuthority }),
    })
    const materialization = await PersistentTreeOutputSession.createNew({
      tree,
      checkpoints,
      ...(options.diagnostics === undefined
        ? {}
        : { diagnostics: options.diagnostics }),
      ...(stageAuthority === undefined ? {} : { stageAuthority }),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
    })
    return new FileSystemAccessOutputSession({
      intent: binding.intent,
      reservation: binding.reservation,
      materialization,
      tree,
      binding,
      operationRepository: options.operationRepository,
      checkpoints,
      rootLease: options.rootLease,
      compatibleNames,
      ...(stageAuthority === undefined ? {} : { stageAuthority }),
      ...(options.diagnostics === undefined
        ? {}
        : { diagnostics: options.diagnostics }),
    })
  } catch (error) {
    recordOutputException(options.diagnostics?.failures?.outputReservation, error)
    outputTrace(options.diagnostics, { eventName: 'output_reservation', transition: 'failed' })
    const cleanupFailure = closeFailedFSAAssembly(
      checkpoints,
      compatibleNames,
      options.diagnostics,
    )
    if (cleanupFailure !== undefined) {
      throw new AggregateError(
        [error, cleanupFailure],
        'FSA output assembly failed and could not close its checkpoint repository',
        { cause: error },
      )
    }
    throw error
  }
}

function closeFailedFSAAssembly(
  checkpoints: FSAFileCheckpointRepository | undefined,
  compatibleNames: CompatibleNamePathAuthority,
  diagnostics: OutputDiagnosticsPorts | undefined,
): unknown {
  const failures: unknown[] = []
  for (const close of [
    () => checkpoints?.close(),
    () => compatibleNames.close(),
  ]) {
    try {
      close()
    } catch (error) {
      failures.push(error)
      recordOutputException(diagnostics?.failures?.cleanup, error)
    }
  }
  if (failures.length === 0) return undefined
  if (failures.length === 1) return failures[0]
  return new AggregateError(
    failures,
    'FSA assembly cleanup could not close all compatible-name authorities',
  )
}

async function requireDirectoryTreeArtifact(
  input: DirectoryTreeArtifact,
): Promise<DirectoryTreeArtifact> {
  const artifact = await validateArtifactSpec(input)
  if (artifact.kind !== 'directory-tree' || artifact.layout.kind === 'catalog-root') {
    throw new TypeError('Browser FSA requires a named DirectoryTree layout')
  }
  return artifact
}

async function firstAvailableName(
  parent: FileSystemDirectoryHandle,
  operationId: string,
  artifact: DirectoryTreeArtifact,
  authorityRef: string,
  prepareCompatibleNameRootRepair: CompatibleNameRootRepairFactory | undefined,
): Promise<{
  readonly requestedName: string
  readonly reservedName: string
  readonly physicalName: string
  readonly collisionIndex: number
  readonly compatibleNameRepair?: PreparedCompatibleNameRootRepair
}> {
  for (let collisionIndex = 0; collisionIndex <= MAX_COLLISION_INDEX; collisionIndex += 1) {
    const decision = await decideCollisionName(operationId, artifact, collisionIndex)
    try {
      if (await inspectFileSystemComponent({
        verifiedParent: parent,
        component: decision.reservedName,
        expectedKind: decision.entryKind,
        stage: 'fsa.root.entry.inspect',
        mode: 'classify-rejection',
      }) === 'occupied') continue
      return Object.freeze({
        requestedName: decision.requestedName,
        reservedName: decision.reservedName,
        physicalName: decision.reservedName,
        collisionIndex: decision.collisionIndex,
      })
    } catch (error) {
      if (!(error instanceof PathComponentRejectedError) ||
          prepareCompatibleNameRootRepair === undefined) {
        throw error
      }
      const compatibleNameRepair = await prepareCompatibleNameRootRepair({
        rejection: error,
        parent,
        operationId,
        authorityRef,
        logicalReservedName: decision.reservedName,
        entryKind: decision.entryKind,
      })
      return Object.freeze({
        requestedName: decision.requestedName,
        reservedName: decision.reservedName,
        physicalName: compatibleNameRepair.bootstrap.initialMapping.physicalComponent,
        collisionIndex: decision.collisionIndex,
        compatibleNameRepair,
      })
    }
  }
  throw new DOMException('The FSA collision namespace is exhausted', 'InvalidStateError')
}
