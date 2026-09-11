import type { ObjectCapacity, ObjectGrowthRequest, ObjectGrowthReservation } from '../origin-private/object-capacity'
import { ORIGIN_PRIVATE_RAW_FILE_CONTAINER, ORIGIN_PRIVATE_DIRECTORY_OBJECT_CONTAINER, type OriginPrivateObjectContainer } from '../origin-private/workspace-root'
import type { OriginPrivateRawObjectRoot } from '../origin-private/workspace-tree'
import type { PersistentHandleRepository } from '../persistence/journal'
import type { DurableCheckpointNamespaceIdentity } from '../persistence/namespace'
import { TargetOwnershipUnknownError, type TargetOwnershipStage } from '../persistent-tree/errors'
import { snapshotIdentity } from '../workspace/canonical'
import { deliveryDigest, namespaceFields } from './codec'

const STAGING_ROOT_HANDLE_KIND = 5
const STAGING_ROOT_CREATION_KIND = 6
const STAGING_ROOT_ENTRY_PREFIX = 'windshare-file-staging-v1-'
const STAGING_ROOT_HANDLE_PREFIX = 'windshare/browser-delivery/root/v1/'

/** Immutable policy plus a persisted handle owns this child namespace independently of sender state. */
export class BrowserDeliveryStagingRoot implements OriginPrivateRawObjectRoot {
  readonly operationId: string
  readonly authorityRef: string
  readonly #parent: FileSystemDirectoryHandle
  readonly #root: FileSystemDirectoryHandle
  readonly #handles: PersistentHandleRepository
  readonly #capacity = new Map<string, ObjectCapacity>()
  readonly #parentOperationId: string

  private constructor(binding: DurableCheckpointNamespaceIdentity, parent: FileSystemDirectoryHandle, root: FileSystemDirectoryHandle, handles: PersistentHandleRepository, parentOperationId: string) {
    this.#parentOperationId = parentOperationId
    this.operationId = binding.operationId
    this.authorityRef = binding.authorityRef
    this.#parent = parent
    this.#root = root
    this.#handles = handles
  }

  static async open(input: {
    readonly binding: DurableCheckpointNamespaceIdentity
    readonly parentOperationId: string
    readonly parent: FileSystemDirectoryHandle
    readonly handles: PersistentHandleRepository
  }): Promise<BrowserDeliveryStagingRoot> {
    const id = STAGING_ROOT_HANDLE_PREFIX + input.binding.operationId
    const persisted = await input.handles.readHandle(id)
    let root = await optionalDirectory(input.parent, STAGING_ROOT_ENTRY_PREFIX + input.binding.operationId)
    if (persisted === undefined) root = await reserveStagingRoot(input, id, root)
    if (root === undefined) throw new TargetOwnershipUnknownError('parent-authority', input.binding.operationId)
    const result = new BrowserDeliveryStagingRoot(input.binding, input.parent, root, input.handles, input.parentOperationId)
    await result.authorize()
    await input.handles.deleteHandle(id + '/creation')
    return result
  }

  bindCapacity(objectId: string, capacity: ObjectCapacity): void {
    this.#capacity.set(snapshotIdentity(objectId, 32, 'staged object ID'), capacity)
  }

