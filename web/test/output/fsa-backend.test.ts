import { describe, expect, it } from 'vitest'
import { byteRange } from '../../src/content/geometry'
import { discardFSATask, openNewFSASession, reopenFSASession } from '../../src/output/backends/fsa'
import type { ReceiveStore } from '../../src/output/store/database'
import type { ReceiveFileRecord, ReceiveTaskRecord } from '../../src/output/store/types'
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

class FakeFileHandle {
  readonly kind = 'file' as const
  readonly name: string
  bytes = new Uint8Array(0)
  readonly writableCalls: boolean[] = []
  readonly #events: string[]

  constructor(name: string, events: string[]) {
    this.name = name
    this.#events = events
  }

  async getFile(): Promise<File> {
    return new File([this.bytes], this.name)
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

  async removeEntry(name: string): Promise<void> {
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

    const pausedTask = await session.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 1n, completedBytes: 4n, discoveredFiles: 2n, discoveredBytes: 12n,
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

    // Simulate external truncation of f2 -> verification MUST reject stale IDB ranges!
    const physicalB = reserved.children.get('big.bin') as FakeFileHandle
    physicalB.bytes = new Uint8Array(2)
    const resumedB = await reopened.transferOutput.openFile({
      fileId: 'f2', path: ['big.bin'], sourcePath: ['Photos', 'big.bin'], revision: 'rev2', exactSize: 8n,
    }, new AbortController().signal)
    expect(resumedB.completedRanges).toEqual([])
    await reopened.close()

    // Discard preserves completed file f1 and removes unfinished file f2.
    await discardFSATask(pausedTask, store as unknown as ReceiveStore)
    expect(reserved.children.has('big.bin')).toBe(false)
    expect(reserved.children.size).toBe(1)
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
})
