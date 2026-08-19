import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import { bigintToSafeNumber } from '../../content/geometry'
import { encodeBase64Url } from '../../crypto/bytes'
import type { NamedContainerEntryReservation } from '../../transfer/intent'
import { FILE_CHECKPOINT_ID_BYTES, identityBytes } from '../persistence/checkpoint'
import type { PersistentHandleRecord, PersistentHandleRepository } from '../persistence/journal'
import type {
  OpenedFileRevision,
  PersistentTreeFile,
} from '../persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type { PersistedFSAOperationBinding } from './indexeddb-root-binding'
import type { FSANamespaceMutationKind, FSARootMutationAuthority } from './namespace-mutation'

export const FSA_FILE_HANDLE_KIND = 1
const FSA_FILE_HANDLE_DOMAIN = 'windshare/fsa-file-handle/v1'

export type FSAFileIdentityStage =
  | 'parent-authority'
  | 'namespace-create'
  | 'writer-open'
  | 'checkpoint'
  | 'commit'

interface BrowserFileLineageAuthorityOptions {
  readonly binding: PersistedFSAOperationBinding
  readonly fileHandles: PersistentHandleRepository
  readonly mutations: FSARootMutationAuthority
  readonly prepareRoot: () => Promise<void>
  readonly verifyParent: () => Promise<FileSystemDirectoryHandle>
  readonly resolveParent: (path: readonly string[]) => Promise<Readonly<{
    parent: FileSystemDirectoryHandle
    name: string
  }>>
  readonly randomOwnedObjectId?: () => string
}

/**
 * File lineage owns the boundary between a selected journal object ID and an FSA
 * namespace entry. The tree facade supplies directory navigation but cannot bypass
 * claim-before-create or weaken the persisted-handle identity checks.
 */
export class BrowserFileLineageAuthority {
  readonly #binding: PersistedFSAOperationBinding
  readonly #fileHandles: PersistentHandleRepository
  readonly #mutations: FSARootMutationAuthority
  readonly #prepareRoot: () => Promise<void>
  readonly #verifyParent: () => Promise<FileSystemDirectoryHandle>
  readonly #resolveParent: BrowserFileLineageAuthorityOptions['resolveParent']
  readonly #randomOwnedObjectId: () => string

  constructor(options: BrowserFileLineageAuthorityOptions) {
    this.#binding = options.binding
    this.#fileHandles = options.fileHandles
    this.#mutations = options.mutations
    this.#prepareRoot = options.prepareRoot
    this.#verifyParent = options.verifyParent
    this.#resolveParent = options.resolveParent
    this.#randomOwnedObjectId = options.randomOwnedObjectId ?? createOwnedObjectId
  }

