import { describe, expect, it } from 'vitest'
import type { CatalogClient } from '../../src/catalog/client'
import type { CommittedDirectory } from '../../src/catalog/page-store'
import type { CatalogEntry, CatalogPage, ShareDescriptor } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { BlockRangeReader } from '../../src/content/broker'
import { FileGeometry, byteRange } from '../../src/content/geometry'
import type { FileRevisionDescriptor } from '../../src/content/records'
import type { RevisionReader } from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { planArtifactOffers } from '../../src/output/backends/contract'
import { discardFSATask, openNewFSASession, reopenFSASession } from '../../src/output/backends/fsa'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import type { ReceiveStore } from '../../src/output/store/database'
import type { ReceiveFileRecord, ReceiveTaskRecord } from '../../src/output/store/types'
import { inspectSelectionShape } from '../../src/transfer/discovery'
import { transferSingleFile } from '../../src/transfer/file-transfer'
import { TransferJob } from '../../src/transfer/job'
import { artifactRootPath, selectionKey, selectionSpecFromPolicy } from '../../src/transfer/selection'
import { initialReceiveState } from '../../src/transfer/state'

class MemoryReceiveStore {
  readonly tasks = new Map<string, ReceiveTaskRecord>()
  readonly files = new Map<string, ReceiveFileRecord>()
  readonly events: string[] = []

  async putTask(record: ReceiveTaskRecord): Promise<void> {
    this.tasks.set(record.taskId, record)
    this.events.push(`putTask:${record.lifecycle.kind}`)
  }

  async getTask(taskId: string): Promise<ReceiveTaskRecord | undefined> {
    return this.tasks.get(taskId)
  }

  async listTasks(): Promise<readonly ReceiveTaskRecord[]> {
    return [...this.tasks.values()]
  }

  async putFile(record: ReceiveFileRecord): Promise<void> {
    this.files.set(record.id, record)
    this.events.push(`putFile:${record.fileId}:${record.status}`)
  }

  async getFile(taskId: string, fileId: string): Promise<ReceiveFileRecord | undefined> {
    return this.files.get(`${taskId}:${fileId}`)
  }

  async listTaskFiles(taskId: string): Promise<readonly ReceiveFileRecord[]> {
    return [...this.files.values()].filter(file => file.taskId === taskId)
  }

  async deleteTask(taskId: string): Promise<void> {
    this.tasks.delete(taskId)
    for (const [id, file] of this.files) {
      if (file.taskId === taskId) this.files.delete(id)
    }
  }
}

interface FakeFsStats {
  recursiveRemoveCalls: number
}

class FakeFileHandle {
  readonly kind = 'file' as const
  readonly name: string
  readonly path: string
  bytes = new Uint8Array(0)
  readonly writableCalls: boolean[] = []
  lastModified = Date.now()
  getFileCalls = 0
  readonly #events: string[]

  constructor(name: string, path: string, events: string[], lastModified = Date.now()) {
    this.name = name
    this.path = path
    this.#events = events
    this.lastModified = lastModified
  }

  // Matches Chromium's path-based FSA handle equality (not inode-based), ensuring
  // tests prove same-name swaps are stopped by fingerprints rather than isSameEntry.
  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other instanceof FakeFileHandle && other.path === this.path
  }

  async getFile(): Promise<File> {
    this.getFileCalls += 1
    return new File([this.bytes], this.name, { lastModified: this.lastModified })
  }

  overwriteExternally(bytes: Uint8Array, lastModified = Math.max(Date.now() + 10_000, this.lastModified + 1_000)): void {
    this.bytes = new Uint8Array(bytes)
    this.lastModified = lastModified
  }

  async createWritable(options?: { keepExistingData?: boolean }) {
    const keepExistingData = Boolean(options?.keepExistingData)
    this.writableCalls.push(keepExistingData)
    let buffer = keepExistingData ? this.bytes.slice() : new Uint8Array(0)
    return {
      write: async (chunk: { type: 'write'; position: number; data: Uint8Array }) => {
        const end = chunk.position + chunk.data.byteLength
        if (end > buffer.byteLength) {
          const grown = new Uint8Array(end)
          grown.set(buffer)
          buffer = grown
        }
        buffer.set(chunk.data, chunk.position)
      },
      close: async () => {
        this.bytes = buffer
        this.lastModified = Math.max(Date.now(), this.lastModified + 1)
        this.#events.push(`writer.close:${this.name}`)
      },
      abort: async () => undefined,
    }
  }
}

class FakeDirectoryHandle {
  readonly kind = 'directory' as const
  readonly name: string
  readonly path: string
  readonly children = new Map<string, FakeDirectoryHandle | FakeFileHandle>()
  readonly stats: FakeFsStats
  readonly #events: string[]
  readonly #rejectNames: Set<string>

  constructor(
    name: string,
    events: string[],
    rejectNames: readonly string[] = [],
    path = `/${name}`,
    stats: FakeFsStats = { recursiveRemoveCalls: 0 },
  ) {
    this.name = name
    this.path = path
    this.#events = events
    this.#rejectNames = new Set(rejectNames)
    this.stats = stats
  }

