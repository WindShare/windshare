import { encodeBase64Url } from '../../crypto/bytes'
import { validateReceiveIntent, type ReceiveIntent } from '../../transfer/intent'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type {
  ReceiveOperationHandleRecord,
  WorkspaceActivationCandidateV1,
} from '../workspace/records'
import type { ReceiveOperationRepository } from '../workspace/repository'
import {
  journalWorkspaceActivation,
  promoteWorkspaceActivation,
  WORKSPACE_HANDLE_ROOT,
} from '../workspace/stages'

const WORKSPACE_ROOT_HANDLE_DOMAIN = 'windshare/origin-private/workspace-root/v1'
const WORKSPACE_ENTRY_PREFIX = 'activation-'
const WORKSPACE_OWNERSHIP_MARKER = '.windshare-workspace-owner-v1'
const WORKSPACE_OWNERSHIP_MARKER_DOMAIN = 'windshare/workspace-activation-owner/v1'
const RANDOM_IDENTITY_BYTES = 32

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

export type OriginPrivateWorkspaceActivationObservation =
  | Readonly<{ readonly kind: 'absent' }>
  | Readonly<{ readonly kind: 'verified'; readonly namespace: OriginPrivateWorkspaceNamespace }>
  | Readonly<{ readonly kind: 'ownership-unknown' }>

export class OriginPrivateWorkspaceNamespaceOpenError extends Error {
  readonly candidate: WorkspaceActivationCandidateV1
  readonly namespace: OriginPrivateWorkspaceNamespace | undefined

  constructor(input: {
    readonly cause: unknown
    readonly candidate: WorkspaceActivationCandidateV1
    readonly namespace?: OriginPrivateWorkspaceNamespace
  }) {
    super('Origin-private workspace activation has a durable journal owner', { cause: input.cause })
    this.name = 'OriginPrivateWorkspaceNamespaceOpenError'
    this.candidate = input.candidate
    this.namespace = input.namespace
  }
}

/** The journal commit precedes every OPFS mutation, including creation of the namespace entry. */
export async function openOriginPrivateWorkspaceNamespace(input: {
  readonly receiveIntent: ReceiveIntent
  readonly repository: ReceiveOperationRepository
  readonly storage?: OriginPrivateStorageManager
  readonly signal?: AbortSignal
  readonly randomEntryIdentity?: () => string
  readonly randomOwnedObjectId?: () => string
  readonly onActivationCandidateCommitted?: (candidate: WorkspaceActivationCandidateV1) => void
  readonly onPersistenceCommitted?: (namespace: OriginPrivateWorkspaceNamespace) => void
  readonly onActivationTransition?: (
    transition: 'journaled' | 'namespace-created' | 'marker-written' | 'promoted',
  ) => void
}): Promise<OriginPrivateWorkspaceNamespace> {
  const intent = await requireWorkspaceIntent(input.receiveIntent)
  const rootHandleId = originPrivateWorkspaceRootHandleId(intent.operationId)
  if (await input.repository.readHandle(rootHandleId) !== undefined) {
    const namespace = await reopenOriginPrivateWorkspaceNamespace({
      receiveIntent: intent,
      repository: input.repository,
      ...(input.storage === undefined ? {} : { storage: input.storage }),
    })
    input.onPersistenceCommitted?.(namespace)
    input.onActivationTransition?.('promoted')
    return namespace
  }
  input.signal?.throwIfAborted()
  const candidate = await journalWorkspaceActivation({
    repository: input.repository,
    receiveIntent: intent,
    entryIdentity: (input.randomEntryIdentity ?? randomIdentity)(),
    workspaceRootHandleId: rootHandleId,
    workspaceOwnedObjectId: (input.randomOwnedObjectId ?? randomIdentity)(),
  })
  input.onActivationCandidateCommitted?.(candidate)
  input.onActivationTransition?.('journaled')

  let namespace: OriginPrivateWorkspaceNamespace | undefined
  try {
    input.signal?.throwIfAborted()
    const parent = await (input.storage ?? requireOriginPrivateStorage()).getDirectory()
    const entryName = originPrivateWorkspaceActivationEntryName(candidate)
    if (await optionalDirectory(parent, entryName) !== undefined) {
      throw new TargetOwnershipUnknownError('namespace-create', intent.operationId)
    }
    const root = await parent.getDirectoryHandle(entryName, { create: true })
    namespace = workspaceNamespace({
      intent,
      parent,
      entryName,
      root,
      rootHandleId: candidate.rootHandleId,
      rootOwnedObjectId: candidate.rootOwnedObjectId,
    })
    input.onActivationTransition?.('namespace-created')
    input.signal?.throwIfAborted()
    await writeWorkspaceActivationMarker(root, candidate)
    input.onActivationTransition?.('marker-written')
    input.signal?.throwIfAborted()
    const current = await requireDirectoryEntry(parent, entryName, intent.operationId)
    if (!await sameEntry(current, root, intent.operationId, 'namespace-create') ||
        !await hasWorkspaceActivationMarker(current, candidate)) {
      throw new TargetOwnershipUnknownError('namespace-create', intent.operationId)
    }
    await promoteWorkspaceActivation({
      repository: input.repository,
      candidate,
      workspaceRootHandle: current,
    })
    namespace = workspaceNamespace({
      intent,
      parent,
      entryName,
      root: current,
      rootHandleId: candidate.rootHandleId,
      rootOwnedObjectId: candidate.rootOwnedObjectId,
    })
    input.onPersistenceCommitted?.(namespace)
    input.onActivationTransition?.('promoted')

    const persisted = requireRootRecord(
      await input.repository.readHandle<FileSystemDirectoryHandle>(candidate.rootHandleId),
      intent.operationId,
      candidate.repositoryAuthority,
    )
    if (!await sameEntry(current, persisted.handle, intent.operationId, 'namespace-create')) {
      throw new TargetOwnershipUnknownError('namespace-create', intent.operationId)
    }
    return namespace
  } catch (cause) {
    throw new OriginPrivateWorkspaceNamespaceOpenError({
      cause,
      candidate,
      ...(namespace === undefined ? {} : { namespace }),
    })
  }
}

