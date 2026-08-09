import type { ReceiveOperationRepository } from '../workspace/repository'
import {
  assertWorkspaceContentGate,
  WORKSPACE_HANDLE_ROOT,
  type WorkspaceContentGate,
} from '../workspace/stages'
import { snapshotIdentity } from '../workspace/canonical'
import { TargetOwnershipUnknownError, type TargetOwnershipStage } from '../persistent-tree/errors'
import type { OriginPrivateWorkspaceBudgetClaim } from './admission'

export const ORIGIN_PRIVATE_RAW_FILE_CONTAINER = 'raw-files-v2' as const
export const ORIGIN_PRIVATE_DIRECTORY_OBJECT_CONTAINER = 'directory-objects-v2' as const
export const ORIGIN_PRIVATE_PACKAGE_CONTAINER = 'packages-v2' as const

export type OriginPrivateObjectContainer =
  | typeof ORIGIN_PRIVATE_RAW_FILE_CONTAINER
  | typeof ORIGIN_PRIVATE_DIRECTORY_OBJECT_CONTAINER
  | typeof ORIGIN_PRIVATE_PACKAGE_CONTAINER

const OWNED_OBJECT_SCAN_BOUND = 1_048_576
const OWNED_OBJECT_CONTAINERS: readonly OriginPrivateObjectContainer[] = Object.freeze([
  ORIGIN_PRIVATE_RAW_FILE_CONTAINER,
  ORIGIN_PRIVATE_DIRECTORY_OBJECT_CONTAINER,
  ORIGIN_PRIVATE_PACKAGE_CONTAINER,
])

export interface OriginPrivateWorkspaceRootOptions {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly workspaceBindingDigest: string
  readonly authorityRef: string
  readonly workspaceRootHandleId: string
  readonly workspaceRootHandle: FileSystemDirectoryHandle
  readonly repository: ReceiveOperationRepository
  readonly contentGate?: WorkspaceContentGate
  readonly budgetClaim?: OriginPrivateWorkspaceBudgetClaim
}

/**
 * This object is the single OPFS authority boundary. Allocation is allowed only after
 * the committed budget gate is proven and the persisted root still names the live root.
 */
export class OriginPrivateWorkspaceRoot {
  readonly operationId: string
  readonly authorityRef: string
  readonly #receiveIntentDigest: string
  readonly #workspaceRootHandleId: string
  readonly #workspaceRootHandle: FileSystemDirectoryHandle
  readonly #repository: ReceiveOperationRepository
  readonly #budgetClaim: OriginPrivateWorkspaceBudgetClaim | undefined
  #rootOwnedObjectId: string | undefined

