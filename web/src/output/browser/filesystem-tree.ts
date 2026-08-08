import { bigintToSafeNumber } from '../../content/geometry'
import { encodeBase64Url } from '../../crypto/bytes'
import { FILE_SYSTEM_ACCESS_BACKEND } from '../capability/contract'
import { FILE_CHECKPOINT_ID_BYTES, identityBytes } from '../persistence/checkpoint'
import {
  durableCheckpointNamespaceIdentity,
  type DurableCheckpointNamespaceIdentity,
} from '../persistence/namespace'
import type {
  PersistentDirectoryMaterialization,
  PersistentOutputTree,
  PersistentTreeFile,
} from '../persistent-tree/contracts'
import type { PersistentHandleRepository } from './indexeddb-repository'
import type {
  BrowserFileSystemMutationAuthority,
  BrowserFileSystemMutationKind,
} from './namespace-mutation'

const READ_WRITE_PERMISSION = Object.freeze({ mode: 'readwrite' as const })

interface PermissionCapableHandle extends FileSystemHandle {
  queryPermission?(descriptor?: { readonly mode?: 'read' | 'readwrite' }): Promise<PermissionState>
  requestPermission?(descriptor?: { readonly mode?: 'read' | 'readwrite' }): Promise<PermissionState>
}

type BoundPersistentHandleRepository = PersistentHandleRepository & {
  readonly binding: DurableCheckpointNamespaceIdentity
}

export interface BrowserFileSystemTreeOptions {
  readonly root: FileSystemDirectoryHandle
  readonly handles: BoundPersistentHandleRepository
  readonly randomIdentity?: () => string
}

export interface BrowserSharedFileSystemTreeOptions extends BrowserFileSystemTreeOptions {
  readonly mutations: BrowserFileSystemMutationAuthority
}

export type BrowserOwnedObjectState = 'owned' | 'absent' | 'changed'

export class BrowserFileSystemTree implements PersistentOutputTree {
  readonly #root: FileSystemDirectoryHandle
  readonly #handles: BoundPersistentHandleRepository
  readonly #randomIdentity: () => string
  readonly #mutations: BrowserFileSystemMutationAuthority | undefined

  private constructor(
    options: BrowserFileSystemTreeOptions,
    mutations: BrowserFileSystemMutationAuthority | undefined,
  ) {
    this.#root = options.root
    this.#handles = options.handles
    this.#randomIdentity = options.randomIdentity ?? createOwnedObjectIdentity
    this.#mutations = mutations
  }

  static forSharedRoot(options: BrowserSharedFileSystemTreeOptions): BrowserFileSystemTree {
    const binding = durableCheckpointNamespaceIdentity(options.handles.binding)
    if (binding.backend !== FILE_SYSTEM_ACCESS_BACKEND) {
      throw new TypeError('Only File System Access output may use a shared-root mutation authority')
    }
    if (!sameBinding(binding, options.mutations.binding)) {
      throw new TypeError('File System Access mutation authority does not match the output namespace')
    }
    return new BrowserFileSystemTree(options, options.mutations)
  }

  static forIsolatedNamespace(options: BrowserFileSystemTreeOptions): BrowserFileSystemTree {
    const binding = durableCheckpointNamespaceIdentity(options.handles.binding)
    if (binding.backend === FILE_SYSTEM_ACCESS_BACKEND) {
      throw new TypeError('File System Access output requires a shared-root mutation authority')
    }
    return new BrowserFileSystemTree(options, undefined)
  }

  async authorize(): Promise<void> {
    const root = this.#root as PermissionCapableHandle
    if (root.queryPermission === undefined) return
    const current = await root.queryPermission(READ_WRITE_PERMISSION)
    if (current === 'granted') return
    if (root.requestPermission === undefined ||
        await root.requestPermission(READ_WRITE_PERMISSION) !== 'granted') {
      throw new DOMException('Output permission was not granted', 'NotAllowedError')
    }
  }

