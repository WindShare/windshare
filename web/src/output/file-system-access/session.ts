import {
  createDestinationReservationID,
  createFSANamedEntryReservation,
  createOperationID,
  validateArtifactSpec,
  type DirectoryTreeArtifact,
  type FSANamedContainerEntryReservation,
  type ReceiveIntent,
} from '../../transfer/intent'
import type { SafeRenamedSummary } from '../../transfer/lifecycle/safe-names'
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
import { decideCollisionName } from '../planning/naming'
import type {
  PersistentDirectoryMaterialization,
  PersistentFileRequest,
  PersistentFileTransactionPort,
  ActivatablePersistentMaterializationPort,
  PersistentTreeTraceEvent,
} from '../persistent-tree/contracts'
import { PersistentTreeOutputSession } from '../persistent-tree/session'
import {
  createPersistentOutputStageAuthority,
  type PersistentOutputStageAuthority,
  type PersistentOutputStageDiagnostics,
} from '../persistent-tree/stage-diagnostics'
import {
  openFSAFileCheckpointRepository,
  type FSAFileCheckpointRepositoryFactory,
  type FSASemanticOutputRepository,
} from './checkpoint-repository'
import type { PersistentDirectTreeMaterializationEvidence } from '../execution/persistent-execution'
import { SafeNameRegistry, deterministicSafeComponent } from './safe-name'
import {
  canonicalAuthorityRef,
  createFSAAuthorityReference,
  emitFSAOutputTrace,
  outputTrace,
  reservationCreated,
} from './session-diagnostics'
import {
  FSASettlementLedgerAuthority,
  type FSACheckpointSettlementSnapshot,
  type FSAResumableCheckpointSettlement,
} from './settlement-ledger'
import { closeFailedFSAAssembly, closeFSAOutputAuthorities } from './assembly-cleanup'

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
export { reopenFileSystemAccessOutput } from './session-reopen'
export type { ReopenFileSystemAccessOutputOptions } from './session-reopen'
export { createFSAAuthorityReference } from './session-diagnostics'

const MAX_COLLISION_INDEX = 0xffff_ffff

export type FSAReservationTraceEvent =
  | Readonly<{
      name: 'receive.reservation.created'
      operation_id: string
      reservation_kind: 'named-container-entry'
      collision_index: number
      name_chooser: 'application-chosen' | 'user-chosen'
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
  verifyOperationBinding(): Promise<void>
  checkpointSettlementSnapshot(
    evidence: PersistentDirectTreeMaterializationEvidence,
  ): Promise<FSACheckpointSettlementSnapshot>
  resumableCheckpointSettlement(
    evidence: PersistentDirectTreeMaterializationEvidence,
  ): Promise<FSAResumableCheckpointSettlement>
  retireRecoveryMetadata(): Promise<void>
}

export interface ReserveNewFileSystemAccessOutputOptions {
  readonly authority: AcquiredFSAParentAuthority
  readonly artifact: DirectoryTreeArtifact
  readonly rootLease: FSARootMutationLease
  readonly operationId?: string
  readonly reservationId?: string
  readonly authorityRef?: string
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly trace?: FSAOutputTrace
}

export interface ReservedFileSystemAccessOutput {
  readonly authority: AcquiredFSAParentAuthority
  readonly artifact: DirectoryTreeArtifact
  readonly operationId: string
  readonly reservation: FSANamedContainerEntryReservation
  readonly rootLease: FSARootMutationLease
}

export interface AssembleNewFileSystemAccessOutputOptions {
  readonly binding: PersistedFSAOperationBinding
  readonly operationRepository: FSAOperationBindingRepository
  readonly rootLease: FSARootMutationLease
  readonly maximumConcurrentInitialClaimInspections?: number
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly stageDiagnostics?: PersistentOutputStageDiagnostics
  readonly trace?: FSAOutputTrace
}

