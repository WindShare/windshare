import { validateReceiveIntent, type ReceiveIntent } from '../../transfer/intent'
import { BrowserFileSystemTree, FSA_FILE_HANDLE_KIND, fsaDirectoryHandleId } from '../browser/filesystem-tree'
import {
  FSA_OPERATION_HANDLE_DIRECTORY,
  FSA_OPERATION_HANDLE_PARENT,
  verifyFSAOperationBinding,
  type PersistedFSAOperationBinding,
} from '../browser/indexeddb-root-binding'
import {
  acquireFSARootMutationLease,
  type BrowserLockManagerRuntime,
  type FSAHandleIdentityResolver,
  type FSARootMutationLease,
} from '../browser/namespace-mutation'
import type { FSATerminalExclusiveAuthority } from '../browser/mutation-coordination/model'
import { fileCheckpointIsComplete, type FileCheckpointV2 } from '../persistence/checkpoint'
import type { PersistentHandleRecord } from '../persistence/journal'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type { ReceiveOperationHandleRecord } from '../workspace/records'
import type { ReceiveOperationHandleInventoryRepository, ReceiveOperationRepository } from '../workspace/repository'
import {
  openFSAFileCheckpointRepository,
  scanAllFSAFileCheckpoints,
  type FSAFileCheckpointRepository,
  type FSAFileCheckpointRepositoryFactory,
} from './checkpoint-repository'
import { retireFSAMaterializationRecoveryMetadata } from './recovery-metadata-retirement'
import { isSafeComponentName } from './safe-name'

export interface OpenFreshPageFileSystemAccessDiscardOptions {
  readonly intent: ReceiveIntent
  readonly binding: PersistedFSAOperationBinding
  readonly operationRepository: ReceiveOperationRepository
  readonly lockManager?: BrowserLockManagerRuntime
  readonly mutationIdentities?: FSAHandleIdentityResolver
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
}

export interface FSAFreshPageDiscardCut {
  readonly committedCheckpoints: readonly FileCheckpointV2[]
  readonly candidateCheckpoints: readonly FileCheckpointV2[]
  readonly successfulCheckpoints: readonly FileCheckpointV2[]
  readonly removedObjectIds: readonly string[]
  readonly removedHandleIds: readonly string[]
  retireRecoveryMetadata(): Promise<void>
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

interface RemovableDirectoryObservation {
  readonly parentHandle: FileSystemDirectoryHandle
  readonly physicalName: string
  readonly ownedObjectId: string
  readonly handleId: string
  readonly persistedHandle: FileSystemDirectoryHandle
  readonly isRoot: boolean
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
    await this.#tree.reopenOwnedRootForCleanup()
    const terminal = this.#rootLease.scheduler.beginTerminal('discard-operation')
    await terminal.drained

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
    const removableDirectories = await this.#verifyNamespaceInventory(objects, operationHandles)

