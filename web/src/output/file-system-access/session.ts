import { encodeBase64Url } from '../../crypto/bytes'
import {
  createDestinationReservationID,
  createFSANamedEntryReservation,
  createOperationID,
  validateArtifactSpec,
  validateReceiveIntent,
  type DirectoryTreeArtifact,
  type NamedContainerEntryReservation,
  type ReceiveIntent,
} from '../../transfer/intent'
import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../diagnostics'
import type { AcquiredFSAParentAuthority } from '../capability/contract'
import { BrowserFileSystemTree } from '../browser/filesystem-tree'
import {
  verifyFSAOperationBinding,
  type FSAOperationBindingRepository,
  type PersistedFSAOperationBinding,
} from '../browser/indexeddb-root-binding'
import {
  acquireFSARootMutationLease,
  type BrowserLockManagerRuntime,
  type FSARootMutationLease,
} from '../browser/namespace-mutation'
import {
  FILE_CHECKPOINT_ID_BYTES,
  fileCheckpointIsComplete,
  identityBytes,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import type { FinalFileCheckpointProof } from '../persistence/journal'
import { decideCollisionName } from '../planning'
import type {
  PersistentDirectoryMaterialization,
  PersistentFileRequest,
  PersistentFileTransactionPort,
  ActivatablePersistentMaterializationPort,
  PersistentTreeTraceEvent,
} from '../persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import { PersistentTreeOutputSession } from '../persistent-tree/session'
import {
  openFSAFileCheckpointRepository,
  scanAllFSAFileCheckpoints,
  type FSAFileCheckpointRepository,
  type FSAFileCheckpointRepositoryFactory,
} from './checkpoint-repository'

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
      name_authority: 'application-chosen'
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
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly trace?: FSAOutputTrace
}

export interface ReservedFileSystemAccessOutput {
  readonly authority: AcquiredFSAParentAuthority
  readonly artifact: DirectoryTreeArtifact
  readonly operationId: string
  readonly reservation: NamedContainerEntryReservation
  readonly rootLease: FSARootMutationLease
}

export interface AssembleNewFileSystemAccessOutputOptions {
  readonly binding: PersistedFSAOperationBinding
  readonly operationRepository: FSAOperationBindingRepository
  readonly rootLease: FSARootMutationLease
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly trace?: FSAOutputTrace
}

export interface ReopenFileSystemAccessOutputOptions {
  readonly intent: ReceiveIntent
  readonly operationRepository: FSAOperationBindingRepository
  readonly lockManager?: BrowserLockManagerRuntime
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly trace?: FSAOutputTrace
}

export class FileSystemAccessOutputSession implements ActivatablePersistentMaterializationPort {
  readonly intent: ReceiveIntent
  readonly reservation: NamedContainerEntryReservation
  readonly #materialization: PersistentTreeOutputSession
  readonly #tree: BrowserFileSystemTree
  readonly #binding: PersistedFSAOperationBinding
  readonly #operationRepository: FSAOperationBindingRepository
  readonly #checkpoints: FSAFileCheckpointRepository
  readonly #rootLease: FSARootMutationLease
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  #settlementStarted = false
  #settlementObservationActive = false
  #closePromise: Promise<void> | undefined

