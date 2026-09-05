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
  observePerformance,
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
import type {
  FSATerminalDrain,
  FSATerminalExclusiveAuthority,
  FSATerminalMutationKind,
} from '../browser/mutation-coordination/model'
import { decideCollisionName } from '../planning'
import type {
  PersistentDirectoryMaterialization,
  PersistentDirectoryLedgerMaterialization,
  PersistentDirectoryLedgerRequest,
  PersistentFileRequest,
  PersistentFileTransactionPort,
  ActivatablePersistentMaterializationPort,
  PersistentDirectoryNamespaceClaim,
  PersistentOutputNamespaceClaimPort,
  PersistentTreeTraceEvent,
} from '../persistent-tree/contracts'
import type {
  MaterializationDirectoryAdmittedEntryV1,
  MaterializationDirectoryFinalization,
  MaterializationDirectoryFinalizedEntryV1,
} from '../materialization-ledger/model'
import {
  type MaterializationLedgerSealPurpose,
  type MaterializationLedgerSealV1,
} from '../materialization-ledger/model'
import { createMaterializationLedgerBinding } from '../materialization-ledger/codec'
import { PersistentTreeOutputSession } from '../persistent-tree/session'
import {
  createPersistentOutputStageAuthority,
  type PersistentOutputStageAuthority,
  type PersistentOutputStageDiagnostics,
} from '../persistent-tree/stage-diagnostics'
import {
  openFSAFileCheckpointRepository,
  type FSAFileCheckpointRepository,
  type FSAFileCheckpointRepositoryFactory,
  type FSASemanticOutputRepository,
} from './checkpoint-repository'
import type { PersistentDirectTreeMaterializationEvidence } from '../../transfer/settlement/persistent-execution'
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
import { compatibleNameFileTransaction } from './compatible-name/file-transaction'
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
import {
  FSASettlementLedgerAuthority,
  type FSAResumableCheckpointEvidence,
} from './settlement-ledger'
import { closeFailedFSAAssembly } from './assembly-cleanup'


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

