import { describe, expect, it } from 'vitest'

import {
  DownloadError,
  FSA_CLEANUP_ATTEMPTS,
  createFileSystemDownloadSink,
  type FsaMetadataWriter,
} from '../../src/download'
import {
  chunk,
  directory,
  file,
  fixtureContext,
  range,
} from './fixtures'

function notFound(): DOMException {
  return new DOMException('Entry not found', 'NotFoundError')
}

function deferred(): { readonly promise: Promise<void>; readonly resolve: () => void } {
  let resolve!: () => void
  const promise = new Promise<void>((settle) => {
    resolve = settle
  })
  return { promise, resolve }
}

interface FakeFsaStats {
  openWritables: number
  maxOpenWritables: number
  beforeWrite?: () => Promise<void>
  writeError?: unknown
  abortError?: unknown
  removeError?: unknown
  removeFailuresRemaining?: number
}

class FakeWritable {
  readonly #file: FakeFile
  readonly #stats: FakeFsaStats
  #working = new Uint8Array()
  #settled = false

  constructor(file: FakeFile, stats: FakeFsaStats) {
    this.#file = file
    this.#stats = stats
  }

  async write(chunk: unknown): Promise<void> {
    if (this.#settled || typeof chunk !== 'object' || chunk === null) {
      throw new DOMException('Writable is closed', 'InvalidStateError')
    }
    const candidate = chunk as {
      readonly type?: unknown
      readonly position?: unknown
      readonly data?: unknown
    }
    if (
      candidate.type !== 'write' ||
      !Number.isSafeInteger(candidate.position) ||
      (candidate.position as number) < 0 ||
      !(candidate.data instanceof Uint8Array)
    ) {
      throw new TypeError('Invalid fake FSA write')
    }
    await this.#stats.beforeWrite?.()
    if (this.#stats.writeError !== undefined) {
      throw this.#stats.writeError
    }
    const position = candidate.position as number
    const end = position + candidate.data.byteLength
    if (end > this.#working.byteLength) {
      const expanded = new Uint8Array(end)
      expanded.set(this.#working)
      this.#working = expanded
    }
    this.#working.set(candidate.data, position)
  }

  async truncate(size: number): Promise<void> {
    if (this.#settled || !Number.isSafeInteger(size) || size < 0) {
      throw new TypeError('Invalid fake FSA truncate')
    }
    const resized = new Uint8Array(size)
    resized.set(this.#working.subarray(0, size))
    this.#working = resized
  }

  async close(): Promise<void> {
    this.#settle()
    this.#file.bytes = this.#working.slice()
  }

  async abort(): Promise<void> {
    this.#settle()
    if (this.#stats.abortError !== undefined) {
      throw this.#stats.abortError
    }
  }

  asNative(): FileSystemWritableFileStream {
    return this as unknown as FileSystemWritableFileStream
  }

  #settle(): void {
    if (!this.#settled) {
      this.#settled = true
      this.#stats.openWritables -= 1
    }
  }
}

class FakeFile {
  readonly kind = 'file' as const
  readonly name: string
  readonly #stats: FakeFsaStats
  bytes: Uint8Array

  constructor(
    name: string,
    bytes: Uint8Array = new Uint8Array(),
    stats: FakeFsaStats = { openWritables: 0, maxOpenWritables: 0 },
  ) {
    this.name = name
    this.bytes = bytes.slice()
    this.#stats = stats
  }

  async createWritable(): Promise<FileSystemWritableFileStream> {
    this.#stats.openWritables += 1
    this.#stats.maxOpenWritables = Math.max(
      this.#stats.maxOpenWritables,
      this.#stats.openWritables,
    )
    return new FakeWritable(this, this.#stats).asNative()
  }

  asNative(): FileSystemFileHandle {
    return this as unknown as FileSystemFileHandle
  }
}

class FakeDirectory {
  readonly kind = 'directory' as const
  readonly name: string
  readonly calls: string[]
  readonly stats: FakeFsaStats

  readonly #directories = new Map<string, FakeDirectory>()
  readonly #files = new Map<string, FakeFile>()

  constructor(
    name: string,
    calls: string[] = [],
    stats: FakeFsaStats = { openWritables: 0, maxOpenWritables: 0 },
  ) {
    this.name = name
    this.calls = calls
    this.stats = stats
  }