export class FileSystemAccessOutputSession implements ActivatablePersistentMaterializationPort {
  readonly intent: ReceiveIntent
  readonly reservation: FSANamedContainerEntryReservation
  readonly #materialization: PersistentTreeOutputSession
  readonly #binding: PersistedFSAOperationBinding
  readonly #operationRepository: FSAOperationBindingRepository
  readonly #checkpoints: FSASemanticOutputRepository
  readonly #rootLease: FSARootMutationLease
  readonly #safeNames: SafeNameRegistry
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
    binding: PersistedFSAOperationBinding
    operationRepository: FSAOperationBindingRepository
    checkpoints: FSASemanticOutputRepository
    rootLease: FSARootMutationLease
    safeNames: SafeNameRegistry
    stageAuthority?: PersistentOutputStageAuthority
    diagnostics?: OutputDiagnosticsPorts
  }>) {
    this.intent = input.intent
    this.reservation = input.reservation
    this.#materialization = input.materialization
    this.#binding = input.binding
    this.#operationRepository = input.operationRepository
    this.#checkpoints = input.checkpoints
    this.#rootLease = input.rootLease
    this.#safeNames = input.safeNames
    this.#stageAuthority = input.stageAuthority
    this.#diagnostics = input.diagnostics
    this.#settlementLedger = new FSASettlementLedgerAuthority({
      intent: input.intent,
      checkpoints: input.checkpoints,
    })
  }

  beginFile(request: PersistentFileRequest): Promise<PersistentFileTransactionPort> {
    this.#requireMaterializing()
    return this.#materialization.beginFile(request)
  }

  ensureDirectory(
    path: readonly string[],
  ): Promise<PersistentDirectoryMaterialization> {
    this.#requireMaterializing()
    return this.#materialization.ensureDirectory(path)
  }

  activate(): Promise<void> {
    this.#requireMaterializing()
    this.#activationPromise ??= this.#materialization.activate()
    return this.#activationPromise
  }

  usesOperationRepository(repository: FSAOperationBindingRepository): boolean {
    return repository === this.#operationRepository
  }

  safeRenamedSummary(): SafeRenamedSummary | undefined {
    return this.#safeNames.summary()
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

  #closeOutputAuthorities(): Promise<void> {
    return closeFSAOutputAuthorities({
      materialization: this.#materialization,
      checkpoints: this.#checkpoints,
      diagnostics: this.#diagnostics,
    })
  }

  #observation(authority: FSATerminalExclusiveAuthority): FSAFinalSettlementObservation {
    if (authority.kind !== this.#terminalDrain?.kind) {
      throw new DOMException('FSA terminal exclusive authority is foreign', 'InvalidStateError')
    }
    return Object.freeze({
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
      checkpointSettlementSnapshot: (evidence: PersistentDirectTreeMaterializationEvidence) =>
        this.#settlementLedger.checkpointSettlementSnapshot(evidence),
      resumableCheckpointSettlement: (evidence: PersistentDirectTreeMaterializationEvidence) =>
        this.#settlementLedger.resumableCheckpointSettlement(evidence),
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
}

export async function reserveNewFileSystemAccessOutput(
  options: ReserveNewFileSystemAccessOutputOptions,
): Promise<ReservedFileSystemAccessOutput> {
  const artifact = await requireDirectoryTreeArtifact(options.artifact)
  const operationId = options.operationId ?? createOperationID()
  const reservationId = options.reservationId ?? createDestinationReservationID()
  const authorityRef = canonicalAuthorityRef(options.authorityRef ?? createFSAAuthorityReference())
  const reservation = await options.rootLease.authority.run('reserve-name', async () => {
    const decision = await firstAvailableName(options.authority.parent, operationId, artifact)
    return createFSANamedEntryReservation({
      operationId,
      reservationId,
      artifact,
      authorityRef,
      logicalReservedName: decision.reservedName,
      physicalName: decision.physicalName,
      collisionIndex: decision.collisionIndex,
    })
  })
  return Object.freeze({
    authority: options.authority,
    artifact,
    operationId,
    reservation,
    rootLease: options.rootLease,
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
  const safeNames = new SafeNameRegistry(binding.reservation)
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
      safeNames,
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
      binding,
      operationRepository: options.operationRepository,
      checkpoints,
      rootLease: options.rootLease,
      safeNames,
      ...(stageAuthority === undefined ? {} : { stageAuthority }),
      ...(options.diagnostics === undefined
        ? {}
        : { diagnostics: options.diagnostics }),
    })
  } catch (error) {
    recordOutputException(options.diagnostics?.failures?.outputReservation, error)
    outputTrace(options.diagnostics, { eventName: 'output_reservation', transition: 'failed' })
    const cleanupFailure = closeFailedFSAAssembly(checkpoints, options.diagnostics)
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
): Promise<{
  readonly requestedName: string
  readonly reservedName: string
  readonly physicalName: string
  readonly collisionIndex: number
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
      if (!(error instanceof PathComponentRejectedError)) throw error
      const safeName = await deterministicSafeComponent({
        logicalPath: [decision.reservedName],
        entryKind: decision.entryKind,
        collisionIndex: decision.collisionIndex,
      })
      if (await inspectFileSystemComponent({
        verifiedParent: parent,
        component: safeName,
        expectedKind: decision.entryKind,
        stage: 'fsa.root.entry.inspect',
        mode: 'diagnostic',
      }) === 'occupied') continue
      return Object.freeze({
        requestedName: decision.requestedName,
        reservedName: decision.reservedName,
        physicalName: safeName,
        collisionIndex: decision.collisionIndex,
      })
    }
  }
  throw new DOMException('The FSA collision namespace is exhausted', 'InvalidStateError')
}