  async queryPermission(): Promise<'granted'> { return 'granted' }
  async requestPermission(): Promise<'granted'> { return 'granted' }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other instanceof FakeDirectoryHandle && other.path === this.path
  }

  async resolve(target: FileSystemHandle): Promise<string[] | null> {
    const candidate = target as FakeDirectoryHandle | FakeFileHandle
    if (typeof candidate.path !== 'string') return null
    if (candidate.path === this.path) return []
    const prefix = `${this.path}/`
    if (!candidate.path.startsWith(prefix)) return null
    return candidate.path.slice(prefix.length).split('/')
  }

  async getDirectoryHandle(name: string, options?: { create?: boolean }): Promise<FakeDirectoryHandle> {
    if (this.#rejectNames.has(name)) throw new TypeError(`Invalid name ${name}`)
    const existing = this.children.get(name)
    if (existing instanceof FakeDirectoryHandle) return existing
    if (existing instanceof FakeFileHandle) throw new DOMException('type mismatch', 'TypeMismatchError')
    if (!options?.create) throw new DOMException('missing', 'NotFoundError')
    const created = new FakeDirectoryHandle(
      name,
      this.#events,
      [...this.#rejectNames],
      `${this.path}/${name}`,
      this.stats,
    )
    this.children.set(name, created)
    return created
  }

  async getFileHandle(name: string, options?: { create?: boolean }): Promise<FakeFileHandle> {
    if (this.#rejectNames.has(name)) throw new TypeError(`Invalid name ${name}`)
    const existing = this.children.get(name)
    if (existing instanceof FakeFileHandle) return existing
    if (existing instanceof FakeDirectoryHandle) throw new DOMException('type mismatch', 'TypeMismatchError')
    if (!options?.create) throw new DOMException('missing', 'NotFoundError')
    const created = new FakeFileHandle(name, `${this.path}/${name}`, this.#events)
    this.children.set(name, created)
    return created
  }

  async removeEntry(name: string, options?: { recursive?: boolean }): Promise<void> {
    if (options?.recursive) this.stats.recursiveRemoveCalls += 1
    const existing = this.children.get(name)
    if (existing === undefined) throw new DOMException('missing', 'NotFoundError')
    if (existing instanceof FakeDirectoryHandle && existing.children.size > 0 && !options?.recursive) {
      throw new DOMException('Directory is not empty', 'InvalidModificationError')
    }
    this.children.delete(name)
  }

  putUserFile(name: string, bytes: Uint8Array, lastModified = Date.now() - 86_400_000): FakeFileHandle {
    const file = new FakeFileHandle(name, `${this.path}/${name}`, this.#events, lastModified)
    file.bytes = new Uint8Array(bytes)
    this.children.set(name, file)
    return file
  }
}

describe('FSA backend safety boundaries', () => {
  it('allocates collision names, flushes before IDB checkpoint, skips fingerprint I/O on commit, verifies physical files on resume, and keeps completed and modified files on discard', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events, ['CON.txt'])
    await parent.getDirectoryHandle('Photos', { create: true })

    const session = await openNewFSASession({
      task: {
        taskId: 'fsa-task-1',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        selectionKey: 'sel-1',
        artifact: { kind: 'directory-tree', name: 'Photos' },
        lifecycle: initialReceiveState('fsa-task-1'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })

    // Existing "Photos" collided -> reserved "Photos (1)".
    const reserved = parent.children.get('Photos (1)') as FakeDirectoryHandle
    expect(reserved).toBeDefined()

    // Write file with OS-invalid name -> falls back to deterministic safe name & flushes before IDB write.
    const fileA = await session.transferOutput.openFile({
      fileId: 'f1', path: ['CON.txt'], sourcePath: ['Photos', 'CON.txt'], revision: 'rev1', exactSize: 4n,
    }, new AbortController().signal)
    await fileA.writeAt(0n, new Uint8Array([1, 2, 3, 4]))
    await fileA.commit([byteRange(0n, 4n)])
    expect(session.transferOutput.safeRenamedSummary?.()?.count).toBe(1)

    // Completed files must not pay extra getFile() I/O or persist a discard fingerprint.
    const completedRow = await store.getFile('fsa-task-1', 'f1')
    expect(completedRow?.fsaDiskFingerprint).toBeUndefined()
    const physicalA = [...reserved.children.values()].find(entry => entry instanceof FakeFileHandle) as FakeFileHandle
    expect(physicalA.getFileCalls).toBe(0)

    const closeIdx = store.events.findIndex(e => e.startsWith('writer.close:'))
    const putIdx = store.events.findIndex(e => e === 'putFile:f1:completed')
    expect(closeIdx).toBeGreaterThanOrEqual(0)
    expect(putIdx).toBeGreaterThan(closeIdx)

    // Write partial second file and pause.
    const fileB = await session.transferOutput.openFile({
      fileId: 'f2', path: ['big.bin'], sourcePath: ['Photos', 'big.bin'], revision: 'rev2', exactSize: 8n,
    }, new AbortController().signal)
    await fileB.writeAt(0n, new Uint8Array([9, 8, 7, 6]))
    await fileB.pause([byteRange(0n, 4n)])
    expect((await store.getFile('fsa-task-1', 'f2'))?.fsaDiskFingerprint?.size).toBe(4)

    // Write partial third file to test external truncation rejection separately from unmodified discard.
    const fileC = await session.transferOutput.openFile({
      fileId: 'f3', path: ['truncated.bin'], sourcePath: ['Photos', 'truncated.bin'], revision: 'rev3', exactSize: 8n,
    }, new AbortController().signal)
    await fileC.writeAt(0n, new Uint8Array([5, 6, 7, 8]))
    await fileC.pause([byteRange(0n, 4n)])

    const pausedTask = await session.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 1n, completedBytes: 4n, discoveredFiles: 3n, discoveredBytes: 20n,
      discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await session.close()

    // Fresh FSA sessions have no prior history and omit pre-open completion lookup.
    expect(session.transferOutput.checkCompleted).toBeUndefined()

    // Reopen: physical file for f1 is intact (checkCompleted hit), f2 is incomplete, f3 rejects stale IDB ranges if truncated.
    const reopened = await reopenFSASession({
      task: pausedTask, store: store as unknown as ReceiveStore, allowPermissionPrompt: true,
    })
    expect(await reopened.transferOutput.checkCompleted?.('f1', new AbortController().signal)).toEqual({ size: 4n })
    expect(await reopened.transferOutput.checkCompleted?.('f2', new AbortController().signal)).toBeUndefined()

    const physicalC = reserved.children.get('truncated.bin') as FakeFileHandle
    physicalC.overwriteExternally(new Uint8Array(2))
    const resumedC = await reopened.transferOutput.openFile({
      fileId: 'f3', path: ['truncated.bin'], sourcePath: ['Photos', 'truncated.bin'], revision: 'rev3', exactSize: 8n,
    }, new AbortController().signal)
    expect(resumedC.completedRanges).toEqual([])
    await reopened.close()

    // Discard preserves completed file f1, removes verified unfinished f2, and preserves externally modified f3.
    await discardFSATask(pausedTask, store as unknown as ReceiveStore)
    expect(reserved.children.has('big.bin')).toBe(false)
    expect(reserved.children.has('truncated.bin')).toBe(true)
    expect(reserved.children.size).toBe(2)
    expect(parent.stats.recursiveRemoveCalls).toBe(0)
    expect(await store.getTask('fsa-task-1')).toBeUndefined()
  })

  it('preserves committed bytes when reopening a new file writer after checkpoint and disables periodic mid-file cuts', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    const session = await openNewFSASession({
      task: {
        taskId: 'fsa-task-2',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        selectionKey: 'sel-2',
        artifact: { kind: 'directory-tree', name: 'archive.bin', singleFileId: 'f1' },
        lifecycle: initialReceiveState('fsa-task-2'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })

    // FSA must not opt into periodic mid-file checkpoints that trigger O(N^2) prefix copies.
    expect(session.transferOutput.capabilities.periodicCheckpoint).toBeUndefined()

    const file = await session.transferOutput.openFile({
      fileId: 'f1', path: ['archive.bin'], sourcePath: ['archive.bin'], revision: 'rev1', exactSize: 8n,
    }, new AbortController().signal)
    await file.writeAt(0n, new Uint8Array([0x53, 0x54, 0x55, 0x56]))
    await file.checkpoint([byteRange(0n, 4n)])
    await file.writeAt(4n, new Uint8Array([0x57, 0x58, 0x59, 0x5a]))
    await file.commit([byteRange(0n, 8n)])

    const physical = parent.children.get('archive.bin') as FakeFileHandle
    expect(physical.writableCalls).toEqual([false, true])
    expect([...physical.bytes]).toEqual([0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a])
    await session.close()
  })

  it('streams files larger than 16 MiB through FSA without periodic writer reopens', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    const session = await openNewFSASession({
      task: {
        taskId: 'fsa-large-single-pass',
        selection: { shareInstance: 's2', syntheticRoot: 'r2', defaultSelected: true, rules: [] },
        selectionKey: 'sel-large',
        artifact: { kind: 'directory-tree', name: 'Videos' },
        lifecycle: initialReceiveState('fsa-large-single-pass'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })

    const mib = 1024 * 1024
    const chunkBytes = 6 * mib
    const totalSize = BigInt(chunkBytes * 3)
    const fileId = new Uint8Array(16).fill(42)
    const fileIdText = encodeBase64Url(fileId)
    const shareInstance = new Uint8Array(16).fill(7)
    const fileRevision = new Uint8Array(16).fill(9)

    const revisions: RevisionReader = {
      open: async () => {
        const descriptor: FileRevisionDescriptor = Object.freeze({
          shareInstance,
          shareInstanceId: encodeBase64Url(shareInstance),
          fileId,
          fileIdText,
          fileRevision,
          fileRevisionText: encodeBase64Url(fileRevision),
          exactSize: totalSize,
          geometry: new FileGeometry(totalSize, BigInt(chunkBytes)),
        })
        return Object.freeze({ descriptor, leaseId: new Uint8Array(16).fill(1), release: async () => undefined })
      },
    }
    const broker: BlockRangeReader = {
      readRange: async function* () {
        for (let part = 0; part < 3; part++) {
          const data = new Uint8Array(chunkBytes)
          data[0] = part + 11
          data[chunkBytes - 1] = part + 21
          yield Object.freeze({ offset: BigInt(part * chunkBytes), data })
        }
      },
    }

    const outcome = await transferSingleFile({
      revisions,
      broker,
      output: session.transferOutput,
      signal: new AbortController().signal,
      isPaused: () => false,
      isStopped: () => false,
    }, {
      kind: 'file',
      entry: Object.freeze({ kind: 'file', id: fileId, idText: fileIdText, name: 'clip.bin', expectedSize: totalSize }),
      relativePath: ['clip.bin'],
      sourcePath: ['Videos', 'clip.bin'],
    })
    await session.close()

    expect(outcome).toEqual({ kind: 'completed', size: totalSize })
    const reserved = parent.children.get('Videos') as FakeDirectoryHandle
    const physical = reserved.children.get('clip.bin') as FakeFileHandle
    expect(physical.writableCalls).toEqual([false])
    expect(physical.bytes.byteLength).toBe(chunkBytes * 3)
    expect(physical.bytes[0]).toBe(11)
    expect(physical.bytes[chunkBytes - 1]).toBe(21)
    expect(physical.bytes[chunkBytes]).toBe(12)
    expect(physical.bytes[chunkBytes * 2]).toBe(13)
    expect(physical.bytes[chunkBytes * 3 - 1]).toBe(23)
  })
})

describe('FSA discard safety', () => {
  it('preserves foreign user files when discarding a directory download with zero completed files', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    const traces: OutputTraceEvent[] = []
    const session = await openNewFSASession({
      task: {
        taskId: 'fsa-foreign',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        selectionKey: 'sel-foreign',
        artifact: { kind: 'directory-tree', name: 'Trip' },
        lifecycle: initialReceiveState('fsa-foreign'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })

    const file = await session.transferOutput.openFile({
      fileId: 'f1', path: ['sub', 'draft.bin'], sourcePath: ['Trip', 'sub', 'draft.bin'], revision: 'rev1', exactSize: 16n,
    }, new AbortController().signal)
    await file.writeAt(0n, new Uint8Array([1, 2, 3, 4]))
    await file.pause([byteRange(0n, 4n)])
    const paused = await session.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 0n, completedBytes: 0n, discoveredFiles: 1n, discoveredBytes: 16n,
      discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await session.close()

    const reserved = parent.children.get('Trip') as FakeDirectoryHandle
    reserved.putUserFile('user-notes.txt', new TextEncoder().encode('keep me'))

    await discardFSATask(paused, store as unknown as ReceiveStore, undefined, {
      backend: 'file_system_access',
      trace: { current: event => traces.push(event) },
    })

    expect(parent.stats.recursiveRemoveCalls).toBe(0)
    expect(parent.children.has('Trip')).toBe(true)
    expect(reserved.children.has('sub')).toBe(false)
    expect(reserved.children.has('user-notes.txt')).toBe(true)
    expect(await store.getTask('fsa-foreign')).toBeUndefined()
    expect(traces.at(-1)).toEqual({
      eventName: 'cleanup',
      payload: {
        backend: 'file_system_access',
        task_id: 'fsa-foreign',
        transition: 'completed',
        removed_files: 1,
        skipped_unverified_files: 0,
        removed_root: false,
      },
    })
  })

  it('cleans unfinished files under safe-renamed subdirectories and removes the result directory only when empty', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events, ['CON'])
    const session = await openNewFSASession({
      task: {
        taskId: 'fsa-safe-dir',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        selectionKey: 'sel-safe',
        artifact: { kind: 'directory-tree', name: 'Bundle' },
        lifecycle: initialReceiveState('fsa-safe-dir'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })

    const file = await session.transferOutput.openFile({
      fileId: 'f1', path: ['CON', 'part.bin'], sourcePath: ['Bundle', 'CON', 'part.bin'], revision: 'rev1', exactSize: 10n,
    }, new AbortController().signal)
    await file.writeAt(0n, new Uint8Array([7, 8]))
    await file.pause([byteRange(0n, 2n)])
    const paused = await session.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 0n, completedBytes: 0n, discoveredFiles: 1n, discoveredBytes: 10n,
      discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await session.close()

    await discardFSATask(paused, store as unknown as ReceiveStore)
    expect(parent.stats.recursiveRemoveCalls).toBe(0)
    expect(parent.children.has('Bundle')).toBe(false)
    expect(await store.getTask('fsa-safe-dir')).toBeUndefined()
  })

  it('preserves a same-name swapped user directory and replaced file even when path-based isSameEntry returns true', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    const session = await openNewFSASession({
      task: {
        taskId: 'fsa-replaced-root',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        selectionKey: 'sel-root',
        artifact: { kind: 'directory-tree', name: 'Docs' },
        lifecycle: initialReceiveState('fsa-replaced-root'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    const file = await session.transferOutput.openFile({
      fileId: 'f1', path: ['tax.pdf'], sourcePath: ['Docs', 'tax.pdf'], revision: 'rev1', exactSize: 16n,
    }, new AbortController().signal)
    await file.writeAt(0n, new Uint8Array([1, 2, 3, 4]))
    await file.pause([byteRange(0n, 4n)])
    const paused = await session.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 0n, completedBytes: 0n, discoveredFiles: 1n, discoveredBytes: 16n,
      discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await session.close()

    // User removes unfinished Docs/ and places their own Docs/ at the exact same path,
    // including a same-size, same-name file with a different mtime (path-based isSameEntry === true).
    await parent.removeEntry('Docs', { recursive: true })
    parent.stats.recursiveRemoveCalls = 0
    const userDocs = await parent.getDirectoryHandle('Docs', { create: true })
    userDocs.putUserFile('tax.pdf', new Uint8Array([9, 8, 7, 6]))
    userDocs.putUserFile('passport.pdf', new Uint8Array([42]))

    await discardFSATask(paused, store as unknown as ReceiveStore)
    expect(parent.stats.recursiveRemoveCalls).toBe(0)
    expect(parent.children.get('Docs')).toBe(userDocs)
    expect([...(userDocs.children.get('tax.pdf') as FakeFileHandle).bytes]).toEqual([9, 8, 7, 6])
    expect(userDocs.children.has('passport.pdf')).toBe(true)
    expect(await store.getTask('fsa-replaced-root')).toBeUndefined()
  })

  it('verifies fingerprint before deleting single-file downloads and refuses to delete unverified records', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    const session = await openNewFSASession({
      task: {
        taskId: 'fsa-single',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        selectionKey: 'sel-single',
        artifact: { kind: 'directory-tree', name: 'report.pdf', singleFileId: 'f1' },
        lifecycle: initialReceiveState('fsa-single'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })

    const file = await session.transferOutput.openFile({
      fileId: 'f1', path: ['report.pdf'], sourcePath: ['report.pdf'], revision: 'rev1', exactSize: 12n,
    }, new AbortController().signal)
    await file.writeAt(0n, new Uint8Array([1, 2, 3, 4]))
    await file.pause([byteRange(0n, 4n)])
    const paused = await session.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 0n, completedBytes: 0n, discoveredFiles: 1n, discoveredBytes: 12n,
      discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await session.close()

    const physical = parent.children.get('report.pdf') as FakeFileHandle
    physical.overwriteExternally(new TextEncoder().encode('real report'))

    await discardFSATask(paused, store as unknown as ReceiveStore)
    expect(parent.stats.recursiveRemoveCalls).toBe(0)
    expect(parent.children.has('report.pdf')).toBe(true)

    // Records missing a disk fingerprint must also be skipped rather than matched by size alone.
    const unverifiedSession = await openNewFSASession({
      task: {
        taskId: 'fsa-no-fp',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        selectionKey: 'sel-no-fp',
        artifact: { kind: 'directory-tree', name: 'draft.bin', singleFileId: 'f2' },
        lifecycle: initialReceiveState('fsa-no-fp'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    const draft = await unverifiedSession.transferOutput.openFile({
      fileId: 'f2', path: ['draft.bin'], sourcePath: ['draft.bin'], revision: 'rev2', exactSize: 8n,
    }, new AbortController().signal)
    await draft.writeAt(0n, new Uint8Array([1, 2, 3, 4]))
    await draft.pause([byteRange(0n, 4n)])
    const unverifiedPaused = await unverifiedSession.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 0n, completedBytes: 0n, discoveredFiles: 1n, discoveredBytes: 8n,
      discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await unverifiedSession.close()

    const savedRow = (await store.getFile('fsa-no-fp', 'f2'))!
    const rowWithoutFingerprint = Object.fromEntries(
      Object.entries(savedRow).filter(([key]) => key !== 'fsaDiskFingerprint'),
    ) as unknown as ReceiveFileRecord
    await store.putFile(rowWithoutFingerprint)

    await discardFSATask(unverifiedPaused, store as unknown as ReceiveStore)
    expect(parent.children.has('draft.bin')).toBe(true)
  })

  it('never deletes a regular file when the user replaced a reserved directory with a same-name file', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    const session = await openNewFSASession({
      task: {
        taskId: 'fsa-dir-to-file',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        selectionKey: 'sel-dir-to-file',
        artifact: { kind: 'directory-tree', name: 'Backup' },
        lifecycle: initialReceiveState('fsa-dir-to-file'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    const paused = await session.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 0n, completedBytes: 0n, discoveredFiles: 0n, discoveredBytes: 0n,
      discoveryComplete: false, failedFiles: 0n, failedDirectories: 0n,
    })
    await session.close()

    await parent.removeEntry('Backup')
    parent.putUserFile('Backup', new Uint8Array([1, 2, 3, 4]))

    await discardFSATask(paused, store as unknown as ReceiveStore)
    expect(parent.stats.recursiveRemoveCalls).toBe(0)
    expect(parent.children.get('Backup')).toBeInstanceOf(FakeFileHandle)
  })

  it('cleans fresh 0-byte placeholders and empty subdirectories on stop while preserving older pre-existing 0-byte user files', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    const session = await openNewFSASession({
      task: {
        taskId: 'fsa-stop',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        selectionKey: 'sel-stop',
        artifact: { kind: 'directory-tree', name: 'CancelMe' },
        lifecycle: initialReceiveState('fsa-stop'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })

    await session.transferOutput.ensureDirectory?.({ directoryId: 'd1', path: ['empty-sub'] }, new AbortController().signal)
    const file = await session.transferOutput.openFile({
      fileId: 'f1', path: ['nested', 'video.bin'], sourcePath: ['CancelMe', 'nested', 'video.bin'], revision: 'rev1', exactSize: 100n,
    }, new AbortController().signal)
    await file.writeAt(0n, new Uint8Array([1, 2, 3]))
    await file.abort(new DOMException('stopped', 'AbortError'))

    const stopped = await session.settle({
      status: 'stopped',
      completedFiles: 0n, completedBytes: 0n, discoveredFiles: 1n, discoveredBytes: 100n,
      discoveryComplete: false, failedFiles: 0n, failedDirectories: 0n,
    })
    await session.close()

    const reserved = parent.children.get('CancelMe') as FakeDirectoryHandle
    expect(reserved.children.size).toBe(0)

    await discardFSATask(stopped, store as unknown as ReceiveStore)
    expect(parent.children.has('CancelMe')).toBe(false)

    // Pre-existing older 0-byte user file in a reopened directory must survive abort().
    const reopenSession = await openNewFSASession({
      task: {
        taskId: 'fsa-reopen-abort',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        selectionKey: 'sel-reopen-abort',
        artifact: { kind: 'directory-tree', name: 'Existing' },
        lifecycle: initialReceiveState('fsa-reopen-abort'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    const pausedReopen = await reopenSession.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 0n, completedBytes: 0n, discoveredFiles: 0n, discoveredBytes: 0n,
      discoveryComplete: false, failedFiles: 0n, failedDirectories: 0n,
    })
    await reopenSession.close()

    const existingDir = parent.children.get('Existing') as FakeDirectoryHandle
    existingDir.putUserFile('.gitkeep', new Uint8Array(0), Date.now() - 86_400_000)

    const reopened = await reopenFSASession({
      task: pausedReopen, store: store as unknown as ReceiveStore, allowPermissionPrompt: true,
    })
    const abortedUserFile = await reopened.transferOutput.openFile({
      fileId: 'f2', path: ['.gitkeep'], sourcePath: ['Existing', '.gitkeep'], revision: 'rev2', exactSize: 10n,
    }, new AbortController().signal)
    await abortedUserFile.abort(new DOMException('stopped', 'AbortError'))
    await reopened.close()

    expect(existingDir.children.has('.gitkeep')).toBe(true)
    expect(parent.stats.recursiveRemoveCalls).toBe(0)
  })
})

describe('FSA result-root layout integration', () => {
  const fullCaps = {
    indexedDb: true,
    fsa: true,
    opfs: true,
    portable: true,
    availableStorageBytes: 1_000_000n,
  } as const

  function makeCatalogFixture(
    entriesByDir: ReadonlyMap<string, readonly CatalogEntry[]>,
    payloadsById: ReadonlyMap<string, Uint8Array<ArrayBuffer>>,
    rootId: Uint8Array<ArrayBuffer>,
  ) {
    const shareInstance = new Uint8Array(16).fill(1)
    const pages = new Map<string, { committed: CommittedDirectory; page: CatalogPage }>()
    for (const [dirIdText, entries] of entriesByDir) {
      const committed: CommittedDirectory = Object.freeze({
        directoryId: rootId,
        directoryIdText: dirIdText,
        generation: new Uint8Array(16).fill(3),
        generationText: encodeBase64Url(new Uint8Array(16).fill(3)),
        pageCount: 1,
        entryCount: entries.length,
        omittedCount: 0n,
        terminalCommitment: new Uint8Array(32),
      })
      const page: CatalogPage = Object.freeze({
        shareInstance,
        directoryId: rootId,
        directoryIdText: dirIdText,
        generation: committed.generation,
        generationText: committed.generationText,
        pageIndex: 0,
        terminal: true,
        previousCommitment: new Uint8Array(32),
        entries: Object.freeze([...entries]),
        omittedCount: 0n,
        objectCommitment: new Uint8Array(32),
        encodedBytes: 64,
      })
      pages.set(dirIdText, { committed, page })
    }
    const descriptor: ShareDescriptor = Object.freeze({
      shareInstance,
      shareInstanceId: encodeBase64Url(shareInstance),
      syntheticRoot: rootId,
      syntheticRootId: encodeBase64Url(rootId),
    }) as unknown as ShareDescriptor
    const catalog: CatalogClient = Object.freeze({
      loadDirectory: async (directoryId: Uint8Array) => pages.get(encodeBase64Url(directoryId))!.committed,
      page: async (committed: CommittedDirectory) => pages.get(committed.directoryIdText)!.page,
    }) as unknown as CatalogClient
    const revisions: RevisionReader = {
      open: async (fileId: Uint8Array) => {
        const key = encodeBase64Url(fileId)
        const bytes = payloadsById.get(key)!
        const rev = new Uint8Array(16).fill(70)
        return Object.freeze({
          descriptor: Object.freeze({
            shareInstance,
            shareInstanceId: descriptor.shareInstanceId,
            fileId,
            fileIdText: key,
            fileRevision: rev,
            fileRevisionText: encodeBase64Url(rev),
            exactSize: BigInt(bytes.byteLength),
            geometry: new FileGeometry(BigInt(bytes.byteLength), 64n),
          }) as FileRevisionDescriptor,
          leaseId: new Uint8Array(16).fill(99),
          release: async () => undefined,
        })
      },
    }
    const broker: BlockRangeReader = {
      readRange: async function* (revDesc, _leaseId, span) {
        const bytes = payloadsById.get(revDesc.fileIdText)!
        yield Object.freeze({ offset: span.start, data: bytes.subarray(Number(span.start), Number(span.end)) })
      },
    }
    return { descriptor, catalog, revisions, broker }
  }

  function listTreePaths(dir: FakeDirectoryHandle, prefix = ''): string[] {
    const out: string[] = []
    for (const [name, child] of [...dir.children.entries()].sort(([a], [b]) => a.localeCompare(b))) {
      const next = prefix === '' ? name : `${prefix}/${name}`
      if (child instanceof FakeDirectoryHandle) {
        out.push(`${next}/`)
        out.push(...listTreePaths(child, next))
      } else {
        out.push(next)
      }
    }
    return out
  }

  it('writes single-folder, subfolder, multi-file, and multi-root downloads without duplicate top-level folders', async () => {
    const rootId = new Uint8Array(16).fill(2)
    const rootText = encodeBase64Url(rootId)
    const photosId = new Uint8Array(16).fill(10)
    const tripId = new Uint8Array(16).fill(11)
    const docsId = new Uint8Array(16).fill(12)
    const aId = new Uint8Array(16).fill(20)
    const bId = new Uint8Array(16).fill(21)
    const cId = new Uint8Array(16).fill(22)

    const photosEntry: CatalogEntry = Object.freeze({ kind: 'directory', id: photosId, idText: encodeBase64Url(photosId), name: 'Photos' })
    const tripEntry: CatalogEntry = Object.freeze({ kind: 'directory', id: tripId, idText: encodeBase64Url(tripId), name: 'Trip' })
    const docsEntry: CatalogEntry = Object.freeze({ kind: 'directory', id: docsId, idText: encodeBase64Url(docsId), name: 'Docs' })
    const aBytes = new TextEncoder().encode('alpha')
    const bBytes = new TextEncoder().encode('beta')
    const cBytes = new TextEncoder().encode('gamma')
    const fileA: CatalogEntry = Object.freeze({ kind: 'file', id: aId, idText: encodeBase64Url(aId), name: 'a.jpg', expectedSize: BigInt(aBytes.byteLength) })
    const fileB: CatalogEntry = Object.freeze({ kind: 'file', id: bId, idText: encodeBase64Url(bId), name: 'b.jpg', expectedSize: BigInt(bBytes.byteLength) })
    const fileC: CatalogEntry = Object.freeze({ kind: 'file', id: cId, idText: encodeBase64Url(cId), name: 'c.pdf', expectedSize: BigInt(cBytes.byteLength) })
    const payloads = new Map([[fileA.idText, aBytes], [fileB.idText, bBytes], [fileC.idText, cBytes]])

    const runFsaDownload = async (
      taskId: string,
      entriesByDir: ReadonlyMap<string, readonly CatalogEntry[]>,
      policy: SelectionPolicy,
    ) => {
      const fixture = makeCatalogFixture(entriesByDir, payloads, rootId)
      const selection = policy.snapshot()
      const spec = selectionSpecFromPolicy(fixture.descriptor.shareInstanceId, fixture.descriptor.syntheticRootId, selection)
      const key = selectionKey(spec)
      const shape = await inspectSelectionShape(fixture.descriptor, fixture.catalog, selection, new AbortController().signal)
      const offers = planArtifactOffers({ epoch: 1n, selectionKey: key, shape, capabilities: fullCaps })
      if (offers.kind !== 'ready') throw new Error('expected ready offers')
      const store = new MemoryReceiveStore()
      const parent = new FakeDirectoryHandle('Downloads', store.events)
      const session = await openNewFSASession({
        task: {
          taskId,
          selection: spec,
          selectionKey: key,
          artifact: offers.primary.artifact,
          lifecycle: initialReceiveState(taskId),
        },
        parentHandle: parent as unknown as FileSystemDirectoryHandle,
        store: store as unknown as ReceiveStore,
      })
      const result = await new TransferJob({
        descriptor: fixture.descriptor,
        catalog: fixture.catalog,
        selection,
        revisions: fixture.revisions,
        broker: fixture.broker,
        output: session.transferOutput,
        rootPath: artifactRootPath(offers.primary.artifact),
      }).run()
      expect(result.status).toBe('completed')
      await session.settle(result)
      await session.close()
      return listTreePaths(parent)
    }

    // 1. Whole single folder Photos/ -> Downloads/Photos/a.jpg and Downloads/Photos/Trip/b.jpg (no Photos/Photos).
    const singleFolderTree = new Map<string, readonly CatalogEntry[]>([
      [rootText, [photosEntry]],
      [photosEntry.idText, [fileA, tripEntry]],
      [tripEntry.idText, [fileB]],
    ])
    expect(await runFsaDownload('t-single-dir', singleFolderTree, new SelectionPolicy(true))).toEqual([
      'Photos/',
      'Photos/a.jpg',
      'Photos/Trip/',
      'Photos/Trip/b.jpg',
    ])

    // 2. Subfolder Photos/Trip/ only -> Downloads/Trip/b.jpg (no Trip/Photos/Trip).
    const tripOnly = new SelectionPolicy(false)
    tripOnly.set(tripEntry, [rootText, photosEntry.idText], true)
    expect(await runFsaDownload('t-subfolder', singleFolderTree, tripOnly)).toEqual([
      'Trip/',
      'Trip/b.jpg',
    ])

    // 3. Multiple root files a.jpg + b.jpg -> Downloads/windshare-download/{a.jpg, b.jpg} (no a.jpg/ folder).
    const multiFileTree = new Map<string, readonly CatalogEntry[]>([
      [rootText, [fileA, fileB]],
    ])
    expect(await runFsaDownload('t-multi-file', multiFileTree, new SelectionPolicy(true))).toEqual([
      'windshare-download/',
      'windshare-download/a.jpg',
      'windshare-download/b.jpg',
    ])

    // 4. Multiple root directories Photos/ + Docs/ -> Downloads/windshare-download/{Docs/c.pdf, Photos/a.jpg}.
    const multiDirTree = new Map<string, readonly CatalogEntry[]>([
      [rootText, [photosEntry, docsEntry]],
      [photosEntry.idText, [fileA]],
      [docsEntry.idText, [fileC]],
    ])
    expect(await runFsaDownload('t-multi-dir', multiDirTree, new SelectionPolicy(true))).toEqual([
      'windshare-download/',
      'windshare-download/Docs/',
      'windshare-download/Docs/c.pdf',
      'windshare-download/Photos/',
      'windshare-download/Photos/a.jpg',
    ])
  })
})

describe('FSA resume completion skip', () => {
  it('skips remote revision opens for intact completed files on FSA resume and redownloads truncated ones', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    const shareInstance = new Uint8Array(16).fill(3)
    const rootId = new Uint8Array(16).fill(4)
    const idA = new Uint8Array(16).fill(11)
    const idB = new Uint8Array(16).fill(12)
    const idC = new Uint8Array(16).fill(13)
    const idAText = encodeBase64Url(idA)
    const idBText = encodeBase64Url(idB)
    const idCText = encodeBase64Url(idC)
    const payloadA = new Uint8Array([1, 2, 3, 4])
    const payloadB = new Uint8Array([5, 6, 7, 8, 9])
    const payloadC = new Uint8Array([10, 11, 12, 13, 14, 15])

    const initial = await openNewFSASession({
      task: {
        taskId: 'fsa-resume-skip',
        selection: { shareInstance: encodeBase64Url(shareInstance), syntheticRoot: encodeBase64Url(rootId), defaultSelected: true, rules: [] },
        selectionKey: 'sel-resume-skip',
        artifact: { kind: 'directory-tree', name: 'Docs' },
        lifecycle: initialReceiveState('fsa-resume-skip'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    for (const [fileId, name, data] of [[idAText, 'a.bin', payloadA], [idBText, 'b.bin', payloadB]] as const) {
      const writer = await initial.transferOutput.openFile({
        fileId, path: [name], sourcePath: ['Docs', name], revision: `rev-${name}`, exactSize: BigInt(data.byteLength),
      }, new AbortController().signal)
      await writer.writeAt(0n, data)
      await writer.commit([byteRange(0n, BigInt(data.byteLength))])
    }
    const pausedTask = await initial.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 2n, completedBytes: 9n, discoveredFiles: 3n, discoveredBytes: 15n,
      discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await initial.close()

    const entries: CatalogEntry[] = [
      Object.freeze({ kind: 'file', id: idA, idText: idAText, name: 'a.bin', expectedSize: BigInt(payloadA.byteLength) }),
      Object.freeze({ kind: 'file', id: idB, idText: idBText, name: 'b.bin', expectedSize: BigInt(payloadB.byteLength) }),
      Object.freeze({ kind: 'file', id: idC, idText: idCText, name: 'c.bin', expectedSize: BigInt(payloadC.byteLength) }),
    ]
    const committed: CommittedDirectory = Object.freeze({
      directoryId: rootId, directoryIdText: encodeBase64Url(rootId),
      generation: new Uint8Array(16).fill(5), generationText: encodeBase64Url(new Uint8Array(16).fill(5)),
      pageCount: 1, entryCount: 3, omittedCount: 0n, terminalCommitment: new Uint8Array(32),
    })
    const page: CatalogPage = Object.freeze({
      shareInstance, directoryId: rootId, directoryIdText: committed.directoryIdText,
      generation: committed.generation, generationText: committed.generationText,
      pageIndex: 0, terminal: true, previousCommitment: new Uint8Array(32),
      entries: Object.freeze(entries), omittedCount: 0n, objectCommitment: new Uint8Array(32), encodedBytes: 64,
    })
    const descriptor = Object.freeze({
      shareInstance, shareInstanceId: encodeBase64Url(shareInstance),
      syntheticRoot: rootId, syntheticRootId: committed.directoryIdText,
    }) as unknown as ShareDescriptor
    const catalog = Object.freeze({ loadDirectory: async () => committed, page: async () => page }) as unknown as CatalogClient

    const payloads = new Map<string, Uint8Array<ArrayBuffer>>([
      [idBText, payloadB],
      [idCText, payloadC],
    ])
    const openedRevisions: string[] = []
    const revisions: RevisionReader = {
      open: async fileId => {
        const key = encodeBase64Url(fileId)
        openedRevisions.push(key)
        const data = payloads.get(key)
        if (data === undefined) throw new Error('source file deleted on sender')
        const rev = new Uint8Array(16).fill(77)
        const desc: FileRevisionDescriptor = Object.freeze({
          shareInstance, shareInstanceId: descriptor.shareInstanceId,
          fileId: new Uint8Array(fileId), fileIdText: key,
          fileRevision: rev, fileRevisionText: encodeBase64Url(rev),
          exactSize: BigInt(data.byteLength), geometry: new FileGeometry(BigInt(data.byteLength), 16n),
        })
        return Object.freeze({ descriptor: desc, leaseId: new Uint8Array(16).fill(9), release: async () => undefined })
      },
    }
    const broker: BlockRangeReader = {
      readRange: async function* (desc, _leaseId, span) {
        const data = payloads.get(desc.fileIdText)!
        yield Object.freeze({ offset: span.start, data: data.subarray(Number(span.start), Number(span.end)) })
      },
    }

    // Resume 1: a.bin is missing on sender, b.bin is intact locally, only c.bin needs remote open.
    const reopened = await reopenFSASession({
      task: pausedTask, store: store as unknown as ReceiveStore, allowPermissionPrompt: true,
    })
    const resumedResult = await new TransferJob({
      descriptor, catalog, selection: new SelectionPolicy(true).snapshot(),
      revisions, broker, output: reopened.transferOutput,
    }).run()
    await reopened.close()

    expect(resumedResult.status).toBe('completed')
    expect(resumedResult.completedFiles).toBe(3n)
    expect(openedRevisions).toEqual([idCText])

    // Resume 2: externally truncating completed b.bin forces checkCompleted to miss and redownload b.bin.
    const docsDir = parent.children.get('Docs') as FakeDirectoryHandle
    const physicalB = docsDir.children.get('b.bin') as FakeFileHandle
    physicalB.overwriteExternally(new Uint8Array(2))
    openedRevisions.length = 0

    const reopenedAfterEdit = await reopenFSASession({
      task: pausedTask, store: store as unknown as ReceiveStore, allowPermissionPrompt: true,
    })
    const repairedResult = await new TransferJob({
      descriptor, catalog, selection: new SelectionPolicy(true).snapshot(),
      revisions, broker, output: reopenedAfterEdit.transferOutput,
    }).run()
    await reopenedAfterEdit.close()

    expect(repairedResult.status).toBe('completed')
    expect(openedRevisions).toEqual([idBText])
    expect([...physicalB.bytes]).toEqual([...payloadB])
  })
})