  async getDirectoryHandle(
    name: string,
    options?: { readonly create?: boolean },
  ): Promise<FileSystemDirectoryHandle> {
    this.calls.push(`directory:${name}:${options?.create === true ? 'create' : 'open'}`)
    let directory = this.#directories.get(name)
    if (directory === undefined && options?.create === true) {
      directory = new FakeDirectory(name, this.calls, this.stats)
      this.#directories.set(name, directory)
    }
    if (directory === undefined) {
      throw notFound()
    }
    return directory.asNative()
  }

  async getFileHandle(
    name: string,
    options?: { readonly create?: boolean },
  ): Promise<FileSystemFileHandle> {
    this.calls.push(`file:${name}:${options?.create === true ? 'create' : 'open'}`)
    let file = this.#files.get(name)
    if (file === undefined && options?.create === true) {
      file = new FakeFile(name, new Uint8Array(), this.stats)
      this.#files.set(name, file)
    }
    if (file === undefined) {
      throw notFound()
    }
    return file.asNative()
  }

  async removeEntry(name: string): Promise<void> {
    this.calls.push(`remove:${name}`)
    if ((this.stats.removeFailuresRemaining ?? 0) > 0) {
      this.stats.removeFailuresRemaining = (this.stats.removeFailuresRemaining ?? 0) - 1
      throw this.stats.removeError ?? new DOMException('Removal failed', 'UnknownError')
    }
    if (this.#files.delete(name)) {
      return
    }
    const directory = this.#directories.get(name)
    if (directory === undefined) {
      throw notFound()
    }
    if (directory.#files.size !== 0 || directory.#directories.size !== 0) {
      throw new DOMException('Directory is not empty', 'InvalidModificationError')
    }
    this.#directories.delete(name)
  }

  seedFile(path: string, bytes: Uint8Array): void {
    const segments = path.split('/')
    const name = segments.pop()
    if (name === undefined) {
      throw new TypeError('Seed path has no file name')
    }
    const directory = this.#ensureDirectory(segments)
    directory.#files.set(name, new FakeFile(name, bytes, this.stats))
  }

  seedDirectory(path: string): void {
    this.#ensureDirectory(path.split('/'))
  }

  file(path: string): FakeFile | undefined {
    const segments = path.split('/')
    const name = segments.pop()
    const directory = this.#findDirectory(segments)
    return name === undefined || directory === undefined
      ? undefined
      : directory.#files.get(name)
  }

  directory(path: string): FakeDirectory | undefined {
    return this.#findDirectory(path.split('/'))
  }

  asNative(): FileSystemDirectoryHandle {
    return this as unknown as FileSystemDirectoryHandle
  }

  #ensureDirectory(segments: readonly string[]): FakeDirectory {
    const [segment, ...remaining] = segments
    if (segment === undefined) {
      return this
    }
    let child = this.#directories.get(segment)
    if (child === undefined) {
      child = new FakeDirectory(segment, this.calls, this.stats)
      this.#directories.set(segment, child)
    }
    return child.#ensureDirectory(remaining)
  }

  #findDirectory(segments: readonly string[]): FakeDirectory | undefined {
    const [segment, ...remaining] = segments
    if (segment === undefined) {
      return this
    }
    const child = this.#directories.get(segment)
    if (child === undefined) {
      return undefined
    }
    return child.#findDirectory(remaining)
  }
}

class RecordingMetadata implements FsaMetadataWriter {
  readonly events: string[] = []

  async setFileMtime(handle: FileSystemFileHandle): Promise<void> {
    this.events.push(`file:${handle.name}`)
  }

  async setDirectoryMtime(handle: FileSystemDirectoryHandle): Promise<void> {
    this.events.push(`directory:${handle.name}`)
  }
}

