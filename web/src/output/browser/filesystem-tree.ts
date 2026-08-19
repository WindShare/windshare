import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import { encodeBase64Url } from '../../crypto/bytes'
import type { NamedContainerEntryReservation } from '../../transfer/intent'
import type { PersistentHandleRepository } from '../persistence/journal'
import {
  BrowserFileLineageAuthority,
  createOwnedObjectId,
  namespaceEntryExists,
  requireOwnedObjectId,
  sameEntry,
} from './filesystem-file-lineage'
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

export { FSA_FILE_HANDLE_KIND } from './filesystem-file-lineage'

const READ_WRITE_PERMISSION = Object.freeze({ mode: 'readwrite' as const })
const FSA_DIRECTORY_LOCATOR_DOMAIN = 'windshare/fsa-directory-locator/v1'

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
  readonly #mutations: FSARootMutationAuthority
  readonly #randomOwnedObjectId: () => string
  readonly #files: BrowserFileLineageAuthority
  #root: FileSystemDirectoryHandle | undefined
  #rootOwnedObjectId: string | undefined

  constructor(options: BrowserFileSystemTreeOptions) {
    this.#binding = options.binding
    this.#operationRepository = options.operationRepository
    this.#mutations = options.mutations
    this.#randomOwnedObjectId = options.randomOwnedObjectId ?? createOwnedObjectId
    this.#files = new BrowserFileLineageAuthority({
      binding: options.binding,
      fileHandles: options.fileHandles,
      mutations: options.mutations,
      prepareRoot: () => this.#requirePreparedRoot(),
      verifyParent: () => this.#verifyParent(),
      resolveParent: path => this.#parent(path),
      randomOwnedObjectId: this.#randomOwnedObjectId,
    })
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

  proposeFileOwnedObjectId(
    path: readonly string[],
    revision: OpenedFileRevision,
  ): Promise<string> {
    return this.#files.proposeOwnedObjectId(path, revision)
  }

  inspectFileDestination(
    path: readonly string[],
    selectedOwnedObjectId: string,
  ): Promise<'absent' | 'occupied'> {
    return this.#files.inspectDestination(path, selectedOwnedObjectId)
  }

  createFileAfterRevisionOpen(
    path: readonly string[],
    revision: OpenedFileRevision,
    selectedOwnedObjectId: string,
  ): Promise<PersistentTreeFile> {
    return this.#files.createAfterRevisionOpen(path, revision, selectedOwnedObjectId)
  }

  openFile(
    path: readonly string[],
    ownedObjectId: string,
  ): Promise<PersistentTreeFile | undefined> {
    return this.#files.open(path, ownedObjectId)
  }

  removeFile(path: readonly string[], ownedObjectId: string): Promise<void> {
    return this.#files.remove(path, ownedObjectId)
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

function snapshotRelativePath(path: readonly string[], allowEmpty: boolean): readonly string[] {
  if (allowEmpty && path.length === 0) return Object.freeze([])
  return snapshotPortableCatalogPath(path)
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