  async authorize(): Promise<void> {
    const record = await this.#handles.readHandle(STAGING_ROOT_HANDLE_PREFIX + this.operationId)
    const current = await optionalDirectory(this.#parent, STAGING_ROOT_ENTRY_PREFIX + this.operationId)
    if (record === undefined || record.operationId !== this.operationId || record.authorityRef !== this.authorityRef ||
        record.kind !== STAGING_ROOT_HANDLE_KIND || record.ownedObjectId !== this.authorityRef || current === undefined ||
        !await this.sameObject(current, this.#root, 'parent-authority') ||
        !await this.sameObject(current, record.handle as FileSystemDirectoryHandle, 'parent-authority')) {
      throw new TargetOwnershipUnknownError('parent-authority', this.operationId)
    }
  }

  async prepareContainers(): Promise<void> {
    await this.authorize()
    await this.#root.getDirectoryHandle(ORIGIN_PRIVATE_RAW_FILE_CONTAINER, { create: true })
    await this.#root.getDirectoryHandle(ORIGIN_PRIVATE_DIRECTORY_OBJECT_CONTAINER, { create: true })
    await this.authorize()
  }

  rootOwnedObjectId(): string { return this.authorityRef }

  async readObject(container: OriginPrivateObjectContainer, objectId: string, stage: TargetOwnershipStage): Promise<FileSystemFileHandle | undefined> {
    try { await this.authorize() }
    catch (cause) { throw new TargetOwnershipUnknownError(stage, this.operationId, { cause }) }
    const directory = await optionalDirectory(this.#root, container)
    if (directory === undefined) return undefined
    return optionalFile(directory, snapshotIdentity(objectId, 32, 'staged object ID'))
  }

  async createObject(container: OriginPrivateObjectContainer, objectId: string): Promise<FileSystemFileHandle> {
    await this.authorize()
    if (!this.#capacity.has(objectId)) throw new DOMException('Staging requires full-file capacity before allocation', 'InvalidStateError')
    const directory = await this.#root.getDirectoryHandle(container, { create: true })
    if (await optionalFile(directory, objectId) !== undefined) throw new TargetOwnershipUnknownError('namespace-create', this.operationId)
    return directory.getFileHandle(snapshotIdentity(objectId, 32, 'staged object ID'), { create: true })
  }

  async removeObject(container: OriginPrivateObjectContainer, objectId: string, expected: FileSystemFileHandle): Promise<'removed' | 'already-absent'> {
    const current = await this.readObject(container, objectId, 'cleanup')
    if (current === undefined || !await this.sameObject(current, expected, 'cleanup')) {
      throw new TargetOwnershipUnknownError('cleanup', this.operationId)
    }
    const directory = await this.#root.getDirectoryHandle(container)
    await directory.removeEntry(objectId)
    if (await optionalFile(directory, objectId) !== undefined) throw new TargetOwnershipUnknownError('cleanup', this.operationId)
    this.#capacity.delete(objectId)
    return 'removed'
  }

  async sameObject(current: FileSystemHandle, expected: FileSystemHandle, stage: TargetOwnershipStage): Promise<boolean> {
    try {
      return expected !== undefined && typeof expected.isSameEntry === 'function' && await current.isSameEntry(expected)
    } catch (cause) {
      throw new TargetOwnershipUnknownError(stage, this.operationId, { cause })
    }
  }

  reserveGrowth(input: Omit<ObjectGrowthRequest, 'operationId'>): Promise<ObjectGrowthReservation> {
    const capacity = this.#capacity.get(input.objectId)
    if (capacity === undefined) return Promise.reject(new DOMException('Staged object has no retained capacity', 'InvalidStateError'))
    // Capacity binds its own parent operation; the child namespace owns only physical storage.
    return capacity.reserveGrowth({ ...input, operationId: this.#parentOperationId })
  }

  async reconcileObject(objectId: string, actualLength: bigint): Promise<void> {
    snapshotIdentity(objectId, 32, 'staged object ID')
    if (actualLength < 0n) throw new TypeError('Staged object length cannot be negative')
    await this.authorize()
  }
}

async function reserveStagingRoot(
  input: Readonly<{ binding: DurableCheckpointNamespaceIdentity; parent: FileSystemDirectoryHandle; handles: PersistentHandleRepository }>,
  id: string,
  current: FileSystemDirectoryHandle | undefined,
): Promise<FileSystemDirectoryHandle> {
  const receiptId = id + '/creation'
  const receipt = await input.handles.readHandle(receiptId)
  const { operationId, authorityRef } = input.binding
  const creationObjectId = deliveryDigest('windshare/browser-delivery/root-creation/v1', namespaceFields(input.binding))
  if (receipt === undefined) {
    if (current !== undefined) throw new TargetOwnershipUnknownError('namespace-create', operationId)
    // Persist creation authority before touching OPFS. No file allocation is possible
    // before the final root handle commits, so only an empty reserved root can recover.
    // The receipt owns a separate identity because both handles coexist during promotion.
    await input.handles.putHandle({ id: receiptId, operationId, authorityRef,
      kind: STAGING_ROOT_CREATION_KIND, ownedObjectId: creationObjectId, handle: input.parent })
  } else {
    const parent = receipt.handle as FileSystemDirectoryHandle
    if (receipt.operationId !== operationId || receipt.authorityRef !== authorityRef ||
        receipt.kind !== STAGING_ROOT_CREATION_KIND || receipt.ownedObjectId !== creationObjectId ||
        typeof parent?.isSameEntry !== 'function' || !await input.parent.isSameEntry(parent)) {
      throw new TargetOwnershipUnknownError('namespace-create', operationId)
    }
    if (current !== undefined && !(await current.entries().next()).done) {
      throw new TargetOwnershipUnknownError('namespace-create', operationId)
    }
  }
  const root = current ?? await input.parent.getDirectoryHandle(STAGING_ROOT_ENTRY_PREFIX + operationId, { create: true })
  await input.handles.putHandle({ id, operationId, authorityRef,
    kind: STAGING_ROOT_HANDLE_KIND, ownedObjectId: authorityRef, handle: root })
  return root
}

async function optionalDirectory(parent: FileSystemDirectoryHandle, name: string): Promise<FileSystemDirectoryHandle | undefined> {
  try { return await parent.getDirectoryHandle(name) }
  catch (error) {
    if (error instanceof DOMException && error.name === 'NotFoundError') return undefined
    throw error
  }
}

async function optionalFile(parent: FileSystemDirectoryHandle, name: string): Promise<FileSystemFileHandle | undefined> {
  try { return await parent.getFileHandle(name) }
  catch (error) {
    if (error instanceof DOMException && error.name === 'NotFoundError') return undefined
    throw error
  }
}