describe('File System Access sink', () => {
  it('random-writes out-of-order blocks, skips siblings, and restores metadata depth-first', async () => {
    const root = new FakeDirectory('root')
    const metadata = new RecordingMetadata()
    const context = fixtureContext(
      [
        directory('tree'),
        directory('tree/nested'),
        file('tree/nested/file.bin', 6),
        file('tree/empty.bin', 0),
      ],
      new Map([
        [
          0,
          [
            range('tree/skipped.bin', 0, 2),
            range('tree/nested/file.bin', 0, 3),
          ],
        ],
        [1, [range('tree/nested/file.bin', 3, 3)]],
      ]),
    )
    const sink = createFileSystemDownloadSink(context, root.asNative(), metadata)

    await sink.writeBlock(chunk(1), new Uint8Array([4, 5, 6]))
    await sink.writeBlock(chunk(0), new Uint8Array([90, 91, 1, 2, 3]))
    await sink.finalize()

    expect(sink.deliveryOrder).toBe('any')
    expect(root.file('tree/nested/file.bin')?.bytes).toEqual(
      new Uint8Array([1, 2, 3, 4, 5, 6]),
    )
    expect(root.file('tree/empty.bin')?.bytes).toHaveLength(0)
    expect(root.file('tree/skipped.bin')).toBeUndefined()
    expect(metadata.events).toEqual([
      'file:file.bin',
      'file:empty.bin',
      'directory:nested',
      'directory:tree',
    ])
  })

  it('removes newly created output on abort even after its file stream completed', async () => {
    const root = new FakeDirectory('root')
    const context = fixtureContext(
      [file('new/path.bin', 2)],
      new Map([[0, [range('new/path.bin', 0, 2)]]]),
    )
    const sink = createFileSystemDownloadSink(context, root.asNative())

    await sink.writeBlock(chunk(0), new Uint8Array([3, 4]))
    await sink.abort(new Error('cancelled'))

    expect(root.directory('new')).toBeUndefined()
    expect(sink.has(chunk(0))).toBe(false)
  })

  it('refuses to overwrite a pre-existing file', async () => {
    const root = new FakeDirectory('root')
    root.seedFile('existing.bin', new Uint8Array([7, 7]))
    const context = fixtureContext(
      [file('existing.bin', 2)],
      new Map([[0, [range('existing.bin', 0, 2)]]]),
    )
    const sink = createFileSystemDownloadSink(context, root.asNative())

    await expect(sink.writeBlock(chunk(0), new Uint8Array([1, 2]))).rejects.toMatchObject({
      code: 'output-write',
    })
    expect(root.file('existing.bin')?.bytes).toEqual(new Uint8Array([7, 7]))
    await sink.abort(new Error('test cleanup'))
  })

  it('rejects overlapping file ranges from distinct blocks', async () => {
    const root = new FakeDirectory('root')
    const context = fixtureContext(
      [file('file.bin', 2)],
      new Map([
        [0, [range('file.bin', 0, 1)]],
        [1, [range('file.bin', 0, 1)]],
      ]),
    )
    const sink = createFileSystemDownloadSink(context, root.asNative())

    await sink.writeBlock(chunk(0), new Uint8Array([1]))
    await expect(sink.writeBlock(chunk(1), new Uint8Array([2]))).rejects.toMatchObject({
      code: 'invalid-layout',
    })
    await sink.abort(new Error('test cleanup'))
  })

  it('runs path policy before invoking any filesystem capability', () => {
    const root = new FakeDirectory('root')
    const context = fixtureContext(
      [file('../escape.bin', 0)],
      new Map(),
    )

    expect(() => createFileSystemDownloadSink(context, root.asNative())).toThrow()
    expect(root.calls).toHaveLength(0)
  })

  it('closes each complete tiny file before opening the next one', async () => {
    const root = new FakeDirectory('root')
    const entries = Array.from({ length: 64 }, (_, index) => file(`batch/${index}.bin`, 1))
    const context = fixtureContext(
      entries,
      new Map([[0, entries.map((entry) => range(entry.path, 0, 1))]]),
    )
    const sink = createFileSystemDownloadSink(context, root.asNative())

    await sink.writeBlock(chunk(0), Uint8Array.from({ length: entries.length }, (_, index) => index))
    await sink.finalize()

    expect(root.stats.maxOpenWritables).toBe(1)
    expect(root.stats.openWritables).toBe(0)
  })

  it('lets abort win over an in-flight block before availability or output survives', async () => {
    const writeStarted = deferred()
    const releaseWrite = deferred()
    const stats: FakeFsaStats = {
      openWritables: 0,
      maxOpenWritables: 0,
      beforeWrite: async () => {
        writeStarted.resolve()
        await releaseWrite.promise
      },
    }
    const root = new FakeDirectory('root', [], stats)
    const context = fixtureContext(
      [file('partial.bin', 2)],
      new Map([[0, [range('partial.bin', 0, 1)]]]),
    )
    const sink = createFileSystemDownloadSink(context, root.asNative())

    const writing = sink.writeBlock(chunk(0), Uint8Array.of(1))
    await writeStarted.promise
    const aborting = sink.abort(new Error('cancelled'))
    releaseWrite.resolve()

    await expect(writing).rejects.toMatchObject({
      code: 'invalid-state',
    } satisfies Partial<DownloadError>)
    await aborting
    expect(sink.has(chunk(0))).toBe(false)
    expect(root.file('partial.bin')).toBeUndefined()
  })

  it('reports browser write and cleanup failures through the download taxonomy', async () => {
    const writeFailure = new Error('browser write failed')
    const writeStats: FakeFsaStats = {
      openWritables: 0,
      maxOpenWritables: 0,
      writeError: writeFailure,
    }
    const writeRoot = new FakeDirectory('write-root', [], writeStats)
    const context = fixtureContext(
      [file('partial.bin', 2)],
      new Map([[0, [range('partial.bin', 0, 1)]]]),
    )
    const writeSink = createFileSystemDownloadSink(context, writeRoot.asNative())

    await expect(writeSink.writeBlock(chunk(0), Uint8Array.of(1))).rejects.toMatchObject({
      code: 'output-write',
      cause: writeFailure,
    } satisfies Partial<DownloadError>)
    await writeSink.abort(new Error('write cleanup'))

    const abortFailure = new Error('browser abort failed')
    const abortStats: FakeFsaStats = {
      openWritables: 0,
      maxOpenWritables: 0,
      abortError: abortFailure,
    }
    const abortRoot = new FakeDirectory('abort-root', [], abortStats)
    const abortSink = createFileSystemDownloadSink(context, abortRoot.asNative())
    await abortSink.writeBlock(chunk(0), Uint8Array.of(1))

    await expect(abortSink.abort(new Error('cancelled'))).rejects.toMatchObject({
      code: 'cleanup-failed',
    } satisfies Partial<DownloadError>)
    expect(abortSink.has(chunk(0))).toBe(false)
    expect(abortRoot.file('partial.bin')).toBeUndefined()
  })

  it('retries a transient exact-owned removal within abort settlement', async () => {
    const stats: FakeFsaStats = {
      openWritables: 0,
      maxOpenWritables: 0,
      removeFailuresRemaining: 1,
    }
    const root = new FakeDirectory('root', [], stats)
    const context = fixtureContext(
      [file('partial.bin', 2)],
      new Map([[0, [range('partial.bin', 0, 1)]]]),
    )
    const sink = createFileSystemDownloadSink(context, root.asNative())
    await sink.writeBlock(chunk(0), Uint8Array.of(1))

    await sink.abort(new Error('cancelled'))
    expect(root.file('partial.bin')).toBeUndefined()
    expect(sink.has(chunk(0))).toBe(false)
  })

  it('retains exact ownership after bounded cleanup exhaustion for a later retry', async () => {
    const stats: FakeFsaStats = {
      openWritables: 0,
      maxOpenWritables: 0,
      removeFailuresRemaining: FSA_CLEANUP_ATTEMPTS,
    }
    const root = new FakeDirectory('root', [], stats)
    const context = fixtureContext(
      [file('partial.bin', 2)],
      new Map([[0, [range('partial.bin', 0, 1)]]]),
    )
    const sink = createFileSystemDownloadSink(context, root.asNative())
    await sink.writeBlock(chunk(0), Uint8Array.of(1))

    await expect(sink.abort(new Error('cancelled'))).rejects.toMatchObject({
      code: 'cleanup-failed',
    } satisfies Partial<DownloadError>)
    expect(root.file('partial.bin')).toBeDefined()

    await sink.abort(new Error('retry cleanup'))
    expect(root.file('partial.bin')).toBeUndefined()
  })

  it('does not rewrite metadata on a pre-existing directory capability', async () => {
    const root = new FakeDirectory('root')
    root.seedDirectory('existing')
    const metadata = new RecordingMetadata()
    const context = fixtureContext(
      [directory('existing'), file('existing/new.bin', 0)],
      new Map(),
    )
    const sink = createFileSystemDownloadSink(context, root.asNative(), metadata)

    await sink.finalize()

    expect(root.directory('existing')).toBeDefined()
    expect(root.file('existing/new.bin')).toBeDefined()
    expect(metadata.events).toEqual(['file:new.bin'])
  })

  it('removes partial filesystem output when finalization detects a hole', async () => {
    const root = new FakeDirectory('root')
    const context = fixtureContext(
      [file('partial.bin', 2)],
      new Map([[0, [range('partial.bin', 0, 1)]]]),
    )
    const sink = createFileSystemDownloadSink(context, root.asNative())
    await sink.writeBlock(chunk(0), Uint8Array.of(1))

    await expect(sink.finalize()).rejects.toMatchObject({
      code: 'output-finalize',
    } satisfies Partial<DownloadError>)
    expect(root.file('partial.bin')).toBeUndefined()
    expect(sink.has(chunk(0))).toBe(false)
  })
})
