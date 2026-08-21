export interface MemoryDirectoryLookup {
  readonly kind: 'file' | 'directory'
  readonly name: string
  readonly create: boolean
}

export class MemoryDirectory {
  readonly kind = 'directory' as const
  readonly name: string
  readonly #token = crypto.randomUUID()
  readonly #entries = new Map<string, MemoryDirectory | MemoryFile>()
  queryPermissionState: PermissionState = 'granted'
  requestPermissionState: PermissionState = 'granted'
  onEntryLookup: ((lookup: MemoryDirectoryLookup) => void | Promise<void>) | undefined
  onFileCreated: (() => void) | undefined
  onRemoveEntry: ((name: string) => Promise<void>) | undefined

  constructor(name: string) {
    this.name = name
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return (other as MemoryDirectory).#token === this.#token
  }

  async queryPermission(): Promise<PermissionState> {
    return this.queryPermissionState
  }

  async requestPermission(): Promise<PermissionState> {
    return this.requestPermissionState
  }

  async getDirectoryHandle(
    name: string,
    options?: FileSystemGetDirectoryOptions,
  ): Promise<FileSystemDirectoryHandle> {
    await this.onEntryLookup?.(Object.freeze({
      kind: 'directory',
      name,
      create: options?.create === true,
    }))
    const existing = this.#entries.get(name)
    if (existing instanceof MemoryDirectory) return existing as unknown as FileSystemDirectoryHandle
    if (existing !== undefined) throw domError('TypeMismatchError')
    if (options?.create !== true) throw domError('NotFoundError')
    const created = new MemoryDirectory(name)
    this.#entries.set(name, created)
    return created as unknown as FileSystemDirectoryHandle
  }

  async getFileHandle(
    name: string,
    options?: FileSystemGetFileOptions,
  ): Promise<FileSystemFileHandle> {
    await this.onEntryLookup?.(Object.freeze({
      kind: 'file',
      name,
      create: options?.create === true,
    }))
    const existing = this.#entries.get(name)
    if (existing instanceof MemoryFile) return existing as unknown as FileSystemFileHandle
    if (existing !== undefined) throw domError('TypeMismatchError')
    if (options?.create !== true) throw domError('NotFoundError')
    const created = new MemoryFile(name)
    this.#entries.set(name, created)
    this.onFileCreated?.()
    return created as unknown as FileSystemFileHandle
  }

  async removeEntry(name: string): Promise<void> {
    await this.onRemoveEntry?.(name)
    if (!this.#entries.delete(name)) throw domError('NotFoundError')
  }

  async *entries(): AsyncIterableIterator<[string, FileSystemHandle]> {
    for (const [name, handle] of [...this.#entries]) {
      yield [name, handle as unknown as FileSystemHandle]
    }
  }

  directoryNames(): string[] {
    return [...this.#entries.entries()]
      .filter((entry): entry is [string, MemoryDirectory] => entry[1] instanceof MemoryDirectory)
      .map(([name]) => name)
      .sort()
  }

  fileNames(): string[] {
    return [...this.#entries.entries()]
      .filter((entry): entry is [string, MemoryFile] => entry[1] instanceof MemoryFile)
      .map(([name]) => name)
      .sort()
  }

  entryNames(): string[] {
    return [...this.#entries.keys()].sort()
  }

  async fileBytes(name: string): Promise<Uint8Array> {
    const file = this.#entries.get(name)
    if (!(file instanceof MemoryFile)) throw new Error('memory file is missing')
    return file.bytes()
  }

  replaceFile(name: string, bytes: Uint8Array): MemoryFile {
    const file = new MemoryFile(name, Uint8Array.from(bytes))
    this.#entries.set(name, file)
    return file
  }
}

export class MemoryFile {
  readonly kind = 'file' as const
  readonly name: string
  readonly #token = crypto.randomUUID()
  #bytes: Uint8Array
  #writableFailure: unknown

  constructor(name: string, bytes = new Uint8Array()) {
    this.name = name
    this.#bytes = bytes.slice()
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other instanceof MemoryFile && other.#token === this.#token
  }

  async getFile(): Promise<File> {
    const copy = new Uint8Array(this.#bytes.byteLength)
    copy.set(this.#bytes)
    return new Blob([copy.buffer]) as File
  }

  async createWritable(
    options?: FileSystemCreateWritableOptions,
  ): Promise<FileSystemWritableFileStream> {
    if (this.#writableFailure !== undefined) throw this.#writableFailure
    if (options?.keepExistingData !== true) this.#bytes = new Uint8Array()
    let position = 0
    return {
      write: async (data: FileSystemWriteChunkType) => {
        if (data instanceof Uint8Array) {
          this.#writeAt(position, data)
          position += data.byteLength
          return
        }
        if (typeof data !== 'object' || data === null || !('type' in data)) {
          throw new TypeError('memory writer requires bytes or a write command')
        }
        if (data.type === 'truncate') {
          const size = Number((data as FileSystemWriteChunkType & { size: number }).size)
          this.#truncate(size)
          return
        }
        if (data.type === 'seek') {
          position = Number((data as FileSystemWriteChunkType & { position: number }).position)
          return
        }
        if (data.type !== 'write') {
          throw new TypeError('memory writer command is unsupported')
        }
        const command = data as WriteParams
        if (!(command.data instanceof Uint8Array)) {
          throw new TypeError('memory writer accepts Uint8Array writes')
        }
        const writePosition = Number(command.position ?? position)
        const source = command.data
        this.#writeAt(writePosition, source)
        position = writePosition + source.byteLength
      },
      seek: async (nextPosition: number) => { position = nextPosition },
      truncate: async (size: number) => { this.#truncate(size) },
      close: async () => {},
      abort: async () => {},
    } as unknown as FileSystemWritableFileStream
  }

  setWritableFailure(error: unknown): void {
    this.#writableFailure = error
  }

  async bytes(): Promise<Uint8Array> {
    return this.#bytes.slice()
  }

  #writeAt(position: number, source: Uint8Array): void {
    const next = new Uint8Array(Math.max(this.#bytes.byteLength, position + source.byteLength))
    next.set(this.#bytes)
    next.set(source, position)
    this.#bytes = next
  }

  #truncate(size: number): void {
    const next = new Uint8Array(size)
    next.set(this.#bytes.subarray(0, size))
    this.#bytes = next
  }
}

function domError(name: string): DOMException {
  return new DOMException(name, name)
}