  async ensureDirectory(
    path: readonly string[],
  ): Promise<PersistentDirectoryMaterialization> {
    requirePath(path)
    return this.#mutate('ensure-directory', path, async () => {
      let current = this.#root
      const parents = path.slice(0, -1)
      for (const segment of parents) {
        current = await current.getDirectoryHandle(segment)
      }
      const name = path.at(-1)
      if (name === undefined) throw new TypeError('Output directory path has no name')
      // Parent creation is forbidden here because every owned directory needs its
      // own journal record and cleanup identity.
      const materialized = await openOrCreateDirectory(current, name)
      current = materialized.handle
      const identity = ownedObjectIdentity(this.#randomIdentity())
      try {
        await this.#handles.putHandle(identity, current)
      } catch (error) {
        if (materialized.created) {
          try {
            await (path.length === 1 ? this.#root : await this.#directory(parents))
              .removeEntry(name)
          } catch (cleanupError) {
            throw new AggregateError(
              [error, cleanupError],
              'Directory handle persistence and cleanup failed',
              { cause: cleanupError },
            )
          }
        }
        throw error
      }
      return Object.freeze({ identity, created: materialized.created })
    })
  }

  async validateDirectory(path: readonly string[], identity: string): Promise<boolean> {
    return await this.ownedDirectoryState(path, identity) === 'owned'
  }

  async ownedDirectoryState(
    path: readonly string[],
    identity: string,
  ): Promise<BrowserOwnedObjectState> {
    const persisted = await this.#handles.getHandle(identity)
    try {
      const current = await this.#directory(path)
      if (persisted?.kind !== 'directory') return 'changed'
      return await current.isSameEntry(persisted) ? 'owned' : 'changed'
    } catch (error) {
      if (errorNamed(error, 'NotFoundError')) return 'absent'
      throw error
    }
  }

  async createFileExclusive(path: readonly string[]): Promise<PersistentTreeFile> {
    requirePath(path)
    return this.#mutate('create-file', path, async () => {
      const { parent, name } = await this.#parent(path)
      try {
        await parent.getFileHandle(name)
        throw new DOMException('Output file already exists', 'InvalidModificationError')
      } catch (error) {
        if (!errorNamed(error, 'NotFoundError')) throw error
      }
      // FSA has no exclusive-create primitive. The shared-root authority keeps
      // observation, creation, ownership persistence, and rollback in one cut.
      const handle = await parent.getFileHandle(name, { create: true })
      const identity = ownedObjectIdentity(this.#randomIdentity())
      try {
        await this.#handles.putHandle(identity, handle)
      } catch (error) {
        try {
          await parent.removeEntry(name)
        } catch (cleanupError) {
          throw new AggregateError(
            [error, cleanupError],
            'File handle persistence and cleanup failed',
            { cause: cleanupError },
          )
        }
        throw error
      }
      return new BrowserPersistentFile(identity, handle)
    })
  }

  async ownedFileState(
    path: readonly string[],
    identity: string,
  ): Promise<BrowserOwnedObjectState> {
    const persisted = await this.#handles.getHandle(identity)
    try {
      const { parent, name } = await this.#parent(path)
      const current = await parent.getFileHandle(name)
      if (persisted?.kind !== 'file') return 'changed'
      return await current.isSameEntry(persisted) ? 'owned' : 'changed'
    } catch (error) {
      if (errorNamed(error, 'NotFoundError')) return 'absent'
      throw error
    }
  }

  async openFile(
    path: readonly string[],
    identity: string,
  ): Promise<PersistentTreeFile | undefined> {
    const persisted = await this.#handles.getHandle(identity)
    if (persisted?.kind !== 'file') return undefined
    try {
      const { parent, name } = await this.#parent(path)
      const current = await parent.getFileHandle(name)
      return await current.isSameEntry(persisted)
        ? new BrowserPersistentFile(identity, persisted as FileSystemFileHandle)
        : undefined
    } catch (error) {
      if (errorNamed(error, 'NotFoundError')) return undefined
      throw error
    }
  }

  async removeFile(path: readonly string[], identity: string): Promise<void> {
    requirePath(path)
    await this.#mutate('remove-file', path, async () => {
      const persisted = await this.#handles.getHandle(identity)
      try {
        const { parent, name } = await this.#parent(path)
        const current = await parent.getFileHandle(name)
        if (persisted?.kind !== 'file') {
          throw new DOMException('Owned output file handle is unavailable', 'InvalidStateError')
        }
        if (!await current.isSameEntry(persisted)) {
          throw new DOMException('Output file identity changed', 'InvalidModificationError')
        }
        await parent.removeEntry(name)
      } catch (error) {
        if (!errorNamed(error, 'NotFoundError')) throw error
      }
      // The missing path is success only after a discard journal has certified the
      // object. Removing the durable handle here makes that physical step replayable.
      await this.#handles.deleteHandle(identity)
    })
  }

  async removeDirectory(path: readonly string[], identity: string): Promise<void> {
    requirePath(path)
    await this.#mutate('remove-directory', path, async () => {
      const persisted = await this.#handles.getHandle(identity)
      try {
        const { parent, name } = await this.#parent(path)
        const current = await parent.getDirectoryHandle(name)
        if (persisted?.kind !== 'directory') {
          throw new DOMException('Owned output directory handle is unavailable', 'InvalidStateError')
        }
        if (!await current.isSameEntry(persisted)) {
          throw new DOMException('Output directory identity changed', 'InvalidModificationError')
        }
        await parent.removeEntry(name)
      } catch (error) {
        if (!errorNamed(error, 'NotFoundError')) throw error
      }
      await this.#handles.deleteHandle(identity)
    })
  }

  forgetIdentity(identity: string): Promise<void> {
    return this.#handles.deleteHandle(identity)
  }

  async #parent(
    path: readonly string[],
  ): Promise<{ readonly parent: FileSystemDirectoryHandle; readonly name: string }> {
    requirePath(path)
    const name = path.at(-1)
    if (name === undefined) throw new TypeError('Output path has no file name')
    const parent = path.length === 1 ? this.#root : await this.#directory(path.slice(0, -1))
    return { parent, name }
  }

  async #directory(path: readonly string[]): Promise<FileSystemDirectoryHandle> {
    requirePath(path)
    let current = this.#root
    for (const segment of path) current = await current.getDirectoryHandle(segment)
    return current
  }

  #mutate<T>(
    kind: BrowserFileSystemMutationKind,
    path: readonly string[],
    operation: () => Promise<T>,
  ): Promise<T> {
    return this.#mutations === undefined
      ? operation()
      : this.#mutations.mutate(kind, path, operation)
  }
}