  async proposeOwnedObjectId(
    path: readonly string[],
    revision: OpenedFileRevision,
  ): Promise<string> {
    this.#filePath(path)
    requireOpenedRevision(revision)
    return requireOwnedObjectId(this.#randomOwnedObjectId())
  }

  async inspectDestination(
    path: readonly string[],
    selectedOwnedObjectId: string,
  ): Promise<'absent' | 'occupied'> {
    const canonicalPath = this.#filePath(path)
    requireOwnedObjectId(selectedOwnedObjectId)
    await this.#prepareRoot()
    return this.#mutations.run('create-file', async () => {
      await this.#verifyParent()
      const { parent, name } = await this.#resolveParent(canonicalPath)
      return await namespaceEntryExists(parent, name) ? 'occupied' : 'absent'
    })
  }

  async createAfterRevisionOpen(
    path: readonly string[],
    revision: OpenedFileRevision,
    selectedOwnedObjectId: string,
  ): Promise<PersistentTreeFile> {
    requireOpenedRevision(revision)
    const canonicalPath = this.#filePath(path)
    const ownedObjectId = requireOwnedObjectId(selectedOwnedObjectId)
    await this.#prepareRoot()
    return this.#mutations.run('create-file', async () => {
      await this.#verifyParent()
      const { parent, name } = await this.#resolveParent(canonicalPath)
      const handleId = fileHandleId(this.#binding.intent.operationId, ownedObjectId)
      const existing = await this.#readFileHandle(handleId, 'namespace-create')
      if (existing !== undefined) {
        return this.#openClaimedFile(canonicalPath, parent, name, ownedObjectId, existing)
      }
      if (await namespaceEntryExists(parent, name)) {
        // Presence after an absent preflight cannot distinguish our authority from an external actor.
        throw new TargetOwnershipUnknownError(
          'namespace-create',
          this.#binding.intent.operationId,
        )
      }
      return this.#createClaimedFile(canonicalPath, parent, name, ownedObjectId)
    })
  }

  async open(
    path: readonly string[],
    ownedObjectId: string,
  ): Promise<PersistentTreeFile | undefined> {
    const canonicalPath = this.#filePath(path)
    const objectId = requireOwnedObjectId(ownedObjectId)
    const record = await this.#readFileHandle(fileHandleId(
      this.#binding.intent.operationId,
      objectId,
    ), 'parent-authority')
    if (record === undefined) return undefined
    const handle = requireFileHandle(
      record.handle,
      this.#binding.intent.operationId,
      'parent-authority',
    )
    if (!this.#recordOwnsObject(record, objectId)) {
      throw new TargetOwnershipUnknownError(
        'parent-authority',
        this.#binding.intent.operationId,
      )
    }
    await this.#verifyFile(canonicalPath, objectId, 'writer-open')
    return this.#persistentFile(canonicalPath, objectId, handle)
  }

  async remove(path: readonly string[], ownedObjectId: string): Promise<void> {
    const canonicalPath = this.#filePath(path)
    const objectId = requireOwnedObjectId(ownedObjectId)
    await this.#mutations.run('remove-entry', async () => {
      await this.#verifyFile(canonicalPath, objectId, 'commit')
      const { parent, name } = await this.#resolveParent(canonicalPath)
      await parent.removeEntry(name)
      try {
        await this.#fileHandles.deleteHandle(fileHandleId(
          this.#binding.intent.operationId,
          objectId,
        ))
      } catch (cause) {
        throw new TargetOwnershipUnknownError(
          'cleanup',
          this.#binding.intent.operationId,
          { cause },
        )
      }
    })
  }

  async #openClaimedFile(
    canonicalPath: readonly string[],
    parent: FileSystemDirectoryHandle,
    name: string,
    ownedObjectId: string,
    existing: PersistentHandleRecord,
  ): Promise<PersistentTreeFile> {
    const persistedHandle = requireFileHandle(
      existing.handle,
      this.#binding.intent.operationId,
      'namespace-create',
    )
    const current = await requireFileEntry(parent, name, this.#binding.intent.operationId)
    const expected = fileHandleRecord(
      this.#binding.reservation,
      ownedObjectId,
      persistedHandle,
    )
    if (!await sameFileHandleRecord(existing, expected) ||
        !await sameEntry(
          current,
          persistedHandle,
          this.#binding.intent.operationId,
          'namespace-create',
        )) {
      throw new TargetOwnershipUnknownError(
        'namespace-create',
        this.#binding.intent.operationId,
      )
    }
    return this.#persistentFile(canonicalPath, ownedObjectId, persistedHandle)
  }

  async #createClaimedFile(
    canonicalPath: readonly string[],
    parent: FileSystemDirectoryHandle,
    name: string,
    ownedObjectId: string,
  ): Promise<PersistentTreeFile> {
    const created = await parent.getFileHandle(name, { create: true })
    const handleRecord = fileHandleRecord(
      this.#binding.reservation,
      ownedObjectId,
      created,
    )
    try {
      await this.#fileHandles.putHandle(handleRecord)
    } catch (cause) {
      throw new TargetOwnershipUnknownError(
        'namespace-create',
        this.#binding.intent.operationId,
        { cause },
      )
    }
    const persisted = await this.#readFileHandle(handleRecord.id, 'namespace-create')
    const current = await requireFileEntry(parent, name, this.#binding.intent.operationId)
    if (!await sameFileHandleRecord(persisted, handleRecord) ||
        !await sameEntry(
          current,
          created,
          this.#binding.intent.operationId,
          'namespace-create',
        )) {
      throw new TargetOwnershipUnknownError(
        'namespace-create',
        this.#binding.intent.operationId,
      )
    }
    return this.#persistentFile(canonicalPath, ownedObjectId, created)
  }

  #persistentFile(
    canonicalPath: readonly string[],
    ownedObjectId: string,
    handle: FileSystemFileHandle,
  ): PersistentTreeFile {
    return new BrowserPersistentFile(
      ownedObjectId,
      handle,
      stage => this.#verifyFile(canonicalPath, ownedObjectId, stage),
      (kind, operation) => this.#mutations.run(kind, operation),
    )
  }

  async #verifyFile(
    path: readonly string[],
    ownedObjectId: string,
    stage: 'writer-open' | 'checkpoint' | 'commit',
  ): Promise<void> {
    await this.#verifyParent()
    const record = await this.#readFileHandle(fileHandleId(
      this.#binding.intent.operationId,
      ownedObjectId,
    ), stage)
    const persistedHandle = record === undefined
      ? undefined
      : requireFileHandle(record.handle, this.#binding.intent.operationId, stage)
    if (record === undefined || !this.#recordOwnsObject(record, ownedObjectId) ||
        persistedHandle === undefined) {
      throw new TargetOwnershipUnknownError(stage, this.#binding.intent.operationId)
    }
    const { parent, name } = await this.#resolveParent(path)
    const current = await requireFileEntry(parent, name, this.#binding.intent.operationId)
    if (!await sameEntry(
      current,
      persistedHandle,
      this.#binding.intent.operationId,
      stage,
    )) {
      throw new TargetOwnershipUnknownError(stage, this.#binding.intent.operationId)
    }
  }

  #filePath(path: readonly string[]): readonly string[] {
    const canonical = snapshotPortableCatalogPath(path)
    if (this.#binding.reservation.entryKind === 'single-file' &&
        (canonical.length !== 1 || canonical[0] !== this.#binding.reservation.reservedName)) {
      throw new TypeError('Single-file DirectoryTree must write directly below its parent')
    }
    return canonical
  }

  #recordOwnsObject(record: PersistentHandleRecord, ownedObjectId: string): boolean {
    return record.operationId === this.#binding.intent.operationId &&
      record.kind === FSA_FILE_HANDLE_KIND &&
      record.authorityRef === this.#binding.reservation.authorityRef &&
      record.ownedObjectId === ownedObjectId
  }

  async #readFileHandle(
    id: string,
    stage: FSAFileIdentityStage,
  ): Promise<PersistentHandleRecord | undefined> {
    try {
      return await this.#fileHandles.readHandle(id)
    } catch (cause) {
      throw new TargetOwnershipUnknownError(stage, this.#binding.intent.operationId, { cause })
    }
  }
}

class BrowserPersistentFile implements PersistentTreeFile {
  readonly ownedObjectId: string
  readonly #handle: FileSystemFileHandle
  readonly #verify: PersistentTreeFile['verify']
  readonly #mutate: <T>(kind: FSANamespaceMutationKind, operation: () => Promise<T>) => Promise<T>
  #writer: FileSystemWritableFileStream | undefined

  constructor(
    ownedObjectId: string,
    handle: FileSystemFileHandle,
    verify: PersistentTreeFile['verify'],
    mutate: <T>(kind: FSANamespaceMutationKind, operation: () => Promise<T>) => Promise<T>,
  ) {
    this.ownedObjectId = ownedObjectId
    this.#handle = handle
    this.#verify = verify
    this.#mutate = mutate
  }

  async writeAt(offset: bigint, data: Uint8Array): Promise<void> {
    await this.#mutate('open-writer', async () => {
      await this.#verify('writer-open')
      this.#writer ??= await this.#handle.createWritable({ keepExistingData: true })
      await this.#writer.write({
        type: 'write',
        position: bigintToSafeNumber(offset, 'output offset'),
        data: data.slice(),
      })
    })
  }

  async flush(): Promise<void> {
    const writer = this.#writer
    if (writer === undefined) return
    this.#writer = undefined
    await writer.close()
  }

  async size(): Promise<bigint> {
    await this.flush()
    return BigInt((await this.#handle.getFile()).size)
  }

  verify(stage: 'writer-open' | 'checkpoint' | 'commit'): Promise<void> {
    return this.#verify(stage)
  }

  close(): Promise<void> {
    return this.flush()
  }

  async read(): Promise<Blob> {
    await this.flush()
    return this.#handle.getFile()
  }
}

function fileHandleRecord(
  reservation: NamedContainerEntryReservation,
  ownedObjectId: string,
  handle: FileSystemFileHandle,
): PersistentHandleRecord {
  return Object.freeze({
    id: fileHandleId(reservation.operationId, ownedObjectId),
    operationId: reservation.operationId,
    kind: FSA_FILE_HANDLE_KIND,
    authorityRef: reservation.authorityRef,
    ownedObjectId,
    handle,
  })
}

function fileHandleId(operationId: string, ownedObjectId: string): string {
  return `${FSA_FILE_HANDLE_DOMAIN}/${operationId}/${ownedObjectId}`
}

async function sameFileHandleRecord(
  actual: PersistentHandleRecord | undefined,
  expected: PersistentHandleRecord,
): Promise<boolean> {
  const actualHandle = actual === undefined
    ? undefined
    : requireFileHandle(actual.handle, expected.operationId, 'namespace-create')
  const expectedHandle = requireFileHandle(
    expected.handle,
    expected.operationId,
    'namespace-create',
  )
  return actual !== undefined && actual.id === expected.id &&
    actual.operationId === expected.operationId && actual.kind === expected.kind &&
    actual.authorityRef === expected.authorityRef &&
    actual.ownedObjectId === expected.ownedObjectId && actualHandle !== undefined &&
    await sameEntry(actualHandle, expectedHandle, expected.operationId, 'namespace-create')
}

function requireFileHandle(
  value: unknown,
  operationId: string,
  stage: FSAFileIdentityStage,
): FileSystemFileHandle {
  try {
    const handle = fileSystemHandle(value)
    if (handle.kind !== 'file') {
      throw new TypeError('Persisted FSA handle is not a file')
    }
    return handle as FileSystemFileHandle
  } catch (cause) {
    if (cause instanceof TargetOwnershipUnknownError) throw cause
    throw new TargetOwnershipUnknownError(stage, operationId, { cause })
  }
}

function fileSystemHandle(value: unknown): FileSystemHandle {
  if (typeof value !== 'object' || value === null ||
      !('kind' in value) || !('isSameEntry' in value) ||
      ((value as { readonly kind?: unknown }).kind !== 'file' &&
       (value as { readonly kind?: unknown }).kind !== 'directory') ||
      typeof (value as { readonly isSameEntry?: unknown }).isSameEntry !== 'function') {
    throw new TypeError('Persisted FSA handle is invalid')
  }
  return value as FileSystemHandle
}

function requireOpenedRevision(revision: OpenedFileRevision): void {
  identityBytes(revision.fileId, 16, 'file ID')
  identityBytes(revision.fileRevision, 16, 'file revision')
  if (typeof revision.exactSize !== 'bigint' || revision.exactSize < 0n ||
      revision.exactSize > 0xffff_ffff_ffff_ffffn) {
    throw new TypeError('Opened file revision has an invalid exact size')
  }
}

export function requireOwnedObjectId(value: string): string {
  return encodeBase64Url(identityBytes(value, FILE_CHECKPOINT_ID_BYTES, 'owned object ID'))
}

export function createOwnedObjectId(): string {
  const value = new Uint8Array(FILE_CHECKPOINT_ID_BYTES)
  crypto.getRandomValues(value)
  return requireOwnedObjectId(encodeBase64Url(value))
}

export async function namespaceEntryExists(
  parent: FileSystemDirectoryHandle,
  name: string,
): Promise<boolean> {
  try {
    await parent.getFileHandle(name)
    return true
  } catch (error) {
    if (!errorNamed(error, 'NotFoundError') && !errorNamed(error, 'TypeMismatchError')) throw error
    if (errorNamed(error, 'TypeMismatchError')) return true
  }
  try {
    await parent.getDirectoryHandle(name)
    return true
  } catch (error) {
    if (errorNamed(error, 'NotFoundError')) return false
    if (errorNamed(error, 'TypeMismatchError')) return true
    throw error
  }
}

async function requireFileEntry(
  parent: FileSystemDirectoryHandle,
  name: string,
  operationId: string,
): Promise<FileSystemFileHandle> {
  try {
    return await parent.getFileHandle(name)
  } catch (cause) {
    throw new TargetOwnershipUnknownError('parent-authority', operationId, { cause })
  }
}

export async function sameEntry(
  left: FileSystemHandle,
  right: FileSystemHandle,
  operationId: string,
  stage: FSAFileIdentityStage,
): Promise<boolean> {
  try {
    return await left.isSameEntry(right)
  } catch (cause) {
    throw new TargetOwnershipUnknownError(stage, operationId, { cause })
  }
}

function errorNamed(error: unknown, name: string): boolean {
  return typeof error === 'object' && error !== null &&
    'name' in error && (error as { readonly name?: unknown }).name === name
}
