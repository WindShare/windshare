import { encodeBase64Url } from '../../crypto/bytes'
import { validateReceiveIntent, type ReceiveIntent } from '../../transfer/intent'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type { ReceiveOperationHandleRecord } from '../workspace/records'
import type { ReceiveOperationRepository } from '../workspace/repository'
import {
  persistWorkspaceOperation,
  WORKSPACE_HANDLE_ROOT,
} from '../workspace/stages'

const WORKSPACE_ROOT_HANDLE_DOMAIN = 'windshare/origin-private/workspace-root/v1'
const WORKSPACE_ENTRY_PREFIX = 'operation-'

interface OriginPrivateStorageManager extends StorageManager {
  getDirectory(): Promise<FileSystemDirectoryHandle>
}

export interface OriginPrivateWorkspaceNamespace {
  readonly operationId: string
  readonly authorityRef: string
  readonly parent: FileSystemDirectoryHandle
  readonly entryName: string
  readonly root: FileSystemDirectoryHandle
  readonly rootHandleId: string
  readonly rootOwnedObjectId: string
}

/** Creates one operation-confined directory and binds it before any content allocation. */
export async function openOriginPrivateWorkspaceNamespace(input: {
  readonly receiveIntent: ReceiveIntent
  readonly repository: ReceiveOperationRepository
  readonly storage?: OriginPrivateStorageManager
  readonly randomOwnedObjectId?: () => string
}): Promise<OriginPrivateWorkspaceNamespace> {
  const intent = await validateReceiveIntent(input.receiveIntent)
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind === 'directory-tree') {
    throw new TypeError('origin-private namespace requires a workspace receive intent')
  }
  const storage = input.storage ?? requireOriginPrivateStorage()
  const parent = await storage.getDirectory()
  const entryName = `${WORKSPACE_ENTRY_PREFIX}${intent.operationId}`
  const rootHandleId = originPrivateWorkspaceRootHandleId(intent.operationId)
  const persisted = await input.repository.readHandle<FileSystemDirectoryHandle>(rootHandleId)
  if (persisted !== undefined) {
    const root = await requireDirectoryEntry(parent, entryName, intent.operationId)
    const persistedRoot = requireRootRecord(
      persisted,
      intent.operationId,
      intent.plan.workspace.repositoryRef,
    )
    if (!await sameEntry(root, persistedRoot.handle, intent.operationId, 'parent-authority')) {
      throw new TargetOwnershipUnknownError('parent-authority', intent.operationId)
    }
    return Object.freeze({
      operationId: intent.operationId,
      authorityRef: intent.plan.workspace.repositoryRef,
      parent,
      entryName,
      root,
      rootHandleId,
      rootOwnedObjectId: persistedRoot.ownedObjectId,
    })
  }
  if (await optionalDirectory(parent, entryName) !== undefined) {
    throw new TargetOwnershipUnknownError('namespace-create', intent.operationId)
  }
  const root = await parent.getDirectoryHandle(entryName, { create: true })
  const rootOwnedObjectId = (input.randomOwnedObjectId ?? randomOwnedObjectId)()
  try {
    await persistWorkspaceOperation({
      repository: input.repository,
      receiveIntent: intent,
      workspaceRootHandleId: rootHandleId,
      workspaceOwnedObjectId: rootOwnedObjectId,
      workspaceRootHandle: root,
    })
    const reread = await input.repository.readHandle<FileSystemDirectoryHandle>(rootHandleId)
    const authority = requireRootRecord(
      reread,
      intent.operationId,
      intent.plan.workspace.repositoryRef,
    )
    const current = await requireDirectoryEntry(parent, entryName, intent.operationId)
    if (!await sameEntry(current, authority.handle, intent.operationId, 'namespace-create') ||
        !await sameEntry(current, root, intent.operationId, 'namespace-create')) {
      throw new TargetOwnershipUnknownError('namespace-create', intent.operationId)
    }
    return Object.freeze({
      operationId: intent.operationId,
      authorityRef: intent.plan.workspace.repositoryRef,
      parent,
      entryName,
      root: current,
      rootHandleId,
      rootOwnedObjectId: authority.ownedObjectId,
    })
  } catch (error) {
    const current = await optionalDirectory(parent, entryName).catch(() => undefined)
    if (current !== undefined && await sameEntry(
      current,
      root,
      intent.operationId,
      'cleanup',
    ).catch(() => false)) {
      await parent.removeEntry(entryName, { recursive: true }).catch(() => undefined)
    }
    throw error
  }
}

/**
 * Crash recovery must prove the pre-existing namespace; creating a replacement at
 * the deterministic name would silently transfer ownership to a different object.
 */