  constructor(options: OriginPrivateWorkspaceRootOptions) {
    this.operationId = snapshotIdentity(options.operationId, 16, 'operation ID')
    this.#receiveIntentDigest = snapshotIdentity(
      options.receiveIntentDigest,
      32,
      'receive intent digest',
    )
    this.authorityRef = snapshotIdentity(options.authorityRef, 32, 'workspace authority reference')
    if (typeof options.workspaceRootHandleId !== 'string' || options.workspaceRootHandleId.length === 0) {
      throw new TypeError('workspace root handle ID is invalid')
    }
    this.#workspaceRootHandleId = options.workspaceRootHandleId
    this.#workspaceRootHandle = requireDirectoryHandle(
      options.workspaceRootHandle,
      this.operationId,
      'parent-authority',
    )
    this.#repository = options.repository
    if ((options.contentGate === undefined) !== (options.budgetClaim === undefined)) {
      throw new TypeError('workspace allocation authority is incomplete')
    }
    this.#budgetClaim = options.budgetClaim
    if (options.contentGate !== undefined && options.budgetClaim !== undefined) {
      if (options.budgetClaim.budgetDigest !== options.contentGate.workspaceBudgetDigest) {
        throw new TypeError('workspace budget claim disagrees with its content gate')
      }
      assertWorkspaceContentGate(options.contentGate, {
        operationId: this.operationId,
        receiveIntentDigest: this.#receiveIntentDigest,
        workspaceBudgetDigest: options.budgetClaim.budgetDigest,
      })
    }
    snapshotIdentity(options.workspaceBindingDigest, 32, 'workspace binding digest')
  }

  async authorize(): Promise<void> {
    await this.#verifyRoot('parent-authority')
  }

  async prepareContainers(): Promise<void> {
    const root = await this.#verifyRoot('parent-authority')
    await Promise.all([
      root.getDirectoryHandle(ORIGIN_PRIVATE_RAW_FILE_CONTAINER, { create: true }),
      root.getDirectoryHandle(ORIGIN_PRIVATE_DIRECTORY_OBJECT_CONTAINER, { create: true }),
    ])
    await this.#verifyRoot('namespace-create')
  }

  rootOwnedObjectId(): string {
    if (this.#rootOwnedObjectId === undefined) {
      throw new TargetOwnershipUnknownError('parent-authority', this.operationId)
    }
    return this.#rootOwnedObjectId
  }

  async readmit(): Promise<void> {
    const claim = this.#budgetClaim
    if (claim === undefined) {
      throw new DOMException('Workspace allocation requires an active budget claim', 'InvalidStateError')
    }
    const admission = await claim.readmit(await this.verifiedAlreadyOwnedBytes())
    if (admission.budgetDigest !== claim.budgetDigest) {
      throw new TargetOwnershipUnknownError('reservation', this.operationId)
    }
    if (admission.kind === 'rejected') {
      throw new DOMException(
        `Origin-private workspace readmission failed: ${admission.reason}`,
        'QuotaExceededError',
      )
    }
  }

  async verifiedAlreadyOwnedBytes(): Promise<bigint> {
    const root = await this.#verifyRoot('reservation')
    let count = 0
    let total = 0n
    for (const containerName of OWNED_OBJECT_CONTAINERS) {
      const container = await optionalDirectory(root, containerName)
      if (container === undefined) continue
      const entries = container.entries
      if (typeof entries !== 'function') {
        throw new TargetOwnershipUnknownError('reservation', this.operationId)
      }
      for await (const [name, handle] of entries.call(container)) {
        count += 1
        if (count > OWNED_OBJECT_SCAN_BOUND || handle.kind !== 'file') {
          throw new TargetOwnershipUnknownError('reservation', this.operationId)
        }
        snapshotIdentity(name, 32, 'owned object ID')
        const size = BigInt((await (handle as FileSystemFileHandle).getFile()).size)
        total += size
        if (total > 0xffff_ffff_ffff_ffffn) {
          throw new TargetOwnershipUnknownError('reservation', this.operationId)
        }
      }
    }
    return total
  }

  async readObject(
    containerName: OriginPrivateObjectContainer,
    ownedObjectId: string,
    stage: TargetOwnershipStage,
  ): Promise<FileSystemFileHandle | undefined> {
    const objectId = snapshotIdentity(ownedObjectId, 32, 'owned object ID')
    const root = await this.#verifyRoot(stage)
    const container = await optionalDirectory(root, containerName)
    if (container === undefined) return undefined
    return optionalFile(container, objectId)
  }

  async createObject(
    containerName: OriginPrivateObjectContainer,
    ownedObjectId: string,
  ): Promise<FileSystemFileHandle> {
    const objectId = snapshotIdentity(ownedObjectId, 32, 'owned object ID')
    await this.readmit()
    const root = await this.#verifyRoot('namespace-create')
    const container = await root.getDirectoryHandle(containerName, { create: true })
    const existing = await optionalFile(container, objectId)
    if (existing !== undefined) {
      throw new TargetOwnershipUnknownError('namespace-create', this.operationId)
    }
    const created = await container.getFileHandle(objectId, { create: true })
    const current = await optionalFile(container, objectId)
    if (current === undefined || !await sameEntry(
      current,
      created,
      this.operationId,
      'namespace-create',
    )) {
      throw new TargetOwnershipUnknownError('namespace-create', this.operationId)
    }
    return created
  }

  async removeObject(
    containerName: OriginPrivateObjectContainer,
    ownedObjectId: string,
    expected: FileSystemFileHandle,
  ): Promise<'removed' | 'already-absent'> {
    const objectId = snapshotIdentity(ownedObjectId, 32, 'owned object ID')
    const root = await this.#verifyRoot('cleanup')
    const container = await optionalDirectory(root, containerName)
    if (container === undefined) return 'already-absent'
    const current = await optionalFile(container, objectId)
    if (current === undefined) return 'already-absent'
    if (!await sameEntry(current, expected, this.operationId, 'cleanup')) {
      throw new TargetOwnershipUnknownError('cleanup', this.operationId)
    }
    await container.removeEntry(objectId)
    return 'removed'
  }

  sameObject(
    left: FileSystemHandle,
    right: FileSystemHandle,
    stage: TargetOwnershipStage,
  ): Promise<boolean> {
    return sameEntry(left, right, this.operationId, stage)
  }

  async #verifyRoot(stage: TargetOwnershipStage): Promise<FileSystemDirectoryHandle> {
    let persisted
    try {
      persisted = await this.#repository.readHandle<FileSystemDirectoryHandle>(
        this.#workspaceRootHandleId,
      )
    } catch (cause) {
      throw new TargetOwnershipUnknownError(stage, this.operationId, { cause })
    }
    const persistedHandle = persisted === undefined
      ? undefined
      : requireDirectoryHandle(persisted.handle, this.operationId, stage)
    if (persisted === undefined || persisted.kind !== WORKSPACE_HANDLE_ROOT ||
        persisted.operationId !== this.operationId || persisted.authorityRef !== this.authorityRef ||
        persisted.ownedObjectId === undefined || persistedHandle === undefined ||
        !await sameEntry(
          persistedHandle,
          this.#workspaceRootHandle,
          this.operationId,
          stage,
        )) {
      throw new TargetOwnershipUnknownError(stage, this.operationId)
    }
    this.#rootOwnedObjectId = snapshotIdentity(
      persisted.ownedObjectId,
      32,
      'workspace root owned object ID',
    )
    return this.#workspaceRootHandle
  }
}

