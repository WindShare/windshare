import type {
  FileCheckpointJournal,
  PersistentHandleRecord,
  PersistentHandleInventoryRepository,
  PersistentHandleRepository,
} from '../persistence/journal'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type {
  WorkspaceCheckpointCleanupObservation,
  WorkspaceOwnedCleanupPort,
  WorkspaceOwnedObjectCleanupObservation,
  WorkspaceOwnedObjectCleanupTarget,
} from '../workspace/cleanup'
import { snapshotIdentity } from '../workspace/canonical'
import type {
  ReceiveOperationHandleInventoryRepository,
  ReceiveOperationRepository,
} from '../workspace/repository'
import type { ReceiveOperationHandleRecord } from '../workspace/records'
import {
  WORKSPACE_HANDLE_PACKAGE_OBJECT,
  WORKSPACE_HANDLE_PUBLICATION_TARGET,
  WORKSPACE_HANDLE_ROOT,
  WORKSPACE_HANDLE_ZIP_LAYOUT,
  type WorkspaceCleanupRequest,
} from '../workspace/stages'
import {
  removeOriginPrivateWorkspaceNamespace,
  type OriginPrivateWorkspaceNamespace,
} from './namespace'
import { OriginPrivatePackageStore } from './package-store'
import {
  ORIGIN_PRIVATE_DIRECTORY_OBJECT_CONTAINER,
  ORIGIN_PRIVATE_PACKAGE_CONTAINER,
  ORIGIN_PRIVATE_RAW_FILE_CONTAINER,
  type OriginPrivateObjectContainer,
  type OriginPrivateWorkspaceRoot,
} from './workspace-root'
import {
  ORIGIN_PRIVATE_DIRECTORY_HANDLE_KIND,
  ORIGIN_PRIVATE_FILE_HANDLE_KIND,
} from './workspace-tree'

interface RetirableCheckpointStore
extends FileCheckpointJournal, PersistentHandleRepository {
  retireOperation(): Promise<void>
}

export interface OriginPrivateWorkspaceCleanupAuthority extends WorkspaceOwnedCleanupPort {
  cleanupRequest(): Promise<WorkspaceCleanupRequest>
}

/** External effects are replayable; missing authority is never treated as successful cleanup. */
export class OriginPrivateWorkspaceCleanupPort implements OriginPrivateWorkspaceCleanupAuthority {
  readonly #root: OriginPrivateWorkspaceRoot
  readonly #namespace: OriginPrivateWorkspaceNamespace
  readonly #operationRepository: ReceiveOperationRepository
  readonly #checkpoints: RetirableCheckpointStore
  readonly #packages: OriginPrivatePackageStore
  #inventoryOwnershipUnknown = false

  constructor(input: {
    readonly root: OriginPrivateWorkspaceRoot
    readonly namespace: OriginPrivateWorkspaceNamespace
    readonly operationRepository: ReceiveOperationRepository
    readonly checkpoints: RetirableCheckpointStore
    readonly packages: OriginPrivatePackageStore
  }) {
    this.#root = input.root
    this.#namespace = input.namespace
    this.#operationRepository = input.operationRepository
    this.#checkpoints = input.checkpoints
    this.#packages = input.packages
  }

  async removeOwnedObject(
    target: WorkspaceOwnedObjectCleanupTarget,
  ): Promise<WorkspaceOwnedObjectCleanupObservation> {
    const objectId = snapshotIdentity(target.ownedObjectId, 32, 'cleanup owned object ID')
    try {
      return await this.#removeOwnedObject(target.handleId, objectId)
    } catch (error) {
      return Object.freeze({
        kind: error instanceof TargetOwnershipUnknownError
          ? 'ownership-unknown'
          : 'retryable-failure',
      })
    }
  }

