import { bigintToSafeNumber } from '../../content/geometry'
import type {
  PersistentDirectoryMaterialization,
  PersistentOutputTree,
  PersistentTreeFile,
} from '../persistent-tree/contracts'
import type { PersistentHandleRepository } from './indexeddb-repository'

const READ_WRITE_PERMISSION = Object.freeze({ mode: 'readwrite' as const })

interface PermissionCapableHandle extends FileSystemHandle {
  queryPermission?(descriptor?: { readonly mode?: 'read' | 'readwrite' }): Promise<PermissionState>
  requestPermission?(descriptor?: { readonly mode?: 'read' | 'readwrite' }): Promise<PermissionState>
}

export interface BrowserFileSystemTreeOptions {
  readonly root: FileSystemDirectoryHandle
  readonly handles: PersistentHandleRepository
  readonly randomIdentity?: () => string
}

export class BrowserFileSystemTree implements PersistentOutputTree {
  readonly #root: FileSystemDirectoryHandle
  readonly #handles: PersistentHandleRepository
  readonly #randomIdentity: () => string

  constructor(options: BrowserFileSystemTreeOptions) {
    this.#root = options.root
    this.#handles = options.handles
    this.#randomIdentity = options.randomIdentity ?? (() => crypto.randomUUID())
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
    const identity = this.#randomIdentity()
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
  }

  async validateDirectory(path: readonly string[], identity: string): Promise<boolean> {
    const persisted = await this.#handles.getHandle(identity)
    if (persisted?.kind !== 'directory') return false
    try {
      const current = await this.#directory(path)
      return current.isSameEntry(persisted)
    } catch (error) {
      if (errorNamed(error, 'NotFoundError')) return false
      throw error
    }
  }

  async createFileExclusive(path: readonly string[]): Promise<PersistentTreeFile> {
    const { parent, name } = await this.#parent(path)
    try {
      await parent.getFileHandle(name)
      throw new DOMException('Output file already exists', 'InvalidModificationError')
    } catch (error) {
      if (!errorNamed(error, 'NotFoundError')) throw error
    }
    const handle = await parent.getFileHandle(name, { create: true })
    const identity = this.#randomIdentity()
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
    const persisted = await this.#handles.getHandle(identity)
    if (persisted?.kind !== 'file') {
      throw new DOMException('Owned output file handle is unavailable', 'InvalidStateError')
    }
    const { parent, name } = await this.#parent(path)
    try {
      const current = await parent.getFileHandle(name)
      if (!await current.isSameEntry(persisted)) {
        throw new DOMException('Output file identity changed', 'InvalidModificationError')
      }
      await parent.removeEntry(name)
    } catch (error) {
      if (!errorNamed(error, 'NotFoundError')) throw error
    }
    await this.#handles.deleteHandle(identity)
  }

  async removeDirectory(path: readonly string[], identity: string): Promise<void> {
    const persisted = await this.#handles.getHandle(identity)
    if (persisted?.kind !== 'directory') {
      throw new DOMException('Owned output directory handle is unavailable', 'InvalidStateError')
    }
    const { parent, name } = await this.#parent(path)
    try {
      const current = await parent.getDirectoryHandle(name)
      if (!await current.isSameEntry(persisted)) {
        throw new DOMException('Output directory identity changed', 'InvalidModificationError')
      }
      await parent.removeEntry(name)
    } catch (error) {
      if (!errorNamed(error, 'NotFoundError')) throw error
    }
    await this.#handles.deleteHandle(identity)
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
