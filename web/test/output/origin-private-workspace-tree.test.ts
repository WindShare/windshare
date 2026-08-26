import { describe, expect, it, vi } from 'vitest'

import type {
  PersistentHandleRecord,
  PersistentHandleRepository,
} from '../../src/output/persistence/journal'
import type { OpenedFileRevision } from '../../src/output/persistent-tree/contracts'
import {
  openOriginPrivateWorkspaceNamespace,
  OriginPrivateWorkspaceNamespaceOpenError,
} from '../../src/output/origin-private/namespace'
import {
  RECEIVE_RECORD_LIFECYCLE_STATE,
  RECEIVE_RECORD_WORKSPACE_ACTIVATION,
  decodeStoredWorkspaceActivationCandidate,
  type ManifestPageRecord,
  type PersistedReceiveRecord,
  type ReceiveOperationHandleRecord,
  type ReceiveOperationLeaseRecord,
  type ReceiveRecordKind,
} from '../../src/output/workspace/records'
import {
  prepareReceiveOperationTransition,
  type ReceiveOperationTransition,
  type WorkspaceActivationJournalRepository,
} from '../../src/output/workspace/repository'
import { recoverWorkspaceActivationCandidates } from '../../src/output/workspace/activation-recovery'
import { decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import {
  createReceiveIntent,
  createSelectionSpec,
  createSyntheticSelectionResultRoot,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
  createZipArchiveArtifact,
  deriveArtifactChoiceIdentity,
} from '../../src/transfer/intent'
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
    expect(reopened?.preservingWriterCost?.(3n)).toEqual({
      prefixCopyBytes: 3n,
      writeAmplificationBytes: 3n,
      temporaryBytes: 3n,
    })
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

describe('origin-private workspace activation commit cut', () => {
  it('publishes the journal owner before creating and promoting the namespace', async () => {
    const intent = await workspaceIntent()
    const repository = new NamespaceRepository()
    const parent = new NamespaceDirectoryHandle('root', true)
    const onPersistenceCommitted = vi.fn()
    const onActivationCandidateCommitted = vi.fn()

    const opening = openOriginPrivateWorkspaceNamespace({
      receiveIntent: intent,
      preClickRanking: await selectedRanking(intent),
      repository,
      storage: { getDirectory: async () => parent as unknown as FileSystemDirectoryHandle } as StorageManager & {
        getDirectory(): Promise<FileSystemDirectoryHandle>
      },
      randomOwnedObjectId: () => identity(91, 32),
      randomEntryIdentity: () => identity(90, 32),
      onActivationCandidateCommitted,
      onPersistenceCommitted,
    })

    await expect(opening).resolves.toEqual(expect.objectContaining({ operationId: intent.operationId }))
    expect(onActivationCandidateCommitted).toHaveBeenCalledOnce()
    expect(onPersistenceCommitted).toHaveBeenCalledOnce()
    expect(repository.handles).toHaveLength(1)
    expect(parent.hasEntries).toBe(true)
  })

  it('proves namespace absence when the repository transaction rejects before the durable cut', async () => {
    const intent = await workspaceIntent()
    const repository = new NamespaceRepository(true)
    const parent = new NamespaceDirectoryHandle('root', true)
    const onPersistenceCommitted = vi.fn()

    const opening = openOriginPrivateWorkspaceNamespace({
      receiveIntent: intent,
      preClickRanking: await selectedRanking(intent),
      repository,
      storage: { getDirectory: async () => parent as unknown as FileSystemDirectoryHandle } as StorageManager & {
        getDirectory(): Promise<FileSystemDirectoryHandle>
      },
      randomOwnedObjectId: () => identity(92, 32),
      randomEntryIdentity: () => identity(93, 32),
      onPersistenceCommitted,
    })

    const error = await opening.catch((reason: unknown) => reason)
    expect(error).not.toBeInstanceOf(OriginPrivateWorkspaceNamespaceOpenError)
    expect(onPersistenceCommitted).not.toHaveBeenCalled()
    expect(repository.handles).toEqual([])
    expect(parent.hasEntries).toBe(false)
  })

  it('removes an activation journal only after fresh-runtime OPFS absence proof', async () => {
    const intent = await workspaceIntent()
    const repository = new NamespaceRepository()
    const parent = new NamespaceDirectoryHandle('root', true)

    await expect(openOriginPrivateWorkspaceNamespace({
      receiveIntent: intent,
      preClickRanking: await selectedRanking(intent),
      repository,
      storage: storageFor(parent),
      randomEntryIdentity: () => identity(94, 32),
      randomOwnedObjectId: () => identity(95, 32),
      onActivationCandidateCommitted: () => { throw new Error('crash after journal commit') },
    })).rejects.toThrow('crash after journal commit')

    expect(await repository.listWorkspaceActivationCandidates()).toHaveLength(1)
    expect(parent.hasEntries).toBe(false)
    await recoverWorkspaceActivationCandidates({
      repository,
      storage: storageFor(parent),
      locks: immediateLocks(),
    })
    expect(await repository.listWorkspaceActivationCandidates()).toEqual([])
    expect(await repository.listRecords(intent.operationId)).toEqual([])
  })

  it('retains NeedsAttention when a crash leaves an unmarked OPFS entry', async () => {
    const intent = await workspaceIntent()
    const repository = new NamespaceRepository()
    const parent = new NamespaceDirectoryHandle('root', true)
    parent.failAfterNextDirectoryCreate()

    await expect(openOriginPrivateWorkspaceNamespace({
      receiveIntent: intent,
      preClickRanking: await selectedRanking(intent),
      repository,
      storage: storageFor(parent),
      randomEntryIdentity: () => identity(96, 32),
      randomOwnedObjectId: () => identity(97, 32),
    })).rejects.toBeInstanceOf(OriginPrivateWorkspaceNamespaceOpenError)
    await recoverWorkspaceActivationCandidates({
      repository,
      storage: storageFor(parent),
      locks: immediateLocks(),
    })

    expect(await repository.listWorkspaceActivationCandidates()).toHaveLength(1)
    expect(repository.handles).toEqual([])
    const lifecycle = await repository.readLifecycle(intent.operationId)
    expect(lifecycle === undefined ? undefined : decodeStoredReceiveLifecycleState(lifecycle)).toEqual(
      expect.objectContaining({ kind: 'needs-attention', reason: 'target-ownership-unknown' }),
    )
  })

  it('promotes only the exact journal marker after a marker-complete reload cut', async () => {
    const intent = await workspaceIntent()
    const repository = new NamespaceRepository()
    const parent = new NamespaceDirectoryHandle('root', true)
    const abort = new AbortController()
    parent.afterNextMarkerClose(() => abort.abort('reload cut'))

    await expect(openOriginPrivateWorkspaceNamespace({
      receiveIntent: intent,
      preClickRanking: await selectedRanking(intent),
      repository,
      storage: storageFor(parent),
      signal: abort.signal,
      randomEntryIdentity: () => identity(98, 32),
      randomOwnedObjectId: () => identity(99, 32),
    })).rejects.toBeInstanceOf(OriginPrivateWorkspaceNamespaceOpenError)
    await recoverWorkspaceActivationCandidates({
      repository,
      storage: storageFor(parent),
      locks: immediateLocks(),
    })

    expect(await repository.listWorkspaceActivationCandidates()).toEqual([])
    expect(repository.handles).toHaveLength(1)
    const lifecycle = await repository.readLifecycle(intent.operationId)
    expect(lifecycle === undefined ? undefined : decodeStoredReceiveLifecycleState(lifecycle)).toEqual(
      expect.objectContaining({ kind: 'needs-attention', reason: 'cleanup-unknown' }),
    )
  })

  it.each(['replacement', 'invalid-marker'] as const)(
    'does not transfer a journal candidate to a same-name %s entry',
    async (mutation) => {
      const intent = await workspaceIntent()
      const repository = new NamespaceRepository()
      const parent = new NamespaceDirectoryHandle('root', true)
      const abort = new AbortController()
      parent.afterNextMarkerClose(() => abort.abort('reload cut'))
      await expect(openOriginPrivateWorkspaceNamespace({
        receiveIntent: intent,
        preClickRanking: await selectedRanking(intent),
        repository,
        storage: storageFor(parent),
        signal: abort.signal,
        randomEntryIdentity: () => identity(100, 32),
        randomOwnedObjectId: () => identity(101, 32),
      })).rejects.toBeInstanceOf(OriginPrivateWorkspaceNamespaceOpenError)
      if (mutation === 'replacement') parent.replaceOnlyDirectory()
      else parent.corruptOnlyDirectoryMarker()

      await recoverWorkspaceActivationCandidates({
        repository,
        storage: storageFor(parent),
        locks: immediateLocks(),
      })
      expect(repository.handles).toEqual([])
      expect(await repository.listWorkspaceActivationCandidates()).toHaveLength(1)
    },
  )

  it('keeps the promoted operation visible when the page dies before bound return', async () => {
    const intent = await workspaceIntent()
    const repository = new NamespaceRepository()
    const parent = new NamespaceDirectoryHandle('root', true)
    await expect(openOriginPrivateWorkspaceNamespace({
      receiveIntent: intent,
      preClickRanking: await selectedRanking(intent),
      repository,
      storage: storageFor(parent),
      randomEntryIdentity: () => identity(102, 32),
      randomOwnedObjectId: () => identity(103, 32),
      onPersistenceCommitted: () => { throw new Error('crash after promotion') },
    })).rejects.toBeInstanceOf(OriginPrivateWorkspaceNamespaceOpenError)

    await recoverWorkspaceActivationCandidates({
      repository,
      storage: storageFor(parent),
      locks: immediateLocks(),
    })
    expect(await repository.listWorkspaceActivationCandidates()).toEqual([])
    expect(repository.handles).toHaveLength(1)
    expect(await repository.listRecords(intent.operationId)).not.toEqual([])
    const lifecycle = await repository.readLifecycle(intent.operationId)
    expect(lifecycle === undefined ? undefined : decodeStoredReceiveLifecycleState(lifecycle)).toEqual(
      expect.objectContaining({ kind: 'needs-attention', reason: 'cleanup-unknown' }),
    )
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

class NamespaceRepository implements WorkspaceActivationJournalRepository {
  readonly #records = new Map<string, PersistedReceiveRecord>()
  readonly #pages = new Map<string, ManifestPageRecord>()
  readonly #handles = new Map<string, ReceiveOperationHandleRecord>()
  readonly #leases = new Map<string, ReceiveOperationLeaseRecord>()
  readonly #rejectCommit: boolean

  constructor(rejectCommit = false) {
    this.#rejectCommit = rejectCommit
  }

  get handles(): readonly ReceiveOperationHandleRecord[] {
    return [...this.#handles.values()]
  }

  async commitTransition(transition: ReceiveOperationTransition): Promise<void> {
    if (this.#rejectCommit) throw new DOMException('transaction rejected', 'QuotaExceededError')
    const prepared = await prepareReceiveOperationTransition(transition)
    for (const id of prepared.deleteRecordIds) this.#records.delete(id)
    for (const id of prepared.deleteManifestPageIds) this.#pages.delete(id)
    for (const id of prepared.deleteHandleIds) this.#handles.delete(id)
    for (const record of prepared.records) this.#records.set(record.id, record)
    for (const page of prepared.manifestPages) this.#pages.set(page.id, page)
    for (const handle of prepared.handles) this.#handles.set(handle.id, handle)
    if (prepared.lease?.kind === 'put') this.#leases.set(prepared.operationId, prepared.lease.record)
    else if (prepared.lease?.kind === 'delete') this.#leases.delete(prepared.operationId)
  }

  readRecord(id: string): Promise<PersistedReceiveRecord | undefined> {
    return Promise.resolve(this.#records.get(id))
  }
  readLifecycle(operationId: string): Promise<PersistedReceiveRecord | undefined> {
    return Promise.resolve([...this.#records.values()].find(record =>
      record.operationId === operationId && record.kind === RECEIVE_RECORD_LIFECYCLE_STATE))
  }
  listRecords(operationId: string, kind?: ReceiveRecordKind): Promise<readonly PersistedReceiveRecord[]> {
    return Promise.resolve([...this.#records.values()].filter(record =>
      record.operationId === operationId && (kind === undefined || record.kind === kind)))
  }
  listManifestPages(operationId: string): Promise<readonly ManifestPageRecord[]> {
    return Promise.resolve([...this.#pages.values()].filter(page => page.operationId === operationId))
  }
  readHandle<T = unknown>(id: string): Promise<ReceiveOperationHandleRecord<T> | undefined> {
    return Promise.resolve(this.#handles.get(id) as ReceiveOperationHandleRecord<T> | undefined)
  }
  listHandles(operationId: string): Promise<readonly ReceiveOperationHandleRecord[]> {
    return Promise.resolve([...this.#handles.values()].filter(handle =>
      handle.operationId === operationId))
  }
  async listWorkspaceActivationCandidates() {
    return Promise.all([...this.#records.values()]
      .filter(record => record.kind === RECEIVE_RECORD_WORKSPACE_ACTIVATION)
      .map(decodeStoredWorkspaceActivationCandidate))
  }
  listInitialWorkspaceActivationOperationIds(): Promise<readonly string[]> {
    return Promise.resolve([...this.#records.values()]
      .filter(record => record.kind === RECEIVE_RECORD_LIFECYCLE_STATE)
      .map(decodeStoredReceiveLifecycleState)
      .filter(lifecycle => lifecycle.kind === 'intent-frozen')
      .map(lifecycle => lifecycle.operationId))
  }
  readLease(operationId: string): Promise<ReceiveOperationLeaseRecord | undefined> {
    return Promise.resolve(this.#leases.get(operationId))
  }
  close(): void {}
}

class NamespaceDirectoryHandle {
  readonly kind = 'directory' as const
  readonly name: string
  readonly #sameEntry: boolean
  readonly #token = crypto.randomUUID()
  readonly #entries = new Map<string, NamespaceDirectoryHandle>()
  readonly #files = new Map<string, NamespaceMarkerFileHandle>()
  #failAfterCreate = false
  #nextMarkerClose: (() => void) | undefined

  constructor(name: string, sameEntry: boolean) {
    this.name = name
    this.#sameEntry = sameEntry
  }

  get hasEntries(): boolean { return this.#entries.size !== 0 }

  failAfterNextDirectoryCreate(): void { this.#failAfterCreate = true }

  afterNextMarkerClose(callback: () => void): void { this.#nextMarkerClose = callback }

  replaceOnlyDirectory(): void {
    const name = this.#entries.keys().next().value as string | undefined
    if (name !== undefined) this.#entries.set(name, new NamespaceDirectoryHandle(name, true))
  }

  corruptOnlyDirectoryMarker(): void {
    const child = this.#entries.values().next().value as NamespaceDirectoryHandle | undefined
    child?.overwriteMarker('invalid')
  }

  overwriteMarker(content: string): void {
    const marker = this.#files.values().next().value as NamespaceMarkerFileHandle | undefined
    marker?.overwrite(content)
  }

  async getDirectoryHandle(
    name: string,
    options?: FileSystemGetDirectoryOptions,
  ): Promise<FileSystemDirectoryHandle> {
    const existing = this.#entries.get(name)
    if (existing !== undefined) return existing as unknown as FileSystemDirectoryHandle
    if (options?.create !== true) throw new DOMException('missing', 'NotFoundError')
    const created = new NamespaceDirectoryHandle(name, this.#sameEntry)
    if (this.#nextMarkerClose !== undefined) {
      created.afterNextMarkerClose(this.#nextMarkerClose)
      this.#nextMarkerClose = undefined
    }
    this.#entries.set(name, created)
    if (this.#failAfterCreate) {
      this.#failAfterCreate = false
      throw new Error('crash after directory creation')
    }
    return created as unknown as FileSystemDirectoryHandle
  }

  async removeEntry(name: string): Promise<void> {
    if (!this.#entries.delete(name)) throw new DOMException('missing', 'NotFoundError')
  }

  async getFileHandle(
    name: string,
    options?: FileSystemGetFileOptions,
  ): Promise<FileSystemFileHandle> {
    const existing = this.#files.get(name)
    if (existing !== undefined) return existing as unknown as FileSystemFileHandle
    if (options?.create !== true) throw new DOMException('missing', 'NotFoundError')
    const created = new NamespaceMarkerFileHandle(name, this.#nextMarkerClose)
    this.#nextMarkerClose = undefined
    this.#files.set(name, created)
    return created as unknown as FileSystemFileHandle
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    if (!this.#sameEntry) return false
    return (other as unknown as NamespaceDirectoryHandle).#token === this.#token
  }
}

class NamespaceMarkerFileHandle {
  readonly kind = 'file' as const
  readonly name: string
  #content = ''

  readonly #afterClose: (() => void) | undefined

  constructor(name: string, afterClose?: () => void) {
    this.name = name
    this.#afterClose = afterClose
  }

  overwrite(content: string): void { this.#content = content }

  getFile(): Promise<File> {
    return Promise.resolve({ text: async () => this.#content } as File)
  }

  createWritable(): Promise<FileSystemWritableFileStream> {
    return Promise.resolve({
      write: (value: FileSystemWriteChunkType) => { this.#content = String(value) },
      close: () => { this.#afterClose?.() },
      abort() {},
    } as unknown as FileSystemWritableFileStream)
  }
}

async function workspaceIntent() {
  const artifact = await createZipArchiveArtifact(createSyntheticSelectionResultRoot())
  const operationId = identity(70)
  const workspace = await createWorkspaceBinding({
    operationId,
    workspaceId: identity(71),
    artifact,
    repositoryRef: identity(72, 32),
  })
  return createReceiveIntent({
    selection: await createSelectionSpec({
      shareInstance: identity(73),
      syntheticRoot: identity(74),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    }),
    artifact,
    plan: await createWorkspaceThenPublishPlan(artifact, workspace),
  })
}

async function selectedRanking(intent: Awaited<ReturnType<typeof workspaceIntent>>) {
  return Object.freeze([(await deriveArtifactChoiceIdentity(intent.artifact, intent.plan)).id])
}

function storageFor(parent: NamespaceDirectoryHandle): StorageManager & {
  getDirectory(): Promise<FileSystemDirectoryHandle>
} {
  return {
    getDirectory: async () => parent as unknown as FileSystemDirectoryHandle,
  } as StorageManager & { getDirectory(): Promise<FileSystemDirectoryHandle> }
}

function immediateLocks(): LockManager {
  return {
    request: async (_name: string, callback: (lock: Lock | null) => unknown) => callback(null),
  } as LockManager
}