  async #removeOwnedObject(
    handleId: string,
    objectId: string,
  ): Promise<WorkspaceOwnedObjectCleanupObservation> {
    const checkpointHandle = await this.#checkpoints.readHandle(handleId)
    if (checkpointHandle !== undefined) {
      return this.#removeCheckpointObject(checkpointHandle, objectId)
    }
    const operationHandle = await this.#operationRepository.readHandle(handleId)
    if (operationHandle !== undefined) {
      return this.#removePackageObject(operationHandle, objectId)
    }
    const observations = await Promise.all([
      this.#root.readObject(ORIGIN_PRIVATE_RAW_FILE_CONTAINER, objectId, 'cleanup'),
      this.#root.readObject(ORIGIN_PRIVATE_DIRECTORY_OBJECT_CONTAINER, objectId, 'cleanup'),
      this.#root.readObject(ORIGIN_PRIVATE_PACKAGE_CONTAINER, objectId, 'cleanup'),
    ])
    const kind = observations.every((handle) => handle === undefined)
      ? 'already-absent'
      : 'ownership-unknown'
    return Object.freeze({ kind })
  }

  async #removeCheckpointObject(
    checkpointHandle: PersistentHandleRecord,
    objectId: string,
  ): Promise<WorkspaceOwnedObjectCleanupObservation> {
    if (!this.#ownedHandle(checkpointHandle, objectId)) {
      return Object.freeze({ kind: 'ownership-unknown' })
    }
    const container = checkpointContainer(checkpointHandle.kind)
    if (container === undefined) return Object.freeze({ kind: 'ownership-unknown' })
    const persisted = requireFileHandle(checkpointHandle.handle)
    const current = await this.#root.readObject(container, objectId, 'cleanup')
    if (current === undefined) return Object.freeze({ kind: 'already-absent' })
    if (!await this.#root.sameObject(current, persisted, 'cleanup')) {
      return Object.freeze({ kind: 'ownership-unknown' })
    }
    const result = await this.#root.removeObject(container, objectId, persisted)
    return Object.freeze({ kind: result })
  }

  async #removePackageObject(
    operationHandle: ReceiveOperationHandleRecord,
    objectId: string,
  ): Promise<WorkspaceOwnedObjectCleanupObservation> {
    if (operationHandle.kind !== WORKSPACE_HANDLE_PACKAGE_OBJECT ||
        !this.#ownedHandle(operationHandle, objectId)) {
      return Object.freeze({ kind: 'ownership-unknown' })
    }
    const proof = await this.#packages.cleanupPackage(objectId)
    return Object.freeze({ kind: proof.result })
  }

  #ownedHandle(
    handle: PersistentHandleRecord | ReceiveOperationHandleRecord,
    objectId: string,
  ): boolean {
    return handle.operationId === this.#root.operationId &&
      handle.authorityRef === this.#root.authorityRef &&
      handle.ownedObjectId === objectId
  }

  async removeFileCheckpoints(input: {
    readonly operationId: string
    readonly receiveIntentDigest: string
  }): Promise<WorkspaceCheckpointCleanupObservation> {
    if (input.operationId !== this.#checkpoints.binding.operationId ||
        input.receiveIntentDigest !== this.#checkpoints.binding.receiveIntentDigest ||
        input.operationId !== this.#namespace.operationId || this.#inventoryOwnershipUnknown) {
      return Object.freeze({ kind: 'ownership-unknown' })
    }
    try {
      const removedRecordDigests = await this.#checkpointDigests()
      await removeOriginPrivateWorkspaceNamespace(this.#namespace, this.#operationRepository)
      await this.#checkpoints.retireOperation()
      return Object.freeze({
        kind: 'clean',
        removedRecordDigests: Object.freeze([...removedRecordDigests]),
      })
    } catch (error) {
      return Object.freeze({
        kind: error instanceof TargetOwnershipUnknownError
          ? 'ownership-unknown'
          : 'retryable-failure',
      })
    }
  }

  /** Derives cleanup authority from persisted handle inventories, never UI-provided IDs. */
  async cleanupRequest(): Promise<WorkspaceCleanupRequest> {
    const inventory = new CleanupInventoryBuilder(this.#namespace)
    try {
      const checkpointInventory = requireCheckpointHandleInventory(this.#checkpoints)
      const operationInventory = requireOperationHandleInventory(this.#operationRepository)
      const [checkpointHandles, operationHandles] = await Promise.all([
        checkpointInventory.listHandles(),
        operationInventory.listHandles(this.#namespace.operationId),
      ])
      checkpointHandles.forEach((handle) => inventory.addCheckpointHandle(handle))
      operationHandles.forEach((handle) => inventory.addOperationHandle(handle))
    } catch (error) {
      if (!isInventoryAmbiguity(error)) throw error
      // Invalid durable inventory is evidence of ambiguity, not permission to skip it.
      inventory.markUnknown()
    }
    const request = inventory.request(this)
    this.#inventoryOwnershipUnknown ||= inventory.ownershipUnknown
    return request
  }

  async #checkpointDigests(): Promise<ReadonlySet<string>> {
    const digests = new Set<string>()
    for (const scan of [
      this.#checkpoints.scanCommitted.bind(this.#checkpoints),
      this.#checkpoints.scanCandidates.bind(this.#checkpoints),
    ]) {
      let cursor: string | undefined
      do {
        const page = await scan({
          direction: 'ascending',
          ...(cursor === undefined ? {} : { cursor }),
        })
        for (const record of page.records) digests.add(record.checksum)
        cursor = page.nextCursor
      } while (cursor !== undefined)
    }
    return digests
  }
}

function requireCheckpointHandleInventory(
  value: RetirableCheckpointStore,
): PersistentHandleInventoryRepository {
  if (!('listHandles' in value) || typeof value.listHandles !== 'function') {
    throw new TypeError('checkpoint cleanup lacks its handle inventory authority')
  }
  return value as RetirableCheckpointStore & PersistentHandleInventoryRepository
}

function requireOperationHandleInventory(
  value: ReceiveOperationRepository,
): ReceiveOperationHandleInventoryRepository {
  if (!('listHandles' in value) || typeof value.listHandles !== 'function') {
    throw new TypeError('operation cleanup lacks its handle inventory authority')
  }
  return value as ReceiveOperationHandleInventoryRepository
}

class CleanupInventoryBuilder {
  readonly #namespace: OriginPrivateWorkspaceNamespace
  readonly #targets = new Map<string, WorkspaceOwnedObjectCleanupTarget>()
  readonly #objectIds = new Set<string>()
  readonly #metadataHandleIds = new Set<string>()
  ownershipUnknown = false

  constructor(namespace: OriginPrivateWorkspaceNamespace) {
    this.#namespace = namespace
  }

  addCheckpointHandle(handle: PersistentHandleRecord): void {
    if (!this.#owned(handle) || checkpointContainer(handle.kind) === undefined) {
      this.markUnknown()
      return
    }
    this.#addTarget(handle.id, handle.ownedObjectId)
  }

  addOperationHandle(handle: ReceiveOperationHandleRecord): void {
    if (!this.#owned(handle)) {
      this.markUnknown()
      return
    }
    if (handle.kind === WORKSPACE_HANDLE_PACKAGE_OBJECT) {
      if (handle.ownedObjectId === undefined) this.markUnknown()
      else this.#addTarget(handle.id, handle.ownedObjectId)
      return
    }
    if (!isWorkspaceMetadataHandle(handle.kind)) {
      this.markUnknown()
      return
    }
    this.#metadataHandleIds.add(handle.id)
    if (handle.kind === WORKSPACE_HANDLE_ROOT && handle.id !== this.#namespace.rootHandleId) {
      this.markUnknown()
    }
  }

  markUnknown(): void {
    this.ownershipUnknown = true
  }

  request(port: WorkspaceOwnedCleanupPort): WorkspaceCleanupRequest {
    if (!this.#metadataHandleIds.has(this.#namespace.rootHandleId)) this.markUnknown()
    return Object.freeze({
      targets: Object.freeze([...this.#targets.values()].sort((left, right) =>
        left.handleId.localeCompare(right.handleId))),
      metadataHandleIds: Object.freeze([...this.#metadataHandleIds].sort()),
      port,
    })
  }

  #owned(handle: PersistentHandleRecord | ReceiveOperationHandleRecord): boolean {
    return handle.operationId === this.#namespace.operationId &&
      handle.authorityRef === this.#namespace.authorityRef
  }

  #addTarget(handleId: string, ownedObjectId: string): void {
    if (this.#targets.has(handleId) || this.#objectIds.has(ownedObjectId)) {
      this.markUnknown()
      return
    }
    this.#objectIds.add(ownedObjectId)
    this.#targets.set(handleId, Object.freeze({ handleId, ownedObjectId }))
  }
}

function isWorkspaceMetadataHandle(kind: number): boolean {
  return kind === WORKSPACE_HANDLE_ROOT || kind === WORKSPACE_HANDLE_ZIP_LAYOUT ||
    kind === WORKSPACE_HANDLE_PUBLICATION_TARGET
}

function isInventoryAmbiguity(error: unknown): boolean {
  return error instanceof TypeError || error instanceof TargetOwnershipUnknownError ||
    (error instanceof DOMException && error.name === 'QuotaExceededError')
}

function requireFileHandle(value: unknown): FileSystemFileHandle {
  if (typeof value !== 'object' || value === null ||
      !('kind' in value) || value.kind !== 'file' ||
      !('isSameEntry' in value) || typeof value.isSameEntry !== 'function') {
    throw new TypeError('origin-private cleanup handle is invalid')
  }
  return value as FileSystemFileHandle
}

function checkpointContainer(kind: number): OriginPrivateObjectContainer | undefined {
  if (kind === ORIGIN_PRIVATE_FILE_HANDLE_KIND) return ORIGIN_PRIVATE_RAW_FILE_CONTAINER
  if (kind === ORIGIN_PRIVATE_DIRECTORY_HANDLE_KIND) {
    return ORIGIN_PRIVATE_DIRECTORY_OBJECT_CONTAINER
  }
  return undefined
}