export async function reopenOriginPrivateWorkspaceNamespace(input: {
  readonly receiveIntent: ReceiveIntent
  readonly repository: ReceiveOperationRepository
  readonly storage?: OriginPrivateStorageManager
}): Promise<OriginPrivateWorkspaceNamespace> {
  const intent = await validateReceiveIntent(input.receiveIntent)
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind === 'directory-tree') {
    throw new TypeError('origin-private namespace reopen requires a workspace receive intent')
  }
  const parent = await (input.storage ?? requireOriginPrivateStorage()).getDirectory()
  const entryName = `${WORKSPACE_ENTRY_PREFIX}${intent.operationId}`
  const rootHandleId = originPrivateWorkspaceRootHandleId(intent.operationId)
  const persistedRoot = requireRootRecord(
    await input.repository.readHandle<FileSystemDirectoryHandle>(rootHandleId),
    intent.operationId,
    intent.plan.workspace.repositoryRef,
  )
  const root = await requireDirectoryEntry(parent, entryName, intent.operationId)
  if (!await sameEntry(root, persistedRoot.handle, intent.operationId, 'parent-authority')) {
    throw new TargetOwnershipUnknownError('parent-authority', intent.operationId)
  }
  return Object.freeze({
    operationId: intent.operationId,
    authorityRef: intent.plan.workspace.repositoryRef,
    parent,
    entryName,
    root,
    rootHandleId,
    rootOwnedObjectId: persistedRoot.ownedObjectId,
  })
}

export async function removeOriginPrivateWorkspaceNamespace(
  namespace: OriginPrivateWorkspaceNamespace,
  repository: ReceiveOperationRepository,
): Promise<'removed' | 'already-absent'> {
  const persisted = requireRootRecord(
    await repository.readHandle<FileSystemDirectoryHandle>(namespace.rootHandleId),
    namespace.operationId,
    namespace.authorityRef,
  )
  const current = await optionalDirectory(namespace.parent, namespace.entryName)
  if (current === undefined) return 'already-absent'
  if (!await sameEntry(current, persisted.handle, namespace.operationId, 'cleanup') ||
      !await sameEntry(current, namespace.root, namespace.operationId, 'cleanup')) {
    throw new TargetOwnershipUnknownError('cleanup', namespace.operationId)
  }
  await namespace.parent.removeEntry(namespace.entryName, { recursive: true })
  return 'removed'
}

export function originPrivateWorkspaceRootHandleId(operationId: string): string {
  return `${WORKSPACE_ROOT_HANDLE_DOMAIN}/${operationId}`
}

function requireRootRecord(
  record: ReceiveOperationHandleRecord<FileSystemDirectoryHandle> | undefined,
  operationId: string,
  authorityRef: string,
): ReceiveOperationHandleRecord<FileSystemDirectoryHandle> & { readonly ownedObjectId: string } {
  if (record === undefined || record.kind !== WORKSPACE_HANDLE_ROOT ||
      record.operationId !== operationId || record.authorityRef !== authorityRef ||
      record.ownedObjectId === undefined || record.handle.kind !== 'directory') {
    throw new TargetOwnershipUnknownError('parent-authority', operationId)
  }
  return record as ReceiveOperationHandleRecord<FileSystemDirectoryHandle> & {
    readonly ownedObjectId: string
  }
}

function requireOriginPrivateStorage(): OriginPrivateStorageManager {
  const storage = navigator.storage as Partial<OriginPrivateStorageManager>
  if (typeof storage.getDirectory !== 'function') {
    throw new DOMException('Origin-private file system is unavailable', 'NotSupportedError')
  }
  return storage as OriginPrivateStorageManager
}

async function requireDirectoryEntry(
  parent: FileSystemDirectoryHandle,
  name: string,
  operationId: string,
): Promise<FileSystemDirectoryHandle> {
  try {
    return await parent.getDirectoryHandle(name)
  } catch (cause) {
    throw new TargetOwnershipUnknownError('parent-authority', operationId, { cause })
  }
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

async function sameEntry(
  left: FileSystemHandle,
  right: FileSystemHandle,
  operationId: string,
  stage: 'parent-authority' | 'namespace-create' | 'cleanup',
): Promise<boolean> {
  try {
    return await left.isSameEntry(right)
  } catch (cause) {
    throw new TargetOwnershipUnknownError(stage, operationId, { cause })
  }
}

function randomOwnedObjectId(): string {
  const bytes = new Uint8Array(32)
  crypto.getRandomValues(bytes)
  return encodeBase64Url(bytes)
}

function errorNamed(error: unknown, name: string): boolean {
  return typeof error === 'object' && error !== null &&
    'name' in error && error.name === name
}
