import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import { bigintToSafeNumber } from '../../content/geometry'
import { encodeBase64Url } from '../../crypto/bytes'
import type { NamedContainerEntryReservation } from '../../transfer/intent'
import type { PersistentHandleRepository, PersistentHandleRecord } from '../persistence/journal'
import { FILE_CHECKPOINT_ID_BYTES, identityBytes } from '../persistence/checkpoint'
import {
  fsaOwnedDirectoryHandleId,
  persistFSAOwnedDirectory,
  readFSAOwnedDirectory,
  verifyFSAOperationBinding,
  type FSAOperationBindingRepository,
  type PersistedFSAOperationBinding,
} from './indexeddb-root-binding'
import type { FSARootMutationAuthority, FSANamespaceMutationKind } from './namespace-mutation'
import type {
  OpenedFileRevision,
  PersistentDirectoryMaterialization,
  PersistentOutputTree,
  PersistentTreeFile,
} from '../persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'

const READ_WRITE_PERMISSION = Object.freeze({ mode: 'readwrite' as const })
export const FSA_FILE_HANDLE_KIND = 1
const FSA_FILE_HANDLE_DOMAIN = 'windshare/fsa-file-handle/v1'
const FSA_DIRECTORY_LOCATOR_DOMAIN = 'windshare/fsa-directory-locator/v1'

type FSAFileIdentityStage =
  | 'parent-authority'
  | 'namespace-create'
  | 'writer-open'
  | 'checkpoint'
  | 'commit'

interface PermissionCapableDirectoryHandle extends FileSystemDirectoryHandle {
  queryPermission?(descriptor?: { readonly mode?: 'read' | 'readwrite' }): Promise<PermissionState>
  requestPermission?(descriptor?: { readonly mode?: 'read' | 'readwrite' }): Promise<PermissionState>
}

export interface BrowserFileSystemTreeOptions {
  readonly binding: PersistedFSAOperationBinding
  readonly operationRepository: FSAOperationBindingRepository
  readonly fileHandles: PersistentHandleRepository
  readonly mutations: FSARootMutationAuthority
  readonly randomOwnedObjectId?: () => string
}

export class BrowserFileSystemTree implements PersistentOutputTree {
  readonly #binding: PersistedFSAOperationBinding
  readonly #operationRepository: FSAOperationBindingRepository
  readonly #fileHandles: PersistentHandleRepository
  readonly #mutations: FSARootMutationAuthority
  readonly #randomOwnedObjectId: () => string
  #root: FileSystemDirectoryHandle | undefined
  #rootOwnedObjectId: string | undefined

  constructor(options: BrowserFileSystemTreeOptions) {
    this.#binding = options.binding
    this.#operationRepository = options.operationRepository
    this.#fileHandles = options.fileHandles
    this.#mutations = options.mutations
    this.#randomOwnedObjectId = options.randomOwnedObjectId ?? createOwnedObjectId
  }

