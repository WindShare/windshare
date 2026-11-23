import type { ReceiveStore } from '../../src/output/store/database'
import type { ReceiveFileRecord, ReceiveTaskRecord } from '../../src/output/store/types'
import { openNewFSASession } from '../../src/output/backends/fsa'
import { initialReceiveState } from '../../src/transfer/state'

export class MemoryReceiveStore {
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

  async deleteFiles(ids: readonly string[]): Promise<void> {
    for (const id of ids) this.files.delete(id)
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

export class FakeFileHandle {
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

export class FakeDirectoryHandle {
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

  async queryPermission(): Promise<PermissionState> { return 'granted' }
  async requestPermission(): Promise<PermissionState> { return 'granted' }

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

export async function openFolderFixture() {
  const memory = new MemoryReceiveStore()
  const parent = new FakeDirectoryHandle('Downloads', memory.events)
  const store = memory as unknown as ReceiveStore
  const session = await openNewFSASession({
    task: {
      taskId: 'task', selectionKey: 'selection',
      selection: { shareInstance: 'share', syntheticRoot: 'root', defaultSelected: true, rules: [] },
      artifact: { kind: 'directory-tree', name: 'Folder' }, lifecycle: initialReceiveState('task'),
    },
    parentHandle: parent as unknown as FileSystemDirectoryHandle, store,
  })
  const root = await parent.getDirectoryHandle('Folder')
  const open = (name: string) => session.transferOutput.openFile({
    fileId: name, revision: 'revision', path: [name], sourcePath: [name], exactSize: 8n,
  }, new AbortController().signal)
  return { memory, store, session, root, parent, open }
}
