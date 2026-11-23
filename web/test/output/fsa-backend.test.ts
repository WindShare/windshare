import { describe, expect, it } from 'vitest'
import type { BlockRangeReader } from '../../src/content/broker'
import { FileGeometry, byteRange } from '../../src/content/geometry'
import type { FileRevisionDescriptor } from '../../src/content/records'
import type { RevisionReader } from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { discardFSATask, openNewFSASession, reopenFSASession } from '../../src/output/backends/fsa'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import type { ReceiveStore } from '../../src/output/store/database'
import type { ReceiveFileRecord, ReceiveTaskRecord } from '../../src/output/store/types'
import { transferSingleFile } from '../../src/transfer/file-transfer'
import { initialReceiveState } from '../../src/transfer/state'

let fakeClockMs = 1_700_000_000_000

function nextFakeTimestamp(): number {
  fakeClockMs += 1_000
  return fakeClockMs
}

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

class FakeFileHandle {
  readonly kind = 'file' as const
  readonly name: string
  bytes = new Uint8Array(0)
  readonly writableCalls: boolean[] = []
  lastModified = nextFakeTimestamp()
  readonly #events: string[]

  constructor(name: string, events: string[]) {
    this.name = name
    this.#events = events
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other === (this as unknown as FileSystemHandle)
  }

  async getFile(): Promise<File> {
    return new File([this.bytes], this.name, { lastModified: this.lastModified })
  }

  overwriteExternally(bytes: Uint8Array): void {
    this.bytes = new Uint8Array(bytes)
    this.lastModified = nextFakeTimestamp()
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
        this.lastModified = nextFakeTimestamp()
        this.#events.push(`writer.close:${this.name}`)
      },
      abort: async () => undefined,
    }
  }
}

class FakeDirectoryHandle {
  readonly kind = 'directory' as const
  readonly name: string
  readonly children = new Map<string, FakeDirectoryHandle | FakeFileHandle>()
  readonly #events: string[]
  readonly #rejectNames: Set<string>

  constructor(name: string, events: string[], rejectNames: readonly string[] = []) {
    this.name = name
    this.#events = events
    this.#rejectNames = new Set(rejectNames)
  }

  async queryPermission(): Promise<'granted'> { return 'granted' }
  async requestPermission(): Promise<'granted'> { return 'granted' }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other === (this as unknown as FileSystemHandle)
  }

  async *keys(): AsyncIterableIterator<string> {
    for (const key of this.children.keys()) yield key
  }

  async resolve(target: FileSystemHandle): Promise<string[] | null> {
    if (target === (this as unknown as FileSystemHandle)) return []
    for (const [name, child] of this.children) {
      if (child === (target as unknown)) return [name]
      if (child instanceof FakeDirectoryHandle) {
        const nested = await child.resolve(target)
        if (nested !== null) return [name, ...nested]
      }
    }
    return null
  }

  async getDirectoryHandle(name: string, options?: { create?: boolean }): Promise<FakeDirectoryHandle> {
    if (this.#rejectNames.has(name)) throw new TypeError(`Invalid name ${name}`)
    const existing = this.children.get(name)
    if (existing instanceof FakeDirectoryHandle) return existing
    if (existing instanceof FakeFileHandle) throw new DOMException('type mismatch', 'TypeMismatchError')
    if (!options?.create) throw new DOMException('missing', 'NotFoundError')
    const created = new FakeDirectoryHandle(name, this.#events, [...this.#rejectNames])
    this.children.set(name, created)
    return created
  }

  async getFileHandle(name: string, options?: { create?: boolean }): Promise<FakeFileHandle> {
    if (this.#rejectNames.has(name)) throw new TypeError(`Invalid name ${name}`)
    const existing = this.children.get(name)
    if (existing instanceof FakeFileHandle) return existing
    if (existing instanceof FakeDirectoryHandle) throw new DOMException('type mismatch', 'TypeMismatchError')
    if (!options?.create) throw new DOMException('missing', 'NotFoundError')
    const created = new FakeFileHandle(name, this.#events)
    this.children.set(name, created)
    return created
  }

  async removeEntry(name: string, options?: { recursive?: boolean }): Promise<void> {
    const existing = this.children.get(name)
    if (existing === undefined) throw new DOMException('missing', 'NotFoundError')
    if (existing instanceof FakeDirectoryHandle && existing.children.size > 0 && !options?.recursive) {
      throw new DOMException('Directory is not empty', 'InvalidModificationError')
    }
    this.children.delete(name)
  }
}

describe('FSA backend safety boundaries', () => {
  it('allocates collision names, flushes before IDB checkpoint, verifies physical files on resume, and keeps completed files on discard', async () => {
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

    // Reopen: physical file for f1 is intact (alreadyCompleted), f2 resumes from [0..4).
    const reopened = await reopenFSASession({
      task: pausedTask, store: store as unknown as ReceiveStore, allowPermissionPrompt: true,
    })
    const resumedA = await reopened.transferOutput.openFile({
      fileId: 'f1', path: ['CON.txt'], sourcePath: ['Photos', 'CON.txt'], revision: 'rev1', exactSize: 4n,
    }, new AbortController().signal)
    expect(resumedA.alreadyCompleted).toBe(true)

    // Simulate external truncation of f3 -> verification MUST reject stale IDB ranges!
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

    // User places their own file inside the result directory before discarding.
    const reserved = parent.children.get('Trip') as FakeDirectoryHandle
    const userFile = await reserved.getFileHandle('user-notes.txt', { create: true })
    userFile.overwriteExternally(new TextEncoder().encode('keep me'))

    await discardFSATask(paused, store as unknown as ReceiveStore, undefined, {
      backend: 'file_system_access',
      trace: { current: event => traces.push(event) },
    })

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
    expect(parent.children.has('Bundle')).toBe(false)
    expect(await store.getTask('fsa-safe-dir')).toBeUndefined()
  })

  it('refuses to touch a replaced root directory whose handle identity no longer matches', async () => {
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
    const paused = await session.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 0n, completedBytes: 0n, discoveredFiles: 0n, discoveredBytes: 0n,
      discoveryComplete: false, failedFiles: 0n, failedDirectories: 0n,
    })
    await session.close()

    // User deletes empty Docs and creates a brand-new Docs directory of their own.
    await parent.removeEntry('Docs')
    const userDocs = await parent.getDirectoryHandle('Docs', { create: true })
    const userFile = await userDocs.getFileHandle('tax.pdf', { create: true })
    userFile.overwriteExternally(new Uint8Array([1, 2, 3]))

    await discardFSATask(paused, store as unknown as ReceiveStore)
    expect(parent.children.get('Docs')).toBe(userDocs)
    expect(userDocs.children.has('tax.pdf')).toBe(true)
    expect(await store.getTask('fsa-replaced-root')).toBeUndefined()
  })

  it('verifies identity before deleting single-file FSA downloads and preserves replaced files', async () => {
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

    // Replace report.pdf in-place with user content (same handle object, mimicking Chromium path-based isSameEntry).
    const physical = parent.children.get('report.pdf') as FakeFileHandle
    physical.overwriteExternally(new TextEncoder().encode('real report'))

    await discardFSATask(paused, store as unknown as ReceiveStore)
    expect(parent.children.has('report.pdf')).toBe(true)
    expect(await store.getTask('fsa-single')).toBeUndefined()
  })

  it('cleans fresh 0-byte files and empty subdirectories on stop while preserving foreign files', async () => {
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
  })
})