  async authorize(): Promise<void> {
    const parent = (await this.#verifyParent()) as PermissionCapableDirectoryHandle
    if (parent.queryPermission === undefined) return
    if (await parent.queryPermission(READ_WRITE_PERMISSION) === 'granted') return
    if (parent.requestPermission === undefined ||
        await parent.requestPermission(READ_WRITE_PERMISSION) !== 'granted') {
      throw new DOMException('Output permission was not granted', 'NotAllowedError')
    }
    await this.#verifyParent()
  }

  async prepareRoot(): Promise<void> {
    await this.authorize()
    if (this.#binding.reservation.entryKind === 'single-file') return
    await this.#mutate('create-directory', async () => {
      const parent = await this.#verifyParent()
      const handleId = await fsaDirectoryHandleId(this.#binding.reservation, [])
      const persisted = await readFSAOwnedDirectory({
        repository: this.#operationRepository,
        reservation: this.#binding.reservation,
        handleId,
      })
      if (persisted !== undefined) {
        const current = await requireDirectoryEntry(
          parent,
          this.#binding.reservation.reservedName,
          this.#binding.intent.operationId,
        )
        if (!await sameEntry(current, persisted, this.#binding.intent.operationId, 'parent-authority')) {
          throw new TargetOwnershipUnknownError('parent-authority', this.#binding.intent.operationId)
        }
        const stored = await this.#operationRepository.readHandle<FileSystemDirectoryHandle>(handleId)
        if (stored?.ownedObjectId === undefined) {
          throw new TargetOwnershipUnknownError('parent-authority', this.#binding.intent.operationId)
        }
        this.#root = current
        this.#rootOwnedObjectId = stored.ownedObjectId
        return
      }
      if (await namespaceEntryExists(parent, this.#binding.reservation.reservedName)) {
        // The reservation proved absence before it was persisted. An unowned entry now
        // is an external race, not permission to merge a result tree.
        throw new TargetOwnershipUnknownError('namespace-create', this.#binding.intent.operationId)
      }
      const created = await parent.getDirectoryHandle(
        this.#binding.reservation.reservedName,
        { create: true },
      )
      const ownedObjectId = requireOwnedObjectId(this.#randomOwnedObjectId())
      await persistFSAOwnedDirectory({
        repository: this.#operationRepository,
        reservation: this.#binding.reservation,
        handleId,
        ownedObjectId,
        handle: created,
      })
      const current = await requireDirectoryEntry(
        parent,
        this.#binding.reservation.reservedName,
        this.#binding.intent.operationId,
      )
      if (!await sameEntry(current, created, this.#binding.intent.operationId, 'namespace-create')) {
        throw new TargetOwnershipUnknownError('namespace-create', this.#binding.intent.operationId)
      }
      this.#root = current
      this.#rootOwnedObjectId = ownedObjectId
    })
  }

  /**
   * Cleanup must attach to an already persisted root without inheriting the
   * materializer's create-if-absent behavior. A missing record or entry is an
   * ownership ambiguity, never permission to recreate the namespace.
   */
  async reopenOwnedRootForCleanup(): Promise<void> {
    await this.authorize()
    if (this.#binding.reservation.entryKind === 'single-file') return
    await this.#mutate('settle-operation', async () => {
      const parent = await this.#verifyParent()
      const handleId = await fsaDirectoryHandleId(this.#binding.reservation, [])
      const persisted = await readFSAOwnedDirectory({
        repository: this.#operationRepository,
        reservation: this.#binding.reservation,
        handleId,
      })
      const record = await this.#operationRepository.readHandle<FileSystemDirectoryHandle>(handleId)
      if (persisted === undefined || record?.ownedObjectId === undefined) {
        throw new TargetOwnershipUnknownError('cleanup', this.#binding.intent.operationId)
      }
      const current = await requireDirectoryEntry(
        parent,
        this.#binding.reservation.reservedName,
        this.#binding.intent.operationId,
      )
      if (!await sameEntry(current, persisted, this.#binding.intent.operationId, 'parent-authority')) {
        throw new TargetOwnershipUnknownError('parent-authority', this.#binding.intent.operationId)
      }
      this.#root = current
      this.#rootOwnedObjectId = record.ownedObjectId
    })
  }

  async ensureDirectory(
    path: readonly string[],
  ): Promise<PersistentDirectoryMaterialization> {
    if (this.#binding.reservation.entryKind === 'single-file') {
      throw new TypeError('Single-file DirectoryTree cannot materialize a directory')
    }
    const canonicalPath = snapshotRelativePath(path, true)
    await this.#requirePreparedRoot()
    if (canonicalPath.length === 0) {
      if (this.#rootOwnedObjectId === undefined) {
        throw new TargetOwnershipUnknownError('parent-authority', this.#binding.intent.operationId)
      }
      return Object.freeze({ ownedObjectId: this.#rootOwnedObjectId, created: false })
    }
    return this.#mutate('create-directory', async () => {
      await this.#verifyParent()
      const parent = await this.#directory(canonicalPath.slice(0, -1))
      const name = canonicalPath.at(-1)!
      const handleId = await fsaDirectoryHandleId(this.#binding.reservation, canonicalPath)
      const persisted = await readFSAOwnedDirectory({
        repository: this.#operationRepository,
        reservation: this.#binding.reservation,
        handleId,
      })
      if (persisted !== undefined) {
        const current = await requireDirectoryEntry(
          parent,
          name,
          this.#binding.intent.operationId,
        )
        if (!await sameEntry(current, persisted, this.#binding.intent.operationId, 'parent-authority')) {
          throw new TargetOwnershipUnknownError('parent-authority', this.#binding.intent.operationId)
        }
        const record = await this.#operationRepository.readHandle<FileSystemDirectoryHandle>(handleId)
        if (record?.ownedObjectId === undefined) {
          throw new TargetOwnershipUnknownError('parent-authority', this.#binding.intent.operationId)
        }
        return Object.freeze({ ownedObjectId: record.ownedObjectId, created: false })
      }
      if (await namespaceEntryExists(parent, name)) {
        throw new TargetOwnershipUnknownError('namespace-create', this.#binding.intent.operationId)
      }
      const created = await parent.getDirectoryHandle(name, { create: true })
      const ownedObjectId = requireOwnedObjectId(this.#randomOwnedObjectId())
      await persistFSAOwnedDirectory({
        repository: this.#operationRepository,
        reservation: this.#binding.reservation,
        handleId,
        ownedObjectId,
        handle: created,
      })
      const current = await requireDirectoryEntry(
        parent,
        name,
        this.#binding.intent.operationId,
      )
      if (!await sameEntry(current, created, this.#binding.intent.operationId, 'namespace-create')) {
        throw new TargetOwnershipUnknownError('namespace-create', this.#binding.intent.operationId)
      }
      return Object.freeze({ ownedObjectId, created: true })
    })
  }

  async validateDirectory(path: readonly string[], ownedObjectId: string): Promise<boolean> {
    if (this.#binding.reservation.entryKind === 'single-file') return false
    const canonicalPath = snapshotRelativePath(path, true)
    const handleId = await fsaDirectoryHandleId(this.#binding.reservation, canonicalPath)
    const persisted = await readFSAOwnedDirectory({
      repository: this.#operationRepository,
      reservation: this.#binding.reservation,
      handleId,
      ownedObjectId,
    })
    if (persisted === undefined) return false
    const current = canonicalPath.length === 0
      ? await requireDirectoryEntry(
          await this.#verifyParent(),
          this.#binding.reservation.reservedName,
          this.#binding.intent.operationId,
        )
      : await this.#directory(canonicalPath)
    return sameEntry(current, persisted, this.#binding.intent.operationId, 'parent-authority')
  }

  async createFileAfterRevisionOpen(
    path: readonly string[],
    revision: OpenedFileRevision,
  ): Promise<PersistentTreeFile> {
    requireOpenedRevision(revision)
    const canonicalPath = this.#filePath(path)
    await this.#requirePreparedRoot()
    return this.#mutate('create-file', async () => {
      await this.#verifyParent()
      const { parent, name } = await this.#parent(canonicalPath)
      if (await namespaceEntryExists(parent, name)) {
        throw new TargetOwnershipUnknownError('namespace-create', this.#binding.intent.operationId)
      }
      const created = await parent.getFileHandle(name, { create: true })
      const ownedObjectId = requireOwnedObjectId(this.#randomOwnedObjectId())
      const handleRecord = fileHandleRecord(
        this.#binding.reservation,
        ownedObjectId,
        created,
      )
      const existing = await this.#readFileHandle(handleRecord.id, 'namespace-create')
      if (existing !== undefined) {
        throw new TargetOwnershipUnknownError('namespace-create', this.#binding.intent.operationId)
      }
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
          !await sameEntry(current, created, this.#binding.intent.operationId, 'namespace-create')) {
        throw new TargetOwnershipUnknownError('namespace-create', this.#binding.intent.operationId)
      }
      return new BrowserPersistentFile(
        ownedObjectId,
        created,
        (stage) => this.#verifyFile(canonicalPath, ownedObjectId, stage),
        (kind, operation) => this.#mutate(kind, operation),
      )
    })
  }

  async openFile(
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
    if (record.operationId !== this.#binding.intent.operationId ||
        record.kind !== FSA_FILE_HANDLE_KIND ||
        record.authorityRef !== this.#binding.reservation.authorityRef ||
        record.ownedObjectId !== objectId) {
      throw new TargetOwnershipUnknownError('parent-authority', this.#binding.intent.operationId)
    }
    await this.#verifyFile(canonicalPath, objectId, 'writer-open')
    return new BrowserPersistentFile(
      objectId,
      handle,
      (stage) => this.#verifyFile(canonicalPath, objectId, stage),
      (kind, operation) => this.#mutate(kind, operation),
    )
  }

  async removeFile(path: readonly string[], ownedObjectId: string): Promise<void> {
    const canonicalPath = this.#filePath(path)
    const objectId = requireOwnedObjectId(ownedObjectId)
    await this.#mutate('remove-entry', async () => {
      await this.#verifyFile(canonicalPath, objectId, 'commit')
      const { parent, name } = await this.#parent(canonicalPath)
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

  async removeDirectory(path: readonly string[], ownedObjectId: string): Promise<void> {
    if (this.#binding.reservation.entryKind === 'single-file') {
      throw new TypeError('Single-file DirectoryTree has no owned directory')
    }
    const canonicalPath = snapshotRelativePath(path, true)
    if (!await this.validateDirectory(canonicalPath, ownedObjectId)) {
      throw new TargetOwnershipUnknownError('cleanup', this.#binding.intent.operationId)
    }
    await this.#mutate('remove-entry', async () => {
      if (canonicalPath.length === 0) {
        const parent = await this.#verifyParent()
        await parent.removeEntry(this.#binding.reservation.reservedName, { recursive: true })
        return
      }
      const { parent, name } = await this.#parent(canonicalPath)
      await parent.removeEntry(name, { recursive: true })
    })
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
    if (record === undefined || record.operationId !== this.#binding.intent.operationId ||
        record.kind !== FSA_FILE_HANDLE_KIND ||
        record.authorityRef !== this.#binding.reservation.authorityRef ||
        record.ownedObjectId !== ownedObjectId || persistedHandle === undefined) {
      throw new TargetOwnershipUnknownError(stage, this.#binding.intent.operationId)
    }
    const { parent, name } = await this.#parent(path)
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

  async #verifyParent(): Promise<FileSystemDirectoryHandle> {
    const verified = await verifyFSAOperationBinding({
      repository: this.#operationRepository,
      intent: this.#binding.intent,
      expectedParent: this.#binding.parent,
    })
    return verified.parent
  }

  async #requirePreparedRoot(): Promise<void> {
    if (this.#binding.reservation.entryKind === 'result-root' && this.#root === undefined) {
      await this.prepareRoot()
    }
  }

  #filePath(path: readonly string[]): readonly string[] {
    const canonical = snapshotRelativePath(path, false)
    if (this.#binding.reservation.entryKind === 'single-file' &&
        (canonical.length !== 1 || canonical[0] !== this.#binding.reservation.reservedName)) {
      throw new TypeError('Single-file DirectoryTree must write directly below its parent')
    }
    return canonical
  }

  async #parent(path: readonly string[]): Promise<{
    readonly parent: FileSystemDirectoryHandle
    readonly name: string
  }> {
    const name = path.at(-1)
    if (name === undefined) throw new TypeError('Output file path has no leaf')
    if (this.#binding.reservation.entryKind === 'single-file') {
      return { parent: await this.#verifyParent(), name }
    }
    return { parent: await this.#directory(path.slice(0, -1)), name }
  }

  async #directory(path: readonly string[]): Promise<FileSystemDirectoryHandle> {
    if (this.#root === undefined) {
      throw new TargetOwnershipUnknownError('parent-authority', this.#binding.intent.operationId)
    }
    let current = this.#root
    const walked: string[] = []
    for (const segment of path) {
      walked.push(segment)
      const handleId = await fsaDirectoryHandleId(this.#binding.reservation, walked)
      const persisted = await readFSAOwnedDirectory({
        repository: this.#operationRepository,
        reservation: this.#binding.reservation,
        handleId,
      })
      if (persisted === undefined) {
        throw new TargetOwnershipUnknownError('parent-authority', this.#binding.intent.operationId)
      }
      const next = await requireDirectoryEntry(
        current,
        segment,
        this.#binding.intent.operationId,
      )
      if (!await sameEntry(next, persisted, this.#binding.intent.operationId, 'parent-authority')) {
        throw new TargetOwnershipUnknownError('parent-authority', this.#binding.intent.operationId)
      }
      current = next
    }
    return current
  }

  #mutate<T>(kind: FSANamespaceMutationKind, operation: () => Promise<T>): Promise<T> {
    return this.#mutations.run(kind, operation)
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

export async function fsaDirectoryHandleId(
  reservation: NamedContainerEntryReservation,
  path: readonly string[],
): Promise<string> {
  const encodedPath = path.map((segment) => `${segment.length}:${segment}`).join('/')
  const material = new TextEncoder().encode(
    `${FSA_DIRECTORY_LOCATOR_DOMAIN}\0${reservation.digest}\0${encodedPath}`,
  )
  const digest = encodeBase64Url(new Uint8Array(await crypto.subtle.digest('SHA-256', material)))
  return fsaOwnedDirectoryHandleId(reservation.operationId, digest)
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

function snapshotRelativePath(path: readonly string[], allowEmpty: boolean): readonly string[] {
  if (allowEmpty && path.length === 0) return Object.freeze([])
  return snapshotPortableCatalogPath(path)
}

function requireOpenedRevision(revision: OpenedFileRevision): void {
  identityBytes(revision.fileId, 16, 'file ID')
  identityBytes(revision.fileRevision, 16, 'file revision')
  if (typeof revision.exactSize !== 'bigint' || revision.exactSize < 0n ||
      revision.exactSize > 0xffff_ffff_ffff_ffffn) {
    throw new TypeError('Opened file revision has an invalid exact size')
  }
}

function requireOwnedObjectId(value: string): string {
  return encodeBase64Url(identityBytes(value, FILE_CHECKPOINT_ID_BYTES, 'owned object ID'))
}

function createOwnedObjectId(): string {
  const value = new Uint8Array(FILE_CHECKPOINT_ID_BYTES)
  crypto.getRandomValues(value)
  return requireOwnedObjectId(encodeBase64Url(value))
}

async function namespaceEntryExists(
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

async function sameEntry(
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