export async function inspectOriginPrivateWorkspaceActivationCandidate(input: {
  readonly candidate: WorkspaceActivationCandidateV1
  readonly storage?: OriginPrivateStorageManager
}): Promise<OriginPrivateWorkspaceActivationObservation> {
  const parent = await (input.storage ?? requireOriginPrivateStorage()).getDirectory()
  const entryName = originPrivateWorkspaceActivationEntryName(input.candidate)
  const root = await optionalDirectory(parent, entryName)
  if (root === undefined) return Object.freeze({ kind: 'absent' })
  if (!await hasWorkspaceActivationMarker(root, input.candidate)) {
    return Object.freeze({ kind: 'ownership-unknown' })
  }
  return Object.freeze({
    kind: 'verified',
    namespace: Object.freeze({
      operationId: input.candidate.operationId,
      authorityRef: input.candidate.repositoryAuthority,
      parent,
      entryName,
      root,
      rootHandleId: input.candidate.rootHandleId,
      rootOwnedObjectId: input.candidate.rootOwnedObjectId,
    }),
  })
}

/** Reopen follows the persisted handle; the operation ID or entry name alone is never authority. */
export async function reopenOriginPrivateWorkspaceNamespace(input: {
  readonly receiveIntent: ReceiveIntent
  readonly repository: ReceiveOperationRepository
  readonly storage?: OriginPrivateStorageManager
}): Promise<OriginPrivateWorkspaceNamespace> {
  const intent = await requireWorkspaceIntent(input.receiveIntent)
  const parent = await (input.storage ?? requireOriginPrivateStorage()).getDirectory()
  const rootHandleId = originPrivateWorkspaceRootHandleId(intent.operationId)
  const persistedRoot = requireRootRecord(
    await input.repository.readHandle<FileSystemDirectoryHandle>(rootHandleId),
    intent.operationId,
    intent.plan.workspace.repositoryRef,
  )
  const entryName = persistedRoot.handle.name
  const root = await requireDirectoryEntry(parent, entryName, intent.operationId)
  if (!await sameEntry(root, persistedRoot.handle, intent.operationId, 'parent-authority') ||
      !await hasWorkspaceOwnershipMarker(root, {
        operationId: intent.operationId,
        rootOwnedObjectId: persistedRoot.ownedObjectId,
        repositoryAuthority: intent.plan.workspace.repositoryRef,
      })) {
    throw new TargetOwnershipUnknownError('parent-authority', intent.operationId)
  }
  return workspaceNamespace({
    intent,
    parent,
    entryName,
    root,
    rootHandleId,
    rootOwnedObjectId: persistedRoot.ownedObjectId,
  })
}

