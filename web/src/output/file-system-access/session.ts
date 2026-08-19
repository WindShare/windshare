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
import { authorizeFSAParent } from '../capability/acquisition'
import type { AcquiredFSAParentAuthority } from '../capability/contract'
import { BrowserFileSystemTree } from '../browser/filesystem-tree'
import {
  persistFSAOperationBinding,
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
  PersistentMaterializationPort,
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

export interface BindNewFileSystemAccessOutputOptions {
  readonly authority: AcquiredFSAParentAuthority
  readonly artifact: DirectoryTreeArtifact
  readonly operationRepository: FSAOperationBindingRepository
  readonly freezeIntent: (
    reservation: NamedContainerEntryReservation,
  ) => Promise<ReceiveIntent>
  readonly lockManager?: BrowserLockManagerRuntime
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
  readonly operationId?: string
  readonly reservationId?: string
  readonly authorityRef?: string
  readonly trace?: FSAOutputTrace
}

export interface ReopenFileSystemAccessOutputOptions {
  readonly intent: ReceiveIntent
  readonly operationRepository: FSAOperationBindingRepository
  readonly lockManager?: BrowserLockManagerRuntime
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
  readonly trace?: FSAOutputTrace
}

export class FileSystemAccessOutputSession implements PersistentMaterializationPort {
  readonly intent: ReceiveIntent
  readonly reservation: NamedContainerEntryReservation
  readonly #materialization: PersistentTreeOutputSession
  readonly #tree: BrowserFileSystemTree
  readonly #binding: PersistedFSAOperationBinding
  readonly #operationRepository: FSAOperationBindingRepository
  readonly #checkpoints: FSAFileCheckpointRepository
  readonly #rootLease: FSARootMutationLease
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
  }>) {
    this.intent = input.intent
    this.reservation = input.reservation
    this.#materialization = input.materialization
    this.#tree = input.tree
    this.#binding = input.binding
    this.#operationRepository = input.operationRepository
    this.#checkpoints = input.checkpoints
    this.#rootLease = input.rootLease
  }

  beginFile(request: PersistentFileRequest): Promise<PersistentFileTransactionPort> {
    this.#requireMaterializing()
    return this.#materialization.beginFile(request)
  }

  ensureDirectory(path: readonly string[]): Promise<PersistentDirectoryMaterialization> {
    this.#requireMaterializing()
    return this.#materialization.ensureDirectory(path)
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
    // Quiescing writers precedes the mutation barrier, while repository and Web Lock
    // resources remain live until the settlement cut explicitly closes the session.
    await this.#materialization.close()
    this.#settlementObservationActive = true
    try {
      return await this.#rootLease.authority.run(
        'settle-operation',
        () => observe(this.#observation()),
      )
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
    let failure: unknown
    try {
      await this.#materialization.close()
    } catch (error) {
      failure = error
    }
    this.#checkpoints.close()
    try {
      await this.#rootLease.release()
    } catch (error) {
      failure = failure === undefined
        ? error
        : new AggregateError([failure, error], 'FSA materialization and root lease close failed')
    }
    if (failure !== undefined) throw failure
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

export async function bindNewFileSystemAccessOutput(
  options: BindNewFileSystemAccessOutputOptions,
): Promise<FileSystemAccessOutputSession> {
  const artifact = await requireDirectoryTreeArtifact(options.artifact)
  const operationId = options.operationId ?? createOperationID()
  const reservationId = options.reservationId ?? createDestinationReservationID()
  const authorityRef = canonicalAuthorityRef(options.authorityRef ?? createAuthorityRef())
  const rootLease = options.lockManager === undefined
    ? await acquireFSARootMutationLease(options.authority.parent)
    : await acquireFSARootMutationLease(options.authority.parent, options.lockManager)
  let checkpoints: FSAFileCheckpointRepository | undefined
  try {
    await authorizeFSAParent(options.authority)
    const reservation = await rootLease.authority.run('reserve-name', async () => {
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
    const intent = await validateBoundIntent(
      await options.freezeIntent(reservation),
      artifact,
      reservation,
    )
    const binding = await persistFSAOperationBinding({
      repository: options.operationRepository,
      intent,
      parent: options.authority.parent,
    })
    emitFSAOutputTrace(options.trace, reservationCreated(reservation))
    checkpoints = await openFSAFileCheckpointRepository(options, intent, reservation)
    const tree = new BrowserFileSystemTree({
      binding,
      operationRepository: options.operationRepository,
      fileHandles: checkpoints,
      mutations: rootLease.authority,
    })
    const materialization = await PersistentTreeOutputSession.open({
      tree,
      checkpoints,
      ...(options.trace === undefined ? {} : { trace: options.trace }),
    })
    return new FileSystemAccessOutputSession({
      intent,
      reservation,
      materialization,
      tree,
      binding,
      operationRepository: options.operationRepository,
      checkpoints,
      rootLease,
    })
  } catch (error) {
    if (error instanceof TargetOwnershipUnknownError && error.stage !== 'checkpoint') {
      emitFSAOutputTrace(options.trace, needsAttention(operationId))
    }
    checkpoints?.close()
    await rootLease.release().catch(() => undefined)
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
    throw error
  }
  const rootLease = options.lockManager === undefined
    ? await acquireFSARootMutationLease(firstBinding.parent)
    : await acquireFSARootMutationLease(firstBinding.parent, options.lockManager)
  let checkpoints: FSAFileCheckpointRepository | undefined
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
    const materialization = await PersistentTreeOutputSession.open({
      tree,
      checkpoints,
      ...(options.trace === undefined ? {} : { trace: options.trace }),
    })
    emitFSAOutputTrace(options.trace, Object.freeze({
      name: 'receive.reservation.reopened',
      operation_id: intent.operationId,
      receive_intent_digest: intent.digest,
      reservation_kind: 'named-container-entry',
    }))
    return new FileSystemAccessOutputSession({
      intent,
      reservation: binding.reservation,
      materialization,
      tree,
      binding,
      operationRepository: options.operationRepository,
      checkpoints,
      rootLease,
    })
  } catch (error) {
    if (error instanceof TargetOwnershipUnknownError && error.stage !== 'checkpoint') {
      emitFSAOutputTrace(options.trace, needsAttention(intent.operationId))
    }
    checkpoints?.close()
    await rootLease.release().catch(() => undefined)
    throw error
  }
}

async function validateBoundIntent(
  input: ReceiveIntent,
  artifact: DirectoryTreeArtifact,
  reservation: NamedContainerEntryReservation,
): Promise<ReceiveIntent> {
  const intent = await validateReceiveIntent(input)
  if (intent.artifact.digest !== artifact.digest || intent.operationId !== reservation.operationId ||
      intent.plan.kind !== 'direct-tree' ||
      intent.plan.reservation.digest !== reservation.digest) {
    throw new TypeError('Frozen ReceiveIntent does not bind the acquired FSA reservation')
  }
  return intent
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

function createAuthorityRef(): string {
  const value = new Uint8Array(FILE_CHECKPOINT_ID_BYTES)
  crypto.getRandomValues(value)
  return canonicalAuthorityRef(encodeBase64Url(value))
}

function errorNamed(error: unknown, name: string): boolean {
  return typeof error === 'object' && error !== null &&
    'name' in error && (error as { readonly name?: unknown }).name === name
}
