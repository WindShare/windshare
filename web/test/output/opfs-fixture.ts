import type { BrowserDownloadWindow } from '../../src/output/backends/browser-handoff'
import type { OPFSEnvironment } from '../../src/output/backends/opfs'
import type { NativeObjectIO } from '../../src/output/native-object/contracts'
import type { ReceiveFileRecord, ReceiveTaskRecord } from '../../src/output/store/types'

export class MemoryReceiveStore {
  readonly tasks = new Map<string, ReceiveTaskRecord>()
  readonly files = new Map<string, ReceiveFileRecord>()

  async putTask(record: ReceiveTaskRecord): Promise<void> {
    this.tasks.set(record.taskId, record)
  }

  async getTask(taskId: string): Promise<ReceiveTaskRecord | undefined> {
    return this.tasks.get(taskId)
  }

  async listTasks(): Promise<readonly ReceiveTaskRecord[]> {
    return [...this.tasks.values()]
  }

  async listTasksByShare(shareInstance: string): Promise<readonly ReceiveTaskRecord[]> {
    return [...this.tasks.values()]
      .filter(task => task.shareInstance === shareInstance)
      .sort((left, right) => right.updatedAt - left.updatedAt)
  }

  async putFile(record: ReceiveFileRecord): Promise<void> {
    this.files.set(record.id, record)
  }

  async putTaskAndFile(task: ReceiveTaskRecord, file: ReceiveFileRecord): Promise<void> {
    this.tasks.set(task.taskId, task)
    this.files.set(file.id, file)
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

class MemoryOPFSFileHandle {
  readonly kind = 'file' as const
  readonly name: string
  bytes = new Uint8Array(0)
  nativeOpen = false

  constructor(name: string) {
    this.name = name
  }

  async getFile(): Promise<File> {
    return new File([this.bytes], this.name)
  }

  createNativeIO(): NativeObjectIO {
    if (this.nativeOpen) throw new DOMException('File already has an exclusive writer', 'NoModificationAllowedError')
    this.nativeOpen = true
    return {
      writeAt: async (offset: bigint, data: Uint8Array) => {
        const start = Number(offset)
        const end = start + data.byteLength
        if (end > this.bytes.byteLength) {
          const grown = new Uint8Array(end)
          grown.set(this.bytes)
          this.bytes = grown
        }
        this.bytes.set(data, start)
      },
      truncate: async (length: bigint) => {
        const len = Number(length)
        const resized = new Uint8Array(len)
        resized.set(this.bytes.subarray(0, Math.min(len, this.bytes.byteLength)))
        this.bytes = resized
      },
      size: async () => BigInt(this.bytes.byteLength),
      flush: async () => undefined,
      close: async () => { this.nativeOpen = false },
    }
  }
}

class MemoryOPFSDirectoryHandle {
  readonly kind = 'directory' as const
  readonly name: string
  readonly dirs = new Map<string, MemoryOPFSDirectoryHandle>()
  readonly files = new Map<string, MemoryOPFSFileHandle>()

  constructor(name: string) {
    this.name = name
  }

  async getDirectoryHandle(name: string, options?: { create?: boolean }): Promise<MemoryOPFSDirectoryHandle> {
    const existing = this.dirs.get(name)
    if (existing !== undefined) return existing
    if (!options?.create) throw new DOMException('missing dir', 'NotFoundError')
    const created = new MemoryOPFSDirectoryHandle(name)
    this.dirs.set(name, created)
    return created
  }

  async getFileHandle(name: string, options?: { create?: boolean }): Promise<MemoryOPFSFileHandle> {
    const existing = this.files.get(name)
    if (existing !== undefined) return existing
    if (!options?.create) throw new DOMException('missing file', 'NotFoundError')
    const created = new MemoryOPFSFileHandle(name)
    this.files.set(name, created)
    return created
  }

  async removeEntry(name: string): Promise<void> {
    if (this.files.get(name)?.nativeOpen) throw new DOMException('File has an exclusive writer', 'NoModificationAllowedError')
    if (!this.files.delete(name) && !this.dirs.delete(name)) {
      throw new DOMException('missing entry', 'NotFoundError')
    }
  }
}

export function createMemoryOPFSEnv(): {
  readonly env: OPFSEnvironment
  readonly root: MemoryOPFSDirectoryHandle
} {
  const root = new MemoryOPFSDirectoryHandle('opfs-root')
  const env: OPFSEnvironment = Object.freeze({
    getRootDirectory: async () => root as unknown as FileSystemDirectoryHandle,
    openNativeObject: async (handle: FileSystemFileHandle) =>
      (handle as unknown as MemoryOPFSFileHandle).createNativeIO(),
  })
  return { env, root }
}

export function createMockDownloadWindow(): {
  readonly windowPort: BrowserDownloadWindow
  readonly downloads: Array<{ readonly blob: Blob; readonly fileName: string }>
} {
  const downloads: Array<{ readonly blob: Blob; readonly fileName: string }> = []
  const blobs = new Map<string, Blob>()
  let seq = 0
  const windowPort: BrowserDownloadWindow = {
    URL: {
      createObjectURL: (blob: Blob | MediaSource) => {
        const url = `blob:mock-${++seq}`
        if (blob instanceof Blob) blobs.set(url, blob)
        return url
      },
      revokeObjectURL: () => undefined,
    },
    document: {
      createElement: () => {
        const anchor = {
          download: '',
          href: '',
          hidden: false,
          click: () => {
            const blob = blobs.get(anchor.href)
            if (blob !== undefined) downloads.push({ blob, fileName: anchor.download })
          },
          remove: () => undefined,
        }
        return anchor as unknown as HTMLAnchorElement
      },
      body: { append: () => undefined } as unknown as HTMLElement,
    } as unknown as Pick<Document, 'createElement' | 'body'>,
    setTimeout: (() => 0) as unknown as typeof setTimeout,
    clearTimeout: () => undefined,
  }
  return { windowPort, downloads }
}
