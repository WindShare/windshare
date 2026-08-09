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
  authorizeFSAParent,
} from '../capability/acquisition'
import type { AcquiredFSAParentAuthority } from '../capability/contract'
import {
  BrowserFileSystemTree,
  FSA_FILE_HANDLE_KIND,
  fsaDirectoryHandleId,
} from '../browser/filesystem-tree'
import {
  FSA_OPERATION_HANDLE_DIRECTORY,
  FSA_OPERATION_HANDLE_PARENT,
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
  IndexedDbFileCheckpointRepository,
} from '../browser/indexeddb-repository'
import {
  FILE_CHECKPOINT_ID_BYTES,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  fileCheckpointIsComplete,
  identityBytes,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import type {
  FileCheckpointJournal,
  FinalFileCheckpointProof,
  PersistentHandleInventoryRepository,
  PersistentHandleRecord,
} from '../persistence/journal'
import { validateFileCheckpointPage } from '../persistence/journal'
import { durableCheckpointNamespaceIdentity } from '../persistence/namespace'
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
import type { ReceiveOperationHandleRecord } from '../workspace/records'
import type {
  ReceiveOperationHandleInventoryRepository,
  ReceiveOperationRepository,
} from '../workspace/repository'

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

export interface FSAFileCheckpointRepository
extends FileCheckpointJournal, PersistentHandleInventoryRepository {
  close(): void
}

export type FSAFileCheckpointRepositoryFactory = (
  binding: FileCheckpointJournal['binding'],
) => Promise<FSAFileCheckpointRepository>

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

export interface OpenFreshPageFileSystemAccessDiscardOptions {
  readonly intent: ReceiveIntent
  readonly binding: PersistedFSAOperationBinding
  readonly operationRepository: ReceiveOperationRepository
  readonly lockManager?: BrowserLockManagerRuntime
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
}

export interface FSAFreshPageDiscardCut {
  readonly committedCheckpoints: readonly FileCheckpointV2[]
  readonly candidateCheckpoints: readonly FileCheckpointV2[]
  readonly successfulCheckpoints: readonly FileCheckpointV2[]
  readonly removedObjectIds: readonly string[]
  readonly removedDirectoryHandleIds: readonly string[]
  retireCheckpoints(): Promise<void>
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
      committedCheckpoints: () => scanAllCheckpoints(this.#checkpoints, 'committed'),
      candidateCheckpoints: () => scanAllCheckpoints(this.#checkpoints, 'candidates'),
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

/**
 * A fresh-page discard session deliberately has no materialization methods. It
 * reopens persisted namespace evidence under the parent Web Lock, removes only
 * objects whose current handles still match that evidence, and keeps the lock
 * alive until the lifecycle owner commits the terminal receipt.
 */
export class FreshPageFileSystemAccessDiscardSession {
  readonly intent: ReceiveIntent
  readonly #binding: PersistedFSAOperationBinding
  readonly #operationRepository: ReceiveOperationRepository
  readonly #tree: BrowserFileSystemTree
  readonly #checkpoints: FSAFileCheckpointRepository
  readonly #rootLease: FSARootMutationLease
  #discardStarted = false
  #closePromise: Promise<void> | undefined

  constructor(input: Readonly<{
    intent: ReceiveIntent
    binding: PersistedFSAOperationBinding
    operationRepository: ReceiveOperationRepository
    tree: BrowserFileSystemTree
    checkpoints: FSAFileCheckpointRepository
    rootLease: FSARootMutationLease
  }>) {
    this.intent = input.intent
    this.#binding = input.binding
    this.#operationRepository = input.operationRepository
    this.#tree = input.tree
    this.#checkpoints = input.checkpoints
    this.#rootLease = input.rootLease
  }

  usesOperationRepository(repository: ReceiveOperationRepository): boolean {
    return repository === this.#operationRepository
  }

  async discardOwnedUnfinishedObjects(): Promise<FSAFreshPageDiscardCut> {
    if (this.#discardStarted || this.#closePromise !== undefined) {
      throw new DOMException('Fresh-page FSA discard authority was already consumed', 'InvalidStateError')
    }
    this.#discardStarted = true
    await this.#verifyBinding()
    if (this.#binding.reservation.entryKind === 'result-root') {
      await this.#tree.reopenOwnedRootForCleanup()
    }

    const [committed, candidates, checkpointHandles, operationHandles] = await Promise.all([
      scanAllCheckpoints(this.#checkpoints, 'committed'),
      scanAllCheckpoints(this.#checkpoints, 'candidates'),
      this.#checkpoints.listHandles(),
      requireOperationHandleInventory(this.#operationRepository, this.intent.operationId)
        .listHandles(this.intent.operationId),
    ])
    const objects = await this.#verifyCheckpointObjects(
      checkpointObjects(this.intent, committed, candidates),
      checkpointHandles,
    )
    const directories = await this.#verifyNamespaceInventory(objects, operationHandles)

    const removedObjectIds: string[] = []
    for (const object of objects) {
      if (object.successful !== undefined) continue
      await cleanupMutation(this.intent.operationId, async () => {
        await this.#tree.removeFile(object.path, object.ownedObjectId)
      })
      removedObjectIds.push(object.ownedObjectId)
    }

    const successful = objects.flatMap(object =>
      object.successful === undefined ? [] : [object.successful])
    const removedDirectoryHandleIds: string[] = []
    const removableDirectories = directories
      .filter(directory => !successful.some(checkpoint =>
        pathStartsWith(checkpoint.canonicalPath, directory.path)))
      .sort((left, right) => right.path.length - left.path.length ||
        comparePaths(right.path, left.path))
    for (const directory of removableDirectories) {
      await cleanupMutation(this.intent.operationId, async () => {
        await this.#tree.removeDirectory(directory.path, directory.ownedObjectId)
      })
      removedObjectIds.push(directory.ownedObjectId)
      removedDirectoryHandleIds.push(directory.handleId)
    }

    return Object.freeze({
      committedCheckpoints: committed,
      candidateCheckpoints: candidates,
      successfulCheckpoints: Object.freeze(successful),
      removedObjectIds: Object.freeze(removedObjectIds.sort()),
      removedDirectoryHandleIds: Object.freeze(removedDirectoryHandleIds.sort()),
      retireCheckpoints: () => this.#checkpoints.retireOperation(),
    })
  }

  close(): Promise<void> {
    this.#closePromise ??= this.#close()
    return this.#closePromise
  }

  async #verifyBinding(): Promise<void> {
    const verified = await verifyFSAOperationBinding({
      repository: this.#operationRepository,
      intent: this.intent,
      expectedParent: this.#binding.parent,
    })
    if (verified.parentHandleId !== this.#binding.parentHandleId ||
        verified.reservation.digest !== this.#binding.reservation.digest) {
      throw new TargetOwnershipUnknownError('reservation', this.intent.operationId)
    }
  }

  async #verifyCheckpointObjects(
    objects: readonly CheckpointObject[],
    handles: readonly PersistentHandleRecord[],
  ): Promise<readonly OwnedCheckpointObject[]> {
    const handleByObject = checkpointHandleInventory(this.intent, this.#binding, handles)
    if (handleByObject.size !== objects.length) {
      throw new TargetOwnershipUnknownError('cleanup', this.intent.operationId)
    }

    const observed: OwnedCheckpointObject[] = []
    for (const object of objects) {
      const persisted = handleByObject.get(object.ownedObjectId)
      if (persisted === undefined) {
        throw new TargetOwnershipUnknownError('cleanup', this.intent.operationId)
      }
      observed.push(await this.#verifyCheckpointObject(object, persisted))
    }
    return Object.freeze(observed)
  }

  async #verifyCheckpointObject(
    object: CheckpointObject,
    persisted: PersistentHandleRecord,
  ): Promise<OwnedCheckpointObject> {
    const file = await this.#tree.openFile(object.path, object.ownedObjectId)
    if (file === undefined) {
      throw new TargetOwnershipUnknownError('cleanup', this.intent.operationId)
    }
    try {
      const size = await file.size()
      for (const checkpoint of object.records) {
        verifyCheckpointFileSize(checkpoint, size, this.intent.operationId)
      }
      await file.verify(object.successful === undefined ? 'checkpoint' : 'commit')
    } finally {
      await file.close()
    }
    return Object.freeze({ ...object, persistedHandle: persisted.handle as FileSystemFileHandle })
  }

  async #verifyNamespaceInventory(
    objects: readonly OwnedCheckpointObject[],
    handles: readonly ReceiveOperationHandleRecord[],
  ): Promise<readonly OwnedDirectoryObservation[]> {
    const parent = handles.find(handle => handle.id === this.#binding.parentHandleId)
    if (parent === undefined || handles.filter(handle =>
      handle.id === this.#binding.parentHandleId).length !== 1 ||
        parent.operationId !== this.intent.operationId ||
        parent.kind !== FSA_OPERATION_HANDLE_PARENT ||
        parent.authorityRef !== this.#binding.reservation.authorityRef ||
        parent.ownedObjectId !== undefined || !isDirectoryHandle(parent.handle) ||
        !await sameEntryForCleanup(parent.handle, this.#binding.parent, this.intent.operationId)) {
      throw new TargetOwnershipUnknownError('parent-authority', this.intent.operationId)
    }
    const directoryById = new Map<string, ReceiveOperationHandleRecord>()
    const directoryObjectIds = new Set<string>()
    for (const handle of handles) {
      if (handle.id === this.#binding.parentHandleId) continue
      if (handle.operationId !== this.intent.operationId ||
          handle.kind !== FSA_OPERATION_HANDLE_DIRECTORY ||
          handle.authorityRef !== this.#binding.reservation.authorityRef ||
          handle.ownedObjectId === undefined || directoryById.has(handle.id) ||
          directoryObjectIds.has(handle.ownedObjectId) || !isDirectoryHandle(handle.handle)) {
        throw new TargetOwnershipUnknownError('cleanup', this.intent.operationId)
      }
      directoryById.set(handle.id, handle)
      directoryObjectIds.add(handle.ownedObjectId)
    }

    if (this.#binding.reservation.entryKind === 'single-file') {
      if (directoryById.size !== 0 || objects.length > 1 ||
          objects.some(object => !samePath(object.path, [this.#binding.reservation.reservedName]))) {
        throw new TargetOwnershipUnknownError('cleanup', this.intent.operationId)
      }
      if (objects.length === 0) await requireReservedEntryAbsent(this.#binding)
      return Object.freeze([])
    }

    const root = await requireResultRoot(this.#binding)
    const fileByPath = new Map(objects.map(object => [pathKey(object.path), object]))
    const seenFiles = new Set<string>()
    const seenDirectories = new Set<string>()
    const directories: OwnedDirectoryObservation[] = []
    await walkOwnedDirectory({
      tree: this.#tree,
      binding: this.#binding,
      directory: root,
      path: Object.freeze([]),
      directoryById,
      fileByPath,
      seenDirectories,
      seenFiles,
      directories,
    })
    if (seenDirectories.size !== directoryById.size || seenFiles.size !== fileByPath.size) {
      throw new TargetOwnershipUnknownError('cleanup', this.intent.operationId)
    }
    return Object.freeze(directories)
  }

  async #close(): Promise<void> {
    this.#checkpoints.close()
    await this.#rootLease.release()
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
    checkpoints = await checkpointRepository(options, intent, reservation)
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
    if (error instanceof TargetOwnershipUnknownError) {
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
    if (error instanceof TargetOwnershipUnknownError) {
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
    checkpoints = await checkpointRepository(options, intent, binding.reservation)
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
    if (error instanceof TargetOwnershipUnknownError) {
      emitFSAOutputTrace(options.trace, needsAttention(intent.operationId))
    }
    checkpoints?.close()
    await rootLease.release().catch(() => undefined)
    throw error
  }
}

export async function openFreshPageFileSystemAccessDiscard(
  options: OpenFreshPageFileSystemAccessDiscardOptions,
): Promise<FreshPageFileSystemAccessDiscardSession> {
  const intent = await validateReceiveIntent(options.intent)
  if (intent.plan.kind !== 'direct-tree' || intent.artifact.kind !== 'directory-tree' ||
      options.binding.intent.operationId !== intent.operationId ||
      options.binding.intent.digest !== intent.digest ||
      options.binding.reservation.digest !== intent.plan.reservation.digest) {
    throw new TypeError('Fresh-page FSA discard requires one validated DirectTree binding')
  }
  const firstBinding = await verifyFSAOperationBinding({
    repository: options.operationRepository,
    intent,
    expectedParent: options.binding.parent,
  })
  if (firstBinding.parentHandleId !== options.binding.parentHandleId) {
    throw new TargetOwnershipUnknownError('parent-authority', intent.operationId)
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
    if (binding.parentHandleId !== options.binding.parentHandleId ||
        binding.reservation.digest !== options.binding.reservation.digest) {
      throw new TargetOwnershipUnknownError('reservation', intent.operationId)
    }
    checkpoints = await checkpointRepository(options, intent, binding.reservation)
    const tree = new BrowserFileSystemTree({
      binding,
      operationRepository: options.operationRepository,
      fileHandles: checkpoints,
      mutations: rootLease.authority,
    })
    return new FreshPageFileSystemAccessDiscardSession({
      intent,
      binding,
      operationRepository: options.operationRepository,
      tree,
      checkpoints,
      rootLease,
    })
  } catch (error) {
    checkpoints?.close()
    await rootLease.release().catch(() => undefined)
    throw error
  }
}

async function scanAllCheckpoints(
  journal: FileCheckpointJournal,
  source: 'committed' | 'candidates',
): Promise<readonly FileCheckpointV2[]> {
  const records: FileCheckpointV2[] = []
  let cursor: string | undefined
  do {
    const scan = {
      direction: 'ascending' as const,
      ...(cursor === undefined ? {} : { cursor }),
    }
    const page = validateFileCheckpointPage(
      source === 'committed'
        ? await journal.scanCommitted(scan)
        : await journal.scanCandidates(scan),
      scan,
      journal.binding,
    )
    records.push(...page.records)
    cursor = page.nextCursor
  } while (cursor !== undefined)
  return Object.freeze(records)
}

async function checkpointRepository(
  options: Pick<
    BindNewFileSystemAccessOutputOptions | OpenFreshPageFileSystemAccessDiscardOptions,
    'checkpointRepositoryFactory' | 'databaseName'
  >,
  intent: ReceiveIntent,
  reservation: NamedContainerEntryReservation,
): Promise<FSAFileCheckpointRepository> {
  const binding = durableCheckpointNamespaceIdentity({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: reservation.digest,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: reservation.authorityRef,
  })
  if (options.checkpointRepositoryFactory !== undefined) {
    return options.checkpointRepositoryFactory(binding)
  }
  return options.databaseName === undefined
    ? IndexedDbFileCheckpointRepository.open(binding)
    : IndexedDbFileCheckpointRepository.open(binding, options.databaseName)
}

interface CheckpointObject {
  readonly path: readonly string[]
  readonly ownedObjectId: string
  readonly records: readonly FileCheckpointV2[]
  readonly successful?: FileCheckpointV2
}

interface OwnedCheckpointObject extends CheckpointObject {
  readonly persistedHandle: FileSystemFileHandle
}

interface OwnedDirectoryObservation {
  readonly path: readonly string[]
  readonly ownedObjectId: string
  readonly handleId: string
}

interface CheckpointObjectGroup {
  readonly committed: FileCheckpointV2[]
  readonly candidates: FileCheckpointV2[]
}

function checkpointObjects(
  intent: ReceiveIntent,
  committed: readonly FileCheckpointV2[],
  candidates: readonly FileCheckpointV2[],
): readonly CheckpointObject[] {
  const groups = checkpointObjectGroups(intent, committed, candidates)
  const paths = new Set<string>()
  const objects: CheckpointObject[] = []
  for (const [ownedObjectId, group] of groups) {
    const object = checkpointObject(intent.operationId, ownedObjectId, group)
    const key = pathKey(object.path)
    if (paths.has(key)) throw new TargetOwnershipUnknownError('cleanup', intent.operationId)
    paths.add(key)
    objects.push(object)
  }
  return Object.freeze(objects.sort((left, right) => comparePaths(left.path, right.path)))
}

function checkpointObjectGroups(
  intent: ReceiveIntent,
  committed: readonly FileCheckpointV2[],
  candidates: readonly FileCheckpointV2[],
): ReadonlyMap<string, CheckpointObjectGroup> {
  const groups = new Map<string, CheckpointObjectGroup>()
  appendCheckpointGroups(intent, groups, 'committed', committed)
  appendCheckpointGroups(intent, groups, 'candidates', candidates)
  return groups
}

function appendCheckpointGroups(
  intent: ReceiveIntent,
  groups: Map<string, CheckpointObjectGroup>,
  source: keyof CheckpointObjectGroup,
  records: readonly FileCheckpointV2[],
): void {
  if (intent.plan.kind !== 'direct-tree') {
    throw new TargetOwnershipUnknownError('cleanup', intent.operationId)
  }
  for (const record of records) {
    if (record.operationId !== intent.operationId || record.receiveIntentDigest !== intent.digest ||
        record.materializationBindingDigest !== intent.plan.reservation.digest) {
      throw new TargetOwnershipUnknownError('cleanup', intent.operationId)
    }
    const group = groups.get(record.ownedObjectId) ?? { committed: [], candidates: [] }
    const authority = group.committed[0] ?? group.candidates[0]
    if (authority !== undefined && !sameCheckpointObject(authority, record)) {
      throw new TargetOwnershipUnknownError('cleanup', intent.operationId)
    }
    group[source].push(record)
    groups.set(record.ownedObjectId, group)
  }
}

function checkpointObject(
  operationId: string,
  ownedObjectId: string,
  group: CheckpointObjectGroup,
): CheckpointObject {
  if (group.committed.length > 1 || group.candidates.length > 1) {
    throw new TargetOwnershipUnknownError('cleanup', operationId)
  }
  const authority = group.committed[0] ?? group.candidates[0]
  if (authority === undefined) throw new TargetOwnershipUnknownError('cleanup', operationId)
  const committed = group.committed[0]
  if (committed !== undefined && fileCheckpointIsComplete(committed) &&
      group.candidates.length !== 0) {
    throw new TargetOwnershipUnknownError('cleanup', operationId)
  }
  return Object.freeze({
    path: authority.canonicalPath,
    ownedObjectId,
    records: Object.freeze([...group.committed, ...group.candidates]),
    ...(committed !== undefined && fileCheckpointIsComplete(committed)
      ? { successful: committed }
      : {}),
  })
}

function checkpointHandleInventory(
  intent: ReceiveIntent,
  binding: PersistedFSAOperationBinding,
  handles: readonly PersistentHandleRecord[],
): ReadonlyMap<string, PersistentHandleRecord> {
  const inventory = new Map<string, PersistentHandleRecord>()
  const handleIds = new Set<string>()
  for (const record of handles) {
    if (record.operationId !== intent.operationId || record.kind !== FSA_FILE_HANDLE_KIND ||
        record.authorityRef !== binding.reservation.authorityRef ||
        handleIds.has(record.id) || inventory.has(record.ownedObjectId) ||
        !isFileHandle(record.handle)) {
      throw new TargetOwnershipUnknownError('cleanup', intent.operationId)
    }
    handleIds.add(record.id)
    inventory.set(record.ownedObjectId, record)
  }
  return inventory
}

function verifyCheckpointFileSize(
  checkpoint: FileCheckpointV2,
  size: bigint,
  operationId: string,
): void {
  const durableEnd = checkpoint.verifiedRanges.at(-1)?.end ?? 0n
  const completeSize = fileCheckpointIsComplete(checkpoint) ? checkpoint.exactSize : undefined
  if (size < durableEnd || size > checkpoint.exactSize ||
      (completeSize !== undefined && size !== completeSize)) {
    throw new TargetOwnershipUnknownError('cleanup', operationId)
  }
}

function sameCheckpointObject(left: FileCheckpointV2, right: FileCheckpointV2): boolean {
  return left.recordId === right.recordId && left.fileId === right.fileId &&
    left.fileRevision === right.fileRevision && left.exactSize === right.exactSize &&
    left.ownedObjectId === right.ownedObjectId && left.authorityRef === right.authorityRef &&
    left.materializerKind === right.materializerKind && samePath(left.canonicalPath, right.canonicalPath)
}

function requireOperationHandleInventory(
  repository: ReceiveOperationRepository,
  operationId: string,
): ReceiveOperationHandleInventoryRepository {
  if (typeof (repository as Partial<ReceiveOperationHandleInventoryRepository>).listHandles !==
      'function') {
    throw new TargetOwnershipUnknownError('cleanup', operationId)
  }
  return repository as ReceiveOperationHandleInventoryRepository
}

async function walkOwnedDirectory(input: Readonly<{
  tree: BrowserFileSystemTree
  binding: PersistedFSAOperationBinding
  directory: FileSystemDirectoryHandle
  path: readonly string[]
  directoryById: ReadonlyMap<string, ReceiveOperationHandleRecord>
  fileByPath: ReadonlyMap<string, OwnedCheckpointObject>
  seenDirectories: Set<string>
  seenFiles: Set<string>
  directories: OwnedDirectoryObservation[]
}>): Promise<void> {
  const operationId = input.binding.intent.operationId
  let handleId: string
  try {
    handleId = await fsaDirectoryHandleId(input.binding.reservation, input.path)
  } catch (cause) {
    throw new TargetOwnershipUnknownError('cleanup', operationId, { cause })
  }
  const record = input.directoryById.get(handleId)
  if (record?.ownedObjectId === undefined || input.seenDirectories.has(handleId) ||
      !await input.tree.validateDirectory(input.path, record.ownedObjectId)) {
    throw new TargetOwnershipUnknownError('cleanup', operationId)
  }
  input.seenDirectories.add(handleId)
  input.directories.push(Object.freeze({
    path: input.path,
    ownedObjectId: record.ownedObjectId,
    handleId,
  }))

  const enumerable = input.directory as FileSystemDirectoryHandle & {
    entries?: () => AsyncIterableIterator<[string, FileSystemHandle]>
  }
  if (typeof enumerable.entries !== 'function') {
    throw new TargetOwnershipUnknownError('cleanup', operationId)
  }
  try {
    for await (const [name, handle] of enumerable.entries()) {
      const path = Object.freeze([...input.path, name])
      if (handle.kind === 'directory') {
        await walkOwnedDirectory({ ...input, directory: handle as FileSystemDirectoryHandle, path })
        continue
      }
      if (handle.kind !== 'file') {
        throw new TargetOwnershipUnknownError('cleanup', operationId)
      }
      const key = pathKey(path)
      const object = input.fileByPath.get(key)
      if (object === undefined || input.seenFiles.has(key) ||
          !await sameEntryForCleanup(handle, object.persistedHandle, operationId)) {
        throw new TargetOwnershipUnknownError('cleanup', operationId)
      }
      input.seenFiles.add(key)
    }
  } catch (cause) {
    if (cause instanceof TargetOwnershipUnknownError) throw cause
    throw new TargetOwnershipUnknownError('cleanup', operationId, { cause })
  }
}

async function requireResultRoot(
  binding: PersistedFSAOperationBinding,
): Promise<FileSystemDirectoryHandle> {
  try {
    return await binding.parent.getDirectoryHandle(binding.reservation.reservedName)
  } catch (cause) {
    throw new TargetOwnershipUnknownError('parent-authority', binding.intent.operationId, { cause })
  }
}

async function requireReservedEntryAbsent(binding: PersistedFSAOperationBinding): Promise<void> {
  for (const kind of ['file', 'directory'] as const) {
    try {
      if (kind === 'file') await binding.parent.getFileHandle(binding.reservation.reservedName)
      else await binding.parent.getDirectoryHandle(binding.reservation.reservedName)
      throw new TargetOwnershipUnknownError('cleanup', binding.intent.operationId)
    } catch (cause) {
      if (cause instanceof TargetOwnershipUnknownError) throw cause
      if (!errorNamed(cause, 'NotFoundError')) {
        throw new TargetOwnershipUnknownError('cleanup', binding.intent.operationId, { cause })
      }
    }
  }
}

async function sameEntryForCleanup(
  left: FileSystemHandle,
  right: FileSystemHandle,
  operationId: string,
): Promise<boolean> {
  try {
    return await left.isSameEntry(right)
  } catch (cause) {
    throw new TargetOwnershipUnknownError('cleanup', operationId, { cause })
  }
}

async function cleanupMutation(operationId: string, mutation: () => Promise<void>): Promise<void> {
  try {
    await mutation()
  } catch (cause) {
    if (cause instanceof TargetOwnershipUnknownError) throw cause
    throw new TargetOwnershipUnknownError('cleanup', operationId, { cause })
  }
}

function isDirectoryHandle(value: unknown): value is FileSystemDirectoryHandle {
  return typeof value === 'object' && value !== null && 'kind' in value &&
    (value as { readonly kind?: unknown }).kind === 'directory' && 'isSameEntry' in value &&
    typeof (value as { readonly isSameEntry?: unknown }).isSameEntry === 'function'
}

function isFileHandle(value: unknown): value is FileSystemFileHandle {
  return typeof value === 'object' && value !== null && 'kind' in value &&
    (value as { readonly kind?: unknown }).kind === 'file' && 'isSameEntry' in value &&
    typeof (value as { readonly isSameEntry?: unknown }).isSameEntry === 'function'
}

function pathStartsWith(path: readonly string[], prefix: readonly string[]): boolean {
  return prefix.length < path.length && prefix.every((segment, index) => path[index] === segment)
}

function pathKey(path: readonly string[]): string {
  return JSON.stringify(path)
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((segment, index) => segment === right[index])
}

function comparePaths(left: readonly string[], right: readonly string[]): number {
  const length = Math.min(left.length, right.length)
  for (let index = 0; index < length; index += 1) {
    if (left[index] === right[index]) continue
    return left[index]! < right[index]! ? -1 : 1
  }
  return left.length - right.length
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