function requireDirectoryHandle(
  value: unknown,
  operationId: string,
  stage: TargetOwnershipStage,
): FileSystemDirectoryHandle {
  if (typeof value !== 'object' || value === null ||
      !('kind' in value) || value.kind !== 'directory' ||
      !('isSameEntry' in value) || typeof value.isSameEntry !== 'function') {
    throw new TargetOwnershipUnknownError(stage, operationId)
  }
  return value as FileSystemDirectoryHandle
}

async function optionalDirectory(
  parent: FileSystemDirectoryHandle,
  name: string,
): Promise<FileSystemDirectoryHandle | undefined> {
  try {
    return await parent.getDirectoryHandle(name)
  } catch (error) {
    if (errorNamed(error, 'NotFoundError')) return undefined
    throw error
  }
}

async function optionalFile(
  parent: FileSystemDirectoryHandle,
  name: string,
): Promise<FileSystemFileHandle | undefined> {
  try {
    return await parent.getFileHandle(name)
  } catch (error) {
    if (errorNamed(error, 'NotFoundError')) return undefined
    throw error
  }
}

async function sameEntry(
  left: FileSystemHandle,
  right: FileSystemHandle,
  operationId: string,
  stage: TargetOwnershipStage,
): Promise<boolean> {
  try {
    return await left.isSameEntry(right)
  } catch (cause) {
    throw new TargetOwnershipUnknownError(stage, operationId, { cause })
  }
}

function errorNamed(error: unknown, name: string): boolean {
  return typeof error === 'object' && error !== null &&
    'name' in error && error.name === name
}