export async function inspectOriginPrivateWorkspaceNamespace(input: {
  readonly receiveIntent: ReceiveIntent
  readonly repository: ReceiveOperationRepository
  readonly storage?: OriginPrivateStorageManager
}): Promise<OriginPrivateWorkspaceNamespace | undefined> {
  const intent = await requireWorkspaceIntent(input.receiveIntent)
  const rootHandleId = originPrivateWorkspaceRootHandleId(intent.operationId)
  const persisted = await input.repository.readHandle<FileSystemDirectoryHandle>(rootHandleId)
  if (persisted === undefined) return undefined
  const persistedRoot = requireRootRecord(
    persisted,
    intent.operationId,
    intent.plan.workspace.repositoryRef,
  )
  const parent = await (input.storage ?? requireOriginPrivateStorage()).getDirectory()
  return workspaceNamespace({
    intent,
    parent,
    entryName: persistedRoot.handle.name,
    root: persistedRoot.handle,
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
      !await sameEntry(current, namespace.root, namespace.operationId, 'cleanup') ||
      !await hasWorkspaceOwnershipMarker(current, {
        operationId: namespace.operationId,
        rootOwnedObjectId: namespace.rootOwnedObjectId,
        repositoryAuthority: namespace.authorityRef,
      })) {
    throw new TargetOwnershipUnknownError('cleanup', namespace.operationId)
  }
  await namespace.parent.removeEntry(namespace.entryName, { recursive: true })
  if (await optionalDirectory(namespace.parent, namespace.entryName) !== undefined) {
    throw new TargetOwnershipUnknownError('cleanup', namespace.operationId)
  }
  return 'removed'
}

export async function removeUncommittedOriginPrivateWorkspaceNamespace(
  namespace: OriginPrivateWorkspaceNamespace,
): Promise<boolean> {
  try {
    const current = await optionalDirectory(namespace.parent, namespace.entryName)
    if (current === undefined) return true
    if (!await sameEntry(current, namespace.root, namespace.operationId, 'cleanup')) return false
    await namespace.parent.removeEntry(namespace.entryName, { recursive: true })
    return await optionalDirectory(namespace.parent, namespace.entryName) === undefined
  } catch {
    return false
  }
}

export function originPrivateWorkspaceRootHandleId(operationId: string): string {
  return `${WORKSPACE_ROOT_HANDLE_DOMAIN}/${operationId}`
}

export function originPrivateWorkspaceActivationEntryName(
  candidate: WorkspaceActivationCandidateV1,
): string {
  return `${WORKSPACE_ENTRY_PREFIX}${candidate.entryIdentity}`
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

async function requireWorkspaceIntent(intentInput: ReceiveIntent): Promise<ReceiveIntent & Readonly<{
  plan: Extract<ReceiveIntent['plan'], { kind: 'workspace-then-publish' }>
}>> {
  const intent = await validateReceiveIntent(intentInput)
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind === 'directory-tree') {
    throw new TypeError('origin-private namespace requires a workspace receive intent')
  }
  return intent as ReceiveIntent & Readonly<{
    plan: Extract<ReceiveIntent['plan'], { kind: 'workspace-then-publish' }>
  }>
}

function requireOriginPrivateStorage(): OriginPrivateStorageManager {
  const storage = navigator.storage as Partial<OriginPrivateStorageManager>
  if (typeof storage.getDirectory !== 'function') {
    throw new DOMException('Origin-private file system is unavailable', 'NotSupportedError')
  }
  return storage as OriginPrivateStorageManager
}

function workspaceNamespace(input: {
  readonly intent: ReceiveIntent & Readonly<{
    plan: Extract<ReceiveIntent['plan'], { kind: 'workspace-then-publish' }>
  }>
  readonly parent: FileSystemDirectoryHandle
  readonly entryName: string
  readonly root: FileSystemDirectoryHandle
  readonly rootHandleId: string
  readonly rootOwnedObjectId: string
}): OriginPrivateWorkspaceNamespace {
  return Object.freeze({
    operationId: input.intent.operationId,
    authorityRef: input.intent.plan.workspace.repositoryRef,
    parent: input.parent,
    entryName: input.entryName,
    root: input.root,
    rootHandleId: input.rootHandleId,
    rootOwnedObjectId: input.rootOwnedObjectId,
  })
}

async function writeWorkspaceActivationMarker(
  root: FileSystemDirectoryHandle,
  candidate: WorkspaceActivationCandidateV1,
): Promise<void> {
  const marker = await root.getFileHandle(WORKSPACE_OWNERSHIP_MARKER, { create: true })
  const writable = await marker.createWritable()
  try {
    await writable.write(workspaceOwnershipMarker(candidate))
    await writable.close()
  } catch (error) {
    await writable.abort(error).catch(() => undefined)
    throw error
  }
}

function hasWorkspaceActivationMarker(
  root: FileSystemDirectoryHandle,
  candidate: WorkspaceActivationCandidateV1,
): Promise<boolean> {
  return hasWorkspaceOwnershipMarker(root, candidate)
}

async function hasWorkspaceOwnershipMarker(
  root: FileSystemDirectoryHandle,
  authority: Readonly<{
    readonly operationId: string
    readonly rootOwnedObjectId: string
    readonly repositoryAuthority: string
  }>,
): Promise<boolean> {
  try {
    const marker = await root.getFileHandle(WORKSPACE_OWNERSHIP_MARKER)
    return await (await marker.getFile()).text() === workspaceOwnershipMarker(authority)
  } catch (error) {
    if (errorNamed(error, 'NotFoundError')) return false
    throw error
  }
}

function workspaceOwnershipMarker(authority: Readonly<{
  readonly operationId: string
  readonly rootOwnedObjectId: string
  readonly repositoryAuthority: string
}>): string {
  return `${WORKSPACE_OWNERSHIP_MARKER_DOMAIN}\n${authority.operationId}\n${authority.rootOwnedObjectId}\n${authority.repositoryAuthority}\n`
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

function randomIdentity(): string {
  const bytes = new Uint8Array(RANDOM_IDENTITY_BYTES)
  crypto.getRandomValues(bytes)
  return encodeBase64Url(bytes)
}

function errorNamed(error: unknown, name: string): boolean {
  return typeof error === 'object' && error !== null && 'name' in error && error.name === name
}
