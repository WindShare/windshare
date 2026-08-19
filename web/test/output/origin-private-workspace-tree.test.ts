import { describe, expect, it } from 'vitest'

import type {
  PersistentHandleRecord,
  PersistentHandleRepository,
} from '../../src/output/persistence/journal'
import type { OpenedFileRevision } from '../../src/output/persistent-tree/contracts'
import {
  OriginPrivateWorkspaceTree,
  rawFileObjectId,
} from '../../src/output/origin-private/workspace-tree'
import type { OriginPrivateWorkspaceRoot } from '../../src/output/origin-private/workspace-root'
import { identity } from './planning/fixture'

describe('origin-private selected file authority', () => {
  it('plans without mutation and reopens the repository-selected object without resetting it', async () => {
    const root = new MemoryWorkspaceRoot()
    const handles = new MemoryHandleRepository()
    const tree = new OriginPrivateWorkspaceTree({
      root: root as unknown as OriginPrivateWorkspaceRoot,
      handles,
    })
    const path = ['report.bin']
    const revision: OpenedFileRevision = Object.freeze({
      fileId: identity(21),
      fileRevision: identity(22),
      exactSize: 4n,
    })

    const selected = await tree.proposeFileOwnedObjectId(path, revision)
    expect(selected).toBe(await rawFileObjectId(root.operationId, path, revision))
    expect(root.createCount).toBe(0)
    await expect(tree.inspectFileDestination(path, selected)).resolves.toBe('absent')

    const created = await tree.createFileAfterRevisionOpen(path, revision, selected)
    const reopenedTree = new OriginPrivateWorkspaceTree({
      root: root as unknown as OriginPrivateWorkspaceRoot,
      handles,
    })
    const reopened = await reopenedTree.openFile(path, selected)
    expect(reopened?.ownedObjectId).toBe(created.ownedObjectId)
    expect(root.createCount).toBe(1)
    expect(root.file(selected)?.writableOpenCount).toBe(0)
    await expect(reopenedTree.inspectFileDestination(path, selected)).resolves.toBe('occupied')
  })

  it('rejects a caller-selected object outside the deterministic plan', async () => {
    const root = new MemoryWorkspaceRoot()
    const tree = new OriginPrivateWorkspaceTree({
      root: root as unknown as OriginPrivateWorkspaceRoot,
      handles: new MemoryHandleRepository(),
    })
    const revision: OpenedFileRevision = Object.freeze({
      fileId: identity(31),
      fileRevision: identity(32),
      exactSize: 1n,
    })

    await expect(tree.createFileAfterRevisionOpen(
      ['report.bin'],
      revision,
      identity(99, 32),
    )).rejects.toThrow('selected raw file object does not match its materialization plan')
    expect(root.createCount).toBe(0)
  })
})

class MemoryHandleRepository implements PersistentHandleRepository {
  readonly #records = new Map<string, PersistentHandleRecord>()

  async putHandle(record: PersistentHandleRecord): Promise<void> {
    this.#records.set(record.id, record)
  }

  async readHandle(id: string): Promise<PersistentHandleRecord | undefined> {
    return this.#records.get(id)
  }

  async deleteHandle(id: string): Promise<void> {
    this.#records.delete(id)
  }
}

class MemoryWorkspaceRoot {
  readonly operationId = identity(1)
  readonly authorityRef = identity(2, 32)
  readonly #files = new Map<string, MemoryFileHandle>()
  createCount = 0

  async authorize(): Promise<void> {}
  async prepareContainers(): Promise<void> {}
  rootOwnedObjectId(): string { return identity(3, 32) }
  async readmit(): Promise<void> {}

  async readObject(
    _containerName: string,
    ownedObjectId: string,
  ): Promise<FileSystemFileHandle | undefined> {
    return this.#files.get(ownedObjectId) as unknown as FileSystemFileHandle | undefined
  }

  async createObject(
    _containerName: string,
    ownedObjectId: string,
  ): Promise<FileSystemFileHandle> {
    if (this.#files.has(ownedObjectId)) throw new DOMException('occupied', 'InvalidStateError')
    const file = new MemoryFileHandle()
    this.#files.set(ownedObjectId, file)
    this.createCount += 1
    return file as unknown as FileSystemFileHandle
  }

  async removeObject(
    _containerName: string,
    ownedObjectId: string,
    expected: FileSystemFileHandle,
  ): Promise<'removed' | 'already-absent'> {
    const current = this.#files.get(ownedObjectId)
    if (current === undefined) return 'already-absent'
    if (!await current.isSameEntry(expected)) throw new DOMException('mismatch', 'InvalidStateError')
    this.#files.delete(ownedObjectId)
    return 'removed'
  }

  async sameObject(left: FileSystemHandle, right: FileSystemHandle): Promise<boolean> {
    return left.isSameEntry(right)
  }

  file(ownedObjectId: string): MemoryFileHandle | undefined {
    return this.#files.get(ownedObjectId)
  }
}

class MemoryFileHandle {
  readonly kind = 'file' as const
  readonly name = 'memory-file'
  readonly #token = crypto.randomUUID()
  writableOpenCount = 0

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return (other as unknown as MemoryFileHandle).#token === this.#token
  }

  async getFile(): Promise<File> {
    return new File([], this.name)
  }

  async createWritable(): Promise<FileSystemWritableFileStream> {
    this.writableOpenCount += 1
    throw new Error('test does not open writers')
  }
}