  constructor(input: Readonly<{
    intent: ReceiveIntent
    reservation: NamedContainerEntryReservation
    materialization: PersistentTreeOutputSession
    tree: BrowserFileSystemTree
    binding: PersistedFSAOperationBinding
    operationRepository: FSAOperationBindingRepository
    checkpoints: FSAFileCheckpointRepository
    rootLease: FSARootMutationLease
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
    this.#diagnostics = input.diagnostics
  }

  beginFile(request: PersistentFileRequest): Promise<PersistentFileTransactionPort> {
    this.#requireMaterializing()
    return this.#materialization.beginFile(request)
  }

  ensureDirectory(path: readonly string[]): Promise<PersistentDirectoryMaterialization> {
    this.#requireMaterializing()
    return this.#materialization.ensureDirectory(path)
  }

  activate(): Promise<void> {
    this.#requireMaterializing()
    return this.#materialization.activate()
  }

  usesOperationRepository(repository: FSAOperationBindingRepository): boolean {
    return repository === this.#operationRepository
  }

  async runFinalSettlement<T>(
    observe: (authority: FSAFinalSettlementObservation) => Promise<T>,
  ): Promise<T> {
    if (this.#settlementStarted || this.#closePromise !== undefined) {
      throw new DOMException('FSA materialization settlement already started', 'InvalidStateError')
    }
    this.#settlementStarted = true
    outputTrace(this.#diagnostics, { eventName: 'settlement', transition: 'started' })
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
      verifyOperationBinding: async () => {
        await verifyFSAOperationBinding({
          repository: this.#operationRepository,
          intent: this.intent,
          expectedParent: this.#binding.parent,
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

export async function reserveNewFileSystemAccessOutput(
  options: ReserveNewFileSystemAccessOutputOptions,
): Promise<ReservedFileSystemAccessOutput> {
  const artifact = await requireDirectoryTreeArtifact(options.artifact)
  const operationId = options.operationId ?? createOperationID()
  const reservationId = options.reservationId ?? createDestinationReservationID()
  const authorityRef = canonicalAuthorityRef(options.authorityRef ?? createFSAAuthorityReference())
  const reservation = await options.rootLease.authority.run('reserve-name', async () => {
    const decision = await firstAvailableName(
      options.authority.parent,
      operationId,
      artifact,
    )
    return createFSANamedEntryReservation({
      operationId,
      reservationId,
      artifact,
      authorityRef,
      reservedName: decision.reservedName,
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
  const binding = await verifyFSAOperationBinding({
    repository: options.operationRepository,
    intent: options.binding.intent,
    expectedParent: options.binding.parent,
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
    })
    const materialization = await PersistentTreeOutputSession.createNew({
      tree,
      checkpoints,
      ...(options.diagnostics === undefined
        ? {}
        : { diagnostics: options.diagnostics }),
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
      ...(options.diagnostics === undefined
        ? {}
        : { diagnostics: options.diagnostics }),
    })
  } catch (error) {
    recordOutputException(options.diagnostics?.failures?.outputReservation, error)
    outputTrace(options.diagnostics, { eventName: 'output_reservation', transition: 'failed' })
    let cleanupFailure: unknown
    try {
      checkpoints?.close()
    } catch (caught) {
      cleanupFailure = caught
      recordOutputException(options.diagnostics?.failures?.cleanup, caught)
    }
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

export async function reopenFileSystemAccessOutput(
  options: ReopenFileSystemAccessOutputOptions,
): Promise<FileSystemAccessOutputSession> {
  const intent = await validateReceiveIntent(options.intent)
  let firstBinding: PersistedFSAOperationBinding
  try {
    firstBinding = await verifyFSAOperationBinding({
      repository: options.operationRepository,
      intent,
    })
  } catch (error) {
    if (error instanceof TargetOwnershipUnknownError && error.stage !== 'checkpoint') {
      emitFSAOutputTrace(options.trace, needsAttention(intent.operationId))
    }
    recordOutputException(options.diagnostics?.failures?.reopen, error)
    outputTrace(options.diagnostics, { eventName: 'reopen', transition: 'failed' })
    throw error
  }
  const rootLease = await (options.lockManager === undefined
    ? acquireFSARootMutationLease(firstBinding.parent)
    : acquireFSARootMutationLease(firstBinding.parent, options.lockManager)
  ).catch((error: unknown) => {
    recordOutputException(options.diagnostics?.failures?.reopen, error)
    outputTrace(options.diagnostics, { eventName: 'reopen', transition: 'failed' })
    throw error
  })
  let checkpoints: FSAFileCheckpointRepository | undefined
  let materializationOpening = false
  try {
    const binding = await verifyFSAOperationBinding({
      repository: options.operationRepository,
      intent,
      expectedParent: firstBinding.parent,
    })
    checkpoints = await openFSAFileCheckpointRepository(options, intent, binding.reservation)
    const tree = new BrowserFileSystemTree({
      binding,
      operationRepository: options.operationRepository,
      fileHandles: checkpoints,
      mutations: rootLease.authority,
    })
    materializationOpening = true
    const materialization = await PersistentTreeOutputSession.open({
      tree,
      checkpoints,
      ...(options.diagnostics === undefined
        ? {}
        : { diagnostics: options.diagnostics }),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
    })
    emitFSAOutputTrace(options.trace, Object.freeze({
      name: 'receive.reservation.reopened',
      operation_id: intent.operationId,
      receive_intent_digest: intent.digest,
      reservation_kind: 'named-container-entry',
    }))
    outputTrace(options.diagnostics, { eventName: 'reopen', transition: 'authorized' })
    return new FileSystemAccessOutputSession({
      intent,
      reservation: binding.reservation,
      materialization,
      tree,
      binding,
      operationRepository: options.operationRepository,
      checkpoints,
      rootLease,
      ...(options.diagnostics === undefined
        ? {}
        : { diagnostics: options.diagnostics }),
    })
  } catch (error) {
    reportFSAReopenFailure(options, intent.operationId, error, materializationOpening)
    const cleanupFailures = await releaseFailedFSAOpen(
      checkpoints,
      rootLease,
      options.diagnostics,
    )
    if (cleanupFailures.length !== 0) {
      throw new AggregateError(
        [error, ...cleanupFailures],
        'FSA output reopen failed and could not release all authorities',
        { cause: error },
      )
    }
    throw error
  }
}

function reportFSAReopenFailure(
  options: ReopenFileSystemAccessOutputOptions,
  operationId: string,
  error: unknown,
  materializationOpening: boolean,
): void {
  if (error instanceof TargetOwnershipUnknownError && error.stage !== 'checkpoint') {
    emitFSAOutputTrace(options.trace, needsAttention(operationId))
  }
  if (!materializationOpening) {
    recordOutputException(options.diagnostics?.failures?.reopen, error)
  }
  outputTrace(options.diagnostics, { eventName: 'reopen', transition: 'failed' })
}

async function releaseFailedFSAOpen(
  checkpoints: FSAFileCheckpointRepository | undefined,
  rootLease: FSARootMutationLease,
  diagnostics: OutputDiagnosticsPorts | undefined,
): Promise<readonly unknown[]> {
  const failures: unknown[] = []
  try {
    checkpoints?.close()
  } catch (error) {
    failures.push(error)
    recordOutputException(diagnostics?.failures?.cleanup, error)
  }
  try {
    await rootLease.release()
  } catch (error) {
    failures.push(error)
    recordOutputException(diagnostics?.failures?.cleanup, error)
  }
  if (failures.length !== 0) {
    outputTrace(diagnostics, { eventName: 'cleanup', transition: 'failed' })
  }
  return Object.freeze(failures)
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
  readonly collisionIndex: number
}> {
  for (let collisionIndex = 0; collisionIndex <= MAX_COLLISION_INDEX; collisionIndex += 1) {
    const decision = await decideCollisionName(operationId, artifact, collisionIndex)
    if (!await namespaceEntryExists(parent, decision.reservedName)) {
      return Object.freeze({
        requestedName: decision.requestedName,
        reservedName: decision.reservedName,
        collisionIndex: decision.collisionIndex,
      })
    }
  }
  throw new DOMException('The FSA collision namespace is exhausted', 'InvalidStateError')
}

async function namespaceEntryExists(
  parent: FileSystemDirectoryHandle,
  name: string,
): Promise<boolean> {
  try {
    await parent.getFileHandle(name)
    return true
  } catch (error) {
    if (errorNamed(error, 'TypeMismatchError')) return true
    if (!errorNamed(error, 'NotFoundError')) throw error
  }
  try {
    await parent.getDirectoryHandle(name)
    return true
  } catch (error) {
    if (errorNamed(error, 'TypeMismatchError')) return true
    if (errorNamed(error, 'NotFoundError')) return false
    throw error
  }
}

function reservationCreated(
  reservation: NamedContainerEntryReservation,
): Extract<FSAReservationTraceEvent, { name: 'receive.reservation.created' }> {
  return Object.freeze({
    name: 'receive.reservation.created',
    operation_id: reservation.operationId,
    reservation_kind: 'named-container-entry',
    collision_index: reservation.collisionIndex,
    name_authority: 'application-chosen',
    replacement_guarantee: 'coordinated-no-replace',
    delivery_mode: 'managed-target',
    commit_visibility: 'prefix-visible',
    rollback_guarantee: 'none',
  })
}

function needsAttention(
  operationId: string,
): Extract<PersistentTreeTraceEvent, { name: 'receive.operation.needs_attention' }> {
  return Object.freeze({
    name: 'receive.operation.needs_attention',
    operation_id: operationId,
    prior_state: 'receiving',
    needs_attention_reason: 'target-ownership-unknown',
  })
}

type FSAOutputTraceInput =
  | Readonly<{
      eventName: 'output_reservation'
      transition: 'acquired' | 'failed'
    }>
  | Readonly<{
      eventName: 'settlement'
      transition: 'started' | 'completed' | 'failed'
    }>
  | Readonly<{
      eventName: 'reopen'
      transition: 'authorized' | 'failed'
    }>
  | Readonly<{
      eventName: 'cleanup'
      transition: 'completed' | 'failed'
    }>

function outputTrace(
  diagnostics: OutputDiagnosticsPorts | undefined,
  input: FSAOutputTraceInput,
): void {
  emitOutputTrace(diagnostics?.trace, () =>
    outputTraceEvent(input.eventName, {
      backend: 'file_system_access',
      transition: input.transition,
    }))
}

function emitFSAOutputTrace(
  trace: FSAOutputTrace | undefined,
  event: FSAOutputTraceEvent,
): void {
  try {
    trace?.(event)
  } catch {
    // Durable destination state must never depend on an observability sink.
  }
}

function canonicalAuthorityRef(value: string): string {
  return encodeBase64Url(identityBytes(value, FILE_CHECKPOINT_ID_BYTES, 'authority reference'))
}

export function createFSAAuthorityReference(): string {
  const value = new Uint8Array(FILE_CHECKPOINT_ID_BYTES)
  crypto.getRandomValues(value)
  return canonicalAuthorityRef(encodeBase64Url(value))
}

function errorNamed(error: unknown, name: string): boolean {
  return typeof error === 'object' && error !== null &&
    'name' in error && (error as { readonly name?: unknown }).name === name
}
