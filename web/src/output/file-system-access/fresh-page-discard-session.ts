import {
  validateReceiveIntent,
  type ReceiveIntent,
} from '../../transfer/intent'
import {
  BrowserFileSystemTree,
  FSA_FILE_HANDLE_KIND,
  fsaDirectoryHandleId,
} from '../browser/filesystem-tree'
import {
  FSA_OPERATION_HANDLE_DIRECTORY,
  FSA_OPERATION_HANDLE_PARENT,
  verifyFSAOperationBinding,
  type PersistedFSAOperationBinding,
} from '../browser/indexeddb-root-binding'
import {
  acquireFSARootMutationLease,
  type BrowserLockManagerRuntime,
  type FSARootMutationLease,
} from '../browser/namespace-mutation'
import {
  fileCheckpointIsComplete,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import type { PersistentHandleRecord } from '../persistence/journal'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type { ReceiveOperationHandleRecord } from '../workspace/records'
import type {
  ReceiveOperationHandleInventoryRepository,
  ReceiveOperationRepository,
} from '../workspace/repository'
import {
  openFSAFileCheckpointRepository,
  scanAllFSAFileCheckpoints,
  type FSAFileCheckpointRepository,
  type FSAFileCheckpointRepositoryFactory,
} from './checkpoint-repository'

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
      scanAllFSAFileCheckpoints(this.#checkpoints, 'committed'),
      scanAllFSAFileCheckpoints(this.#checkpoints, 'candidates'),
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
    checkpoints = await openFSAFileCheckpointRepository(options, intent, binding.reservation)
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

function errorNamed(error: unknown, name: string): boolean {
  return typeof error === 'object' && error !== null &&
    'name' in error && (error as { readonly name?: unknown }).name === name
}