    return terminal.runExclusive(authority => this.#discardVerifiedObjects({
      authority,
      committed,
      candidates,
      objects,
      removableDirectories,
    }))
  }

  async #discardVerifiedObjects(input: Readonly<{
    authority: FSATerminalExclusiveAuthority
    committed: readonly FileCheckpointV2[]
    candidates: readonly FileCheckpointV2[]
    objects: readonly OwnedCheckpointObject[]
    removableDirectories: readonly RemovableDirectoryObservation[]
  }>): Promise<FSAFreshPageDiscardCut> {
    const removedObjectIds: string[] = []
    for (const object of input.objects) {
      if (object.successful !== undefined) continue
      await cleanupMutation(this.intent.operationId, async () => {
        await this.#tree.removeFileWithinTerminal(input.authority, object.path, object.ownedObjectId)
      })
      removedObjectIds.push(object.ownedObjectId)
    }

    const successful = input.objects.flatMap(object =>
      object.successful === undefined ? [] : [object.successful])
    const removedHandleIds: string[] = []
    for (const directory of input.removableDirectories) {
      await cleanupMutation(this.intent.operationId, async () => {
        if (directory.isRoot) await this.#verifyBinding()
        const current = await directory.parentHandle.getDirectoryHandle(directory.physicalName)
        if (!await sameEntryForCleanup(current, directory.persistedHandle, this.intent.operationId)) {
          throw new TargetOwnershipUnknownError('cleanup', this.intent.operationId)
        }
        await directory.parentHandle.removeEntry(directory.physicalName, { recursive: true })
      })
      removedObjectIds.push(directory.ownedObjectId)
      removedHandleIds.push(directory.handleId)
    }

    return Object.freeze({
      committedCheckpoints: input.committed,
      candidateCheckpoints: input.candidates,
      successfulCheckpoints: Object.freeze(successful),
      removedObjectIds: Object.freeze(removedObjectIds.sort()),
      removedHandleIds: Object.freeze(removedHandleIds.sort()),
      retireRecoveryMetadata: () => retireFSAMaterializationRecoveryMetadata(this.#checkpoints),
    })
  }

  close(): Promise<void> {
    this.#closePromise ??= this.#close()
    return this.#closePromise
  }

  async #verifyBinding(): Promise<void> {
    await verifyExpectedBinding(this.#operationRepository, this.intent, this.#binding)
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
  ): Promise<readonly RemovableDirectoryObservation[]> {
    await verifyPersistedParentAuthority(this.#binding, handles)
    const directoryById = ownedDirectoryHandleInventory(this.#binding, handles)

    if (this.#binding.reservation.entryKind === 'single-file') {
      // The reserved file is the FSA materialization root; accepting its logical name here
      // would silently revive the retired pre-fence checkpoint coordinate.
      if (directoryById.size !== 0 || objects.length > 1 ||
          objects.some(object => object.path.length !== 0)) {
        throw new TargetOwnershipUnknownError('cleanup', this.intent.operationId)
      }
      if (objects.length === 0) await requireReservedEntryAbsent(this.#binding)
      return Object.freeze([])
    }

    const root = await requireResultRoot(this.#binding)
    const rootHandleId = await fsaDirectoryHandleId(this.#binding.reservation, [])
    const rootRecord = directoryById.get(rootHandleId)
    if (rootRecord?.ownedObjectId === undefined ||
        !await this.#tree.validateDirectory([], rootRecord.ownedObjectId)) {
      throw new TargetOwnershipUnknownError('cleanup', this.intent.operationId)
    }
    const walkContext: OwnedDirectoryWalkContext = {
      tree: this.#tree,
      binding: this.#binding,
      directoryById,
      directoriesByPhysicalName: groupByPhysicalName(
        [...directoryById.values()],
        record => (record.handle as FileSystemDirectoryHandle).name,
      ),
      fileByPath: new Map(objects.map(object => [pathKey(object.path), object])),
      filesByPhysicalName: groupByPhysicalName(objects, object => object.persistedHandle.name),
      seenDirectories: new Set<string>(),
      seenFiles: new Set<string>(),
      removableDirectories: [],
    }
    await walkOwnedDirectory(walkContext, {
      parentHandle: this.#binding.parent,
      physicalName: this.#binding.reservation.physicalName,
      directory: root,
      record: rootRecord,
      path: Object.freeze([]),
      isRoot: true,
    })
    if (walkContext.seenDirectories.size !== directoryById.size ||
        walkContext.seenFiles.size !== objects.length) {
      throw new TargetOwnershipUnknownError('cleanup', this.intent.operationId)
    }
    return Object.freeze(walkContext.removableDirectories)
  }

  async #close(): Promise<void> {
    this.#checkpoints.close()
    await this.#rootLease.release()
  }
}

async function verifyExpectedBinding(
  repository: ReceiveOperationRepository,
  intent: ReceiveIntent,
  expected: PersistedFSAOperationBinding,
  parentOverride?: FileSystemDirectoryHandle,
): Promise<PersistedFSAOperationBinding> {
  const verified = await verifyFSAOperationBinding({
    repository,
    intent,
    expectedParent: parentOverride ?? expected.parent,
  })
  if (verified.parentHandleId !== expected.parentHandleId ||
      verified.reservation.digest !== expected.reservation.digest) {
    throw new TargetOwnershipUnknownError('reservation', intent.operationId)
  }
  return verified
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
  const rootLease = await acquireFSARootMutationLease(
    firstBinding.parent,
    options.lockManager,
    undefined,
    undefined,
    options.mutationIdentities,
  )
  let checkpoints: FSAFileCheckpointRepository | undefined
  try {
    const binding = await verifyExpectedBinding(
      options.operationRepository,
      intent,
      options.binding,
      firstBinding.parent,
    )
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
  const groups = new Map<string, CheckpointObjectGroup>()
  appendCheckpointGroups(intent, groups, 'committed', committed)
  appendCheckpointGroups(intent, groups, 'candidates', candidates)
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

async function verifyPersistedParentAuthority(
  binding: PersistedFSAOperationBinding,
  handles: readonly ReceiveOperationHandleRecord[],
): Promise<void> {
  const parentRecords = handles.filter(handle => handle.id === binding.parentHandleId)
  const parent = parentRecords[0]
  if (parentRecords.length !== 1 || parent === undefined ||
      parent.operationId !== binding.intent.operationId ||
      parent.kind !== FSA_OPERATION_HANDLE_PARENT ||
      parent.authorityRef !== binding.reservation.authorityRef ||
      parent.ownedObjectId !== undefined || !isDirectoryHandle(parent.handle) ||
      !await sameEntryForCleanup(parent.handle, binding.parent, binding.intent.operationId)) {
    throw new TargetOwnershipUnknownError('parent-authority', binding.intent.operationId)
  }
}

function ownedDirectoryHandleInventory(
  binding: PersistedFSAOperationBinding,
  handles: readonly ReceiveOperationHandleRecord[],
): ReadonlyMap<string, ReceiveOperationHandleRecord> {
  const directoryById = new Map<string, ReceiveOperationHandleRecord>()
  const directoryObjectIds = new Set<string>()
  for (const handle of handles) {
    if (handle.id === binding.parentHandleId) continue
    if (handle.operationId !== binding.intent.operationId ||
        handle.kind !== FSA_OPERATION_HANDLE_DIRECTORY ||
        handle.authorityRef !== binding.reservation.authorityRef ||
        handle.ownedObjectId === undefined || directoryById.has(handle.id) ||
        directoryObjectIds.has(handle.ownedObjectId) || !isDirectoryHandle(handle.handle)) {
      throw new TargetOwnershipUnknownError('cleanup', binding.intent.operationId)
    }
    directoryById.set(handle.id, handle)
    directoryObjectIds.add(handle.ownedObjectId)
  }
  return directoryById
}

interface OwnedDirectoryWalkContext {
  readonly tree: BrowserFileSystemTree
  readonly binding: PersistedFSAOperationBinding
  readonly directoryById: ReadonlyMap<string, ReceiveOperationHandleRecord>
  readonly directoriesByPhysicalName: ReadonlyMap<string, readonly ReceiveOperationHandleRecord[]>
  readonly fileByPath: ReadonlyMap<string, OwnedCheckpointObject>
  readonly filesByPhysicalName: ReadonlyMap<string, readonly OwnedCheckpointObject[]>
  readonly seenDirectories: Set<string>
  readonly seenFiles: Set<string>
  readonly removableDirectories: RemovableDirectoryObservation[]
}

interface OwnedDirectoryNode {
  readonly parentHandle: FileSystemDirectoryHandle
  readonly physicalName: string
  readonly directory: FileSystemDirectoryHandle
  readonly record: ReceiveOperationHandleRecord
  readonly path: readonly string[] | undefined
  readonly isRoot: boolean
}

async function walkOwnedDirectory(
  context: OwnedDirectoryWalkContext,
  node: OwnedDirectoryNode,
): Promise<boolean> {
  const operationId = context.binding.intent.operationId
  const { record } = node
  if (record.ownedObjectId === undefined || context.seenDirectories.has(record.id)) {
    throw new TargetOwnershipUnknownError('cleanup', operationId)
  }
  context.seenDirectories.add(record.id)

  const enumerable = node.directory as FileSystemDirectoryHandle & {
    entries?: () => AsyncIterableIterator<[string, FileSystemHandle]>
  }
  if (typeof enumerable.entries !== 'function') {
    throw new TargetOwnershipUnknownError('cleanup', operationId)
  }
  let hasKeptDescendant = false
  try {
    for await (const [name, handle] of enumerable.entries()) {
      if (await walkOwnedEntry(context, node, name, handle, operationId)) {
        hasKeptDescendant = true
      }
    }
  } catch (cause) {
    if (cause instanceof TargetOwnershipUnknownError) throw cause
    throw new TargetOwnershipUnknownError('cleanup', operationId, { cause })
  }
  if (!hasKeptDescendant) {
    context.removableDirectories.push(Object.freeze({
      parentHandle: node.parentHandle,
      physicalName: node.physicalName,
      ownedObjectId: record.ownedObjectId,
      handleId: record.id,
      persistedHandle: record.handle as FileSystemDirectoryHandle,
      isRoot: node.isRoot,
    }))
  }
  return hasKeptDescendant
}

async function walkOwnedEntry(
  context: OwnedDirectoryWalkContext,
  parent: OwnedDirectoryNode,
  name: string,
  handle: FileSystemHandle,
  operationId: string,
): Promise<boolean> {
  if (handle.kind === 'directory') {
    const matched = await matchOwnedDirectory(context, parent.path, name, handle as FileSystemDirectoryHandle, operationId)
    return walkOwnedDirectory(context, {
      parentHandle: parent.directory,
      physicalName: name,
      directory: handle as FileSystemDirectoryHandle,
      record: matched.record,
      path: matched.path,
      isRoot: false,
    })
  }
  if (handle.kind === 'file') {
    const matched = await matchOwnedFile(context, parent.path, name, handle as FileSystemFileHandle, operationId)
    context.seenFiles.add(matched.ownedObjectId)
    return matched.successful !== undefined
  }
  throw new TargetOwnershipUnknownError('cleanup', operationId)
}

async function matchOwnedDirectory(
  context: OwnedDirectoryWalkContext,
  parentPath: readonly string[] | undefined,
  physicalName: string,
  handle: FileSystemDirectoryHandle,
  operationId: string,
): Promise<Readonly<{ record: ReceiveOperationHandleRecord; path: readonly string[] | undefined }>> {
  if (parentPath !== undefined) {
    const logicalPath = Object.freeze([...parentPath, physicalName])
    let handleId: string | undefined
    try {
      handleId = await fsaDirectoryHandleId(context.binding.reservation, logicalPath)
    } catch {
      handleId = undefined
    }
    const direct = handleId === undefined ? undefined : context.directoryById.get(handleId)
    if (direct?.ownedObjectId !== undefined && !context.seenDirectories.has(direct.id) &&
        await context.tree.validateDirectory(logicalPath, direct.ownedObjectId) &&
        await sameEntryForCleanup(handle, direct.handle as FileSystemDirectoryHandle, operationId)) {
      return Object.freeze({ record: direct, path: logicalPath })
    }
    if (!isSafeComponentName(physicalName)) {
      throw new TargetOwnershipUnknownError('cleanup', operationId)
    }
  }
  // Safe-renamed directories (and their descendants) verify exact handle identity against
  // the persisted operation inventory rather than relying on a separate mapping ledger.
  for (const candidate of context.directoriesByPhysicalName.get(physicalName) ?? []) {
    if (context.seenDirectories.has(candidate.id)) continue
    if (await sameEntryForCleanup(handle, candidate.handle as FileSystemDirectoryHandle, operationId)) {
      return Object.freeze({ record: candidate, path: undefined })
    }
  }
  throw new TargetOwnershipUnknownError('cleanup', operationId)
}

async function matchOwnedFile(
  context: OwnedDirectoryWalkContext,
  parentPath: readonly string[] | undefined,
  physicalName: string,
  handle: FileSystemFileHandle,
  operationId: string,
): Promise<OwnedCheckpointObject> {
  if (parentPath !== undefined) {
    const direct = context.fileByPath.get(pathKey([...parentPath, physicalName]))
    if (direct !== undefined && !context.seenFiles.has(direct.ownedObjectId) &&
        await sameEntryForCleanup(handle, direct.persistedHandle, operationId)) {
      return direct
    }
  }
  for (const candidate of context.filesByPhysicalName.get(physicalName) ?? []) {
    if (context.seenFiles.has(candidate.ownedObjectId)) continue
    if (parentPath !== undefined && !samePath(candidate.path.slice(0, -1), parentPath)) continue
    if (await sameEntryForCleanup(handle, candidate.persistedHandle, operationId)) {
      return candidate
    }
  }
  throw new TargetOwnershipUnknownError('cleanup', operationId)
}

function groupByPhysicalName<T>(
  items: readonly T[],
  nameOf: (item: T) => string,
): ReadonlyMap<string, readonly T[]> {
  const groups = new Map<string, T[]>()
  for (const item of items) {
    const name = nameOf(item)
    const bucket = groups.get(name) ?? []
    bucket.push(item)
    groups.set(name, bucket)
  }
  return groups
}

async function requireResultRoot(binding: PersistedFSAOperationBinding): Promise<FileSystemDirectoryHandle> {
  try {
    return await binding.parent.getDirectoryHandle(binding.reservation.physicalName)
  } catch (cause) {
    throw new TargetOwnershipUnknownError('parent-authority', binding.intent.operationId, { cause })
  }
}

async function requireReservedEntryAbsent(binding: PersistedFSAOperationBinding): Promise<void> {
  for (const kind of ['file', 'directory'] as const) {
    try {
      if (kind === 'file') await binding.parent.getFileHandle(binding.reservation.physicalName)
      else await binding.parent.getDirectoryHandle(binding.reservation.physicalName)
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

function isHandleKind<T extends FileSystemHandle>(value: unknown, kind: T['kind']): value is T {
  return typeof value === 'object' && value !== null &&
    (value as { readonly kind?: unknown }).kind === kind &&
    typeof (value as { readonly isSameEntry?: unknown }).isSameEntry === 'function'
}

const isDirectoryHandle = (value: unknown): value is FileSystemDirectoryHandle => isHandleKind(value, 'directory')
const isFileHandle = (value: unknown): value is FileSystemFileHandle => isHandleKind(value, 'file')
const pathKey = (path: readonly string[]): string => JSON.stringify(path)
const samePath = (left: readonly string[], right: readonly string[]): boolean =>
  left.length === right.length && left.every((segment, index) => segment === right[index])

function comparePaths(left: readonly string[], right: readonly string[]): number {
  const length = Math.min(left.length, right.length)
  for (let index = 0; index < length; index += 1) {
    if (left[index] === right[index]) continue
    return left[index]! < right[index]! ? -1 : 1
  }
  return left.length - right.length
}

const errorNamed = (error: unknown, name: string): boolean =>
  typeof error === 'object' && error !== null && (error as { readonly name?: unknown }).name === name