class BrowserPersistentFile implements PersistentTreeFile {
  readonly identity: string
  readonly #handle: FileSystemFileHandle
  #writer: FileSystemWritableFileStream | undefined

  constructor(identity: string, handle: FileSystemFileHandle) {
    this.identity = identity
    this.#handle = handle
  }

  async writeAt(offset: bigint, data: Uint8Array): Promise<void> {
    this.#writer ??= await this.#handle.createWritable({ keepExistingData: true })
    await this.#writer.write({
      type: 'write',
      position: bigintToSafeNumber(offset, 'output offset'),
      data: data.slice(),
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

  close(): Promise<void> {
    return this.flush()
  }

  async read(): Promise<Blob> {
    await this.flush()
    return this.#handle.getFile()
  }
}

async function openOrCreateDirectory(
  parent: FileSystemDirectoryHandle,
  name: string,
): Promise<{ readonly handle: FileSystemDirectoryHandle; readonly created: boolean }> {
  try {
    return { handle: await parent.getDirectoryHandle(name), created: false }
  } catch (error) {
    if (!errorNamed(error, 'NotFoundError')) throw error
  }
  return { handle: await parent.getDirectoryHandle(name, { create: true }), created: true }
}

function requirePath(path: readonly string[]): void {
  if (path.length === 0 || path.some((segment) =>
    segment.length === 0 || segment === '.' || segment === '..' ||
    segment.includes('/') || segment.includes('\\') || segment.includes('\0'))) {
    throw new TypeError('Browser output path is not root-confined')
  }
}

function errorNamed(error: unknown, name: string): boolean {
  return typeof error === 'object' && error !== null &&
    'name' in error && (error as { readonly name?: unknown }).name === name
}

function sameBinding(
  left: DurableCheckpointNamespaceIdentity,
  right: DurableCheckpointNamespaceIdentity,
): boolean {
  const canonicalRight = durableCheckpointNamespaceIdentity(right)
  return left.backend === canonicalRight.backend &&
    left.transferIntentDigest === canonicalRight.transferIntentDigest &&
    left.rootIdentity === canonicalRight.rootIdentity
}

function ownedObjectIdentity(value: string): string {
  return encodeBase64Url(identityBytes(
    value,
    FILE_CHECKPOINT_ID_BYTES,
    'owned output object',
  ))
}

function createOwnedObjectIdentity(): string {
  const bytes = new Uint8Array(FILE_CHECKPOINT_ID_BYTES)
  const cryptoSource = globalThis.crypto
  if (cryptoSource?.getRandomValues === undefined) {
    throw new DOMException(
      'Secure owned-output identity generation is unavailable',
      'NotSupportedError',
    )
  }
  cryptoSource.getRandomValues(bytes)
  return encodeBase64Url(bytes)
}