export interface FSAFinalSettlementObservation {
  persistCompatibleNamePendingOutcome(outcome: CompatibleNamePendingTerminalOutcomeV1): Promise<void>
  drainCompatibleNameProjector(
    state: CompatibleNameTerminalFooterState,
  ): Promise<CompatibleNameRepairSummary | undefined>
  clearCompatibleNamePendingOutcome(): Promise<void>
  removeVerifiedEmptyCompatibleNameRepair(): Promise<CompatibleNameEmptyRepairRemoval>
  verifyOperationBinding(): Promise<void>
  sealMaterializationLedger(input: Readonly<{
    evidence: PersistentDirectTreeMaterializationEvidence
    sealSequence: bigint
    purpose: MaterializationLedgerSealPurpose
  }>): Promise<MaterializationLedgerSealV1>
  resumableCheckpointEvidence(): Promise<FSAResumableCheckpointEvidence>
  retireRecoveryMetadata(): Promise<void>
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
  readonly maximumConcurrentInitialClaimInspections?: number
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
  openFileSystemAccessCompatibleNameCatchUp,
  reopenFileSystemAccessOutput,
} from './session-reopen'
export type {
  FileSystemAccessCompatibleNameCatchUpSession,
  OpenFileSystemAccessCompatibleNameCatchUpOptions,
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
  readonly #settlementLedger: FSASettlementLedgerAuthority
  #settlementStarted = false
  #settlementObservationActive = false
  #terminalDrain: FSATerminalDrain | undefined
  #activationPromise: Promise<void> | undefined
  #outputClosePromise: Promise<void> | undefined
  #rootReleasePromise: Promise<void> | undefined
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
    const ledgerBinding = createMaterializationLedgerBinding({
      operationId: input.checkpoints.binding.operationId,
      receiveIntentDigest: input.checkpoints.binding.receiveIntentDigest,
      materializationBindingDigest: input.checkpoints.binding.materializationBindingDigest,
      authorityRef: input.checkpoints.binding.authorityRef,
    })
    this.#settlementLedger = new FSASettlementLedgerAuthority({
      intent: input.intent,
      checkpoints: input.checkpoints,
      binding: ledgerBinding,
      ...(input.diagnostics?.performance === undefined
        ? {}
        : { performance: input.diagnostics.performance }),
    })
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

  materializeDirectory(
    request: PersistentDirectoryLedgerRequest,
  ): Promise<PersistentDirectoryLedgerMaterialization> {
    this.#requireMaterializing()
    const materialize = this.#materialization.materializeDirectory
    if (materialize === undefined) {
      throw new DOMException(
        'DirectTree materialization requires durable directory-ledger authority',
        'InvalidStateError',
      )
    }
    return materialize.call(this.#materialization, request)
  }

  finalizeDirectory(
    admission: MaterializationDirectoryAdmittedEntryV1,
    outcome: MaterializationDirectoryFinalization,
  ): Promise<MaterializationDirectoryFinalizedEntryV1> {
    this.#requireDirectoryFinalizable()
    const finalize = this.#materialization.finalizeDirectory
    if (finalize === undefined) {
      throw new DOMException(
        'DirectTree materialization requires durable directory-ledger authority',
        'InvalidStateError',
      )
    }
    return finalize.call(this.#materialization, admission, outcome)
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

  beginTerminal(kind: FSATerminalMutationKind): void {
    if (this.#terminalDrain !== undefined) {
      if (this.#terminalDrain.kind !== kind) {
        throw new DOMException('FSA terminal cut kind cannot change', 'InvalidStateError')
      }
      return
    }
    if (this.#settlementStarted || this.#outputClosePromise !== undefined ||
        this.#rootReleasePromise !== undefined) {
      throw new DOMException('FSA terminal cut is unavailable after shutdown', 'InvalidStateError')
    }
    // This method intentionally performs no await: cancellation callers close native admission
    // before they finish already-admitted directory descendants.
    this.#terminalDrain = this.#rootLease.scheduler.beginTerminal(kind)
    if (kind === 'settle-operation') {
      observePerformance(this.#diagnostics?.performance, summary =>
        summary.markMilestone('settlement_started'))
    }
    outputTrace(this.#diagnostics, { eventName: 'settlement', transition: 'started' })
  }

  runFinalSettlement<T>(
    kind: FSATerminalMutationKind,
    observe: (authority: FSAFinalSettlementObservation) => Promise<T>,
  ): Promise<T> {
    this.beginTerminal(kind)
    if (this.#settlementStarted || this.#outputClosePromise !== undefined) {
      throw new DOMException('FSA materialization settlement already started', 'InvalidStateError')
    }
    this.#settlementStarted = true
    const materializationDrain = this.#materialization.close()
    return this.#runFinalSettlement(materializationDrain, observe)
  }

  async #runFinalSettlement<T>(
    materializationDrain: Promise<void>,
    observe: (authority: FSAFinalSettlementObservation) => Promise<T>,
  ): Promise<T> {
    const terminalDrain = this.#terminalDrain!
    await Promise.all([materializationDrain, terminalDrain.drained])
    this.#requireDrainedScheduler()
    this.#settlementObservationActive = true
    try {
      const result = await terminalDrain.runExclusive(
        authority => observe(this.#observation(authority)),
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

  closeForTerminalSettlement(): Promise<void> {
    if (this.#settlementObservationActive) {
      return Promise.reject(new DOMException(
        'FSA output authorities cannot close during final observation',
        'InvalidStateError',
      ))
    }
    this.#outputClosePromise ??= this.#closeOutputAuthorities()
    return this.#outputClosePromise
  }

  releaseRootLease(): Promise<void> {
    this.#rootReleasePromise ??= this.#rootLease.release()
    return this.#rootReleasePromise
  }

  async #close(): Promise<void> {
    const failures: unknown[] = []
    try {
      await this.closeForTerminalSettlement()
    } catch (error) {
      failures.push(error)
    }
    try {
      await this.releaseRootLease()
    } catch (error) {
      failures.push(error)
      recordOutputException(this.#diagnostics?.failures?.cleanup, error)
    }
    if (failures.length === 0) return
    outputTrace(this.#diagnostics, { eventName: 'cleanup', transition: 'failed' })
    if (failures.length === 1) throw failures[0]
    throw new AggregateError(failures, 'FSA output authorities did not close cleanly')
  }

  async #closeOutputAuthorities(): Promise<void> {
    const failures: unknown[] = []
    try {
      await this.#materialization.close()
    } catch (error) {
      // File-transaction cleanup owns its native classification; this layer preserves
      // that failure while still releasing repository-backed output authorities.
      failures.push(error)
    }
    try {
      this.#checkpoints.close()
    } catch (error) {
      failures.push(error)
      recordOutputException(this.#diagnostics?.failures?.cleanup, error)
    }
    try {
      this.#compatibleNames.close()
    } catch (error) {
      failures.push(error)
      recordOutputException(this.#diagnostics?.failures?.cleanup, error)
    }
    if (failures.length !== 0) {
      outputTrace(this.#diagnostics, { eventName: 'cleanup', transition: 'failed' })
      if (failures.length === 1) throw failures[0]
      throw new AggregateError(failures, 'FSA output repositories did not close cleanly')
    }
    outputTrace(this.#diagnostics, { eventName: 'cleanup', transition: 'completed' })
  }

  #observation(authority: FSATerminalExclusiveAuthority): FSAFinalSettlementObservation {
    if (authority.kind !== this.#terminalDrain?.kind) {
      throw new DOMException('FSA terminal exclusive authority is foreign', 'InvalidStateError')
    }
    return Object.freeze({
      persistCompatibleNamePendingOutcome: (outcome: CompatibleNamePendingTerminalOutcomeV1) =>
        this.#compatibleNames.persistPendingTerminalOutcome(outcome),
      drainCompatibleNameProjector: (state: CompatibleNameTerminalFooterState) =>
        this.#compatibleNames.drainTerminalProjector(state),
      clearCompatibleNamePendingOutcome: () =>
        this.#compatibleNames.clearPendingTerminalOutcome(),
      removeVerifiedEmptyCompatibleNameRepair: () =>
        this.#compatibleNames.removeVerifiedEmptyRepairWithinMutation(this.#tree.ownedRoot()),
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
      sealMaterializationLedger: (input: Parameters<
        FSAFinalSettlementObservation['sealMaterializationLedger']
      >[0]) => this.#settlementLedger.seal(input),
      resumableCheckpointEvidence: () => this.#settlementLedger.resumableCheckpointEvidence(),
      retireRecoveryMetadata: () => this.#settlementLedger.retireRecoveryMetadata(),
    })
  }

  #requireDrainedScheduler(): void {
    const diagnostics = this.#rootLease.scheduler.diagnostics()
    if (diagnostics.activeWriters !== 0 || diagnostics.queuedWriters !== 0 ||
        diagnostics.activeNamespaceMutations !== 0 || diagnostics.queuedNamespaceMutations !== 0) {
      throw new DOMException('FSA terminal cut retained active native work', 'InvalidStateError')
    }
  }

  #requireMaterializing(): void {
    if (this.#terminalDrain !== undefined || this.#settlementStarted ||
        this.#outputClosePromise !== undefined || this.#closePromise !== undefined) {
      throw new DOMException('FSA materialization is no longer mutable', 'InvalidStateError')
    }
  }

  #requireDirectoryFinalizable(): void {
    if (this.#settlementStarted || this.#outputClosePromise !== undefined ||
        this.#closePromise !== undefined) {
      throw new DOMException('FSA directory finalization is no longer mutable', 'InvalidStateError')
    }
  }
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
  let checkpoints: FSASemanticOutputRepository | undefined
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
      semantic: checkpoints,
      ...(options.maximumConcurrentInitialClaimInspections === undefined
        ? {}
        : {
            maximumConcurrentInitialClaimInspections:
              options.maximumConcurrentInitialClaimInspections,
          }),
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
