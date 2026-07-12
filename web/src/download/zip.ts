import { ZipWriter, type ZipWriterAddDataOptions } from '@zip.js/zip.js'

import type {
  ChunkIndex,
  FileManifestEntry,
  ManifestEntry,
  OrderedBlockSink,
} from '../contracts'
import { ChunkAvailabilityMap } from './availability'
import { DownloadError } from './errors'
import type { DownloadSinkContext } from './model'
import { BlockProjector, type SelectedBlockSlice } from './projector'

const ZIP_STORE_LEVEL = 0
// Zip.js writes DOS local calendar fields and applies these same local-date
// endpoints internally. UTC endpoints can cross a calendar boundary in the
// browser timezone and overflow the dependency's otherwise valid DOS range.
const ZIP_MIN_DATE_MS = new Date(1980, 0, 1).getTime()
const ZIP_MAX_DATE_MS = new Date(2107, 11, 31).getTime()

type SinkState = 'open' | 'finalized' | 'aborted'

function zipDate(mtime: number): Date {
  return new Date(Math.min(Math.max(mtime, ZIP_MIN_DATE_MS), ZIP_MAX_DATE_MS))
}

function entryOptions(
  entry: ManifestEntry,
  signal: AbortSignal,
  directory: boolean,
): ZipWriterAddDataOptions {
  return {
    level: ZIP_STORE_LEVEL,
    zip64: true,
    bufferedWrite: false,
    keepOrder: true,
    useWebWorkers: false,
    extendedTimestamp: false,
    lastModDate: zipDate(entry.mtime),
    directory,
    signal,
  }
}

class OutputBridge {
  readonly stream: WritableStream<Uint8Array>

  readonly #writer: WritableStreamDefaultWriter<Uint8Array>
  #settled = false

  constructor(output: WritableStream<Uint8Array>) {
    if (output.locked) {
      throw new DownloadError('invalid-state', 'The ZIP output stream is already locked')
    }
    this.#writer = output.getWriter()
    this.stream = new WritableStream<Uint8Array>({
      write: (chunk) => this.#writer.write(chunk),
      close: () => this.#close(),
      abort: (reason) => this.abort(reason),
    })
  }

  async abort(reason: unknown): Promise<void> {
    if (this.#settled) {
      return
    }
    this.#settled = true
    try {
      await this.#writer.abort(reason)
    } finally {
      this.#writer.releaseLock()
    }
  }

  async #close(): Promise<void> {
    if (this.#settled) {
      return
    }
    this.#settled = true
    try {
      await this.#writer.close()
    } finally {
      this.#writer.releaseLock()
    }
  }
}

class ActiveZipFile {
  readonly entry: FileManifestEntry

  readonly #writer: WritableStreamDefaultWriter<Uint8Array>
  readonly #completion: Promise<void>
  #failure: unknown
  #failed = false
  #released = false

  written = 0

  constructor(
    zip: ZipWriter<unknown>,
    entry: FileManifestEntry,
    signal: AbortSignal,
  ) {
    this.entry = entry
    const pipe = new TransformStream<Uint8Array, Uint8Array>()
    this.#writer = pipe.writable.getWriter()
    this.#completion = zip
      .add(entry.path, pipe.readable, entryOptions(entry, signal, false))
      .then(
        () => undefined,
        (error: unknown) => {
          this.#failed = true
          this.#failure = error
        },
      )
  }

  async write(data: Uint8Array): Promise<void> {
    try {
      await this.#writer.write(data)
    } catch (error) {
      throw new DownloadError('output-write', 'Could not stream a ZIP entry', error)
    }
    this.#throwFailure()
    this.written += data.byteLength
  }

  async close(): Promise<void> {
    try {
      await this.#writer.close()
      await this.#completion
      this.#throwFailure()
    } finally {
      this.#release()
    }
  }

  async abort(reason: unknown): Promise<void> {
    try {
      await this.#writer.abort(reason)
      await this.#completion
    } catch {
      // The transfer's original failure remains the useful abort reason.
    } finally {
      this.#release()
    }
  }

  #throwFailure(): void {
    if (this.#failed) {
      throw new DownloadError('output-write', 'Could not stream a ZIP entry', this.#failure)
    }
  }

  #release(): void {
    if (!this.#released) {
      this.#released = true
      this.#writer.releaseLock()
    }
  }
}

/**
 * Streams one selected entry at a time into a store-mode Zip64 writer. Only the
 * central-directory metadata and the active entry's backpressure queue are
 * retained; archive content is never accumulated in a Blob or byte array.
 */
export class ZipDownloadSink implements OrderedBlockSink {
  readonly deliveryOrder = 'ascending' as const

  readonly #projector: BlockProjector
  readonly #bridge: OutputBridge
  readonly #abortController = new AbortController()
  readonly #zip: ZipWriter<unknown>
  readonly #have: ChunkAvailabilityMap

  #state: SinkState = 'open'
  #entryCursor = 0
  #active: ActiveZipFile | undefined
  #writing = false

  constructor(projector: BlockProjector, output: WritableStream<Uint8Array>) {
    this.#projector = projector
    this.#have = new ChunkAvailabilityMap(projector.chunks)
    this.#bridge = new OutputBridge(output)
    this.#zip = new ZipWriter(this.#bridge.stream, {
      level: ZIP_STORE_LEVEL,
      zip64: true,
      bufferedWrite: false,
      keepOrder: true,
      useWebWorkers: false,
      extendedTimestamp: false,
      signal: this.#abortController.signal,
    })
  }

  has(index: ChunkIndex): boolean {
    return this.#have.has(index)
  }

  async writeBlock(index: ChunkIndex, plaintext: Uint8Array): Promise<void> {
    this.#assertOpen()
    if (this.#writing) {
      throw new DownloadError('out-of-order', 'Ordered ZIP output accepts one block at a time')
    }
    if (this.#have.has(index)) {
      throw new DownloadError('duplicate-block', 'The block was already written')
    }
    this.#writing = true
    try {
      const slices = this.#projector.project(index, plaintext)
      for (const slice of slices) {
        await this.#writeSlice(slice)
      }
      this.#assertOpen()
      this.#have.add(index)
    } finally {
      this.#writing = false
    }
  }

  async finalize(): Promise<void> {
    this.#assertOpen()
    if (this.#writing) {
      throw new DownloadError('invalid-state', 'Cannot finalize while a ZIP block is active')
    }
    try {
      if (this.#active !== undefined) {
        throw new DownloadError('output-finalize', 'A ZIP file is missing selected bytes')
      }
      await this.#emitRemainingEmptyEntries()
      await this.#zip.close(undefined, { zip64: true })
      this.#state = 'finalized'
    } catch (error) {
      try {
        await this.abort(error)
      } catch (cleanupError) {
        throw new DownloadError(
          'output-finalize',
          'Could not finalize or abort ZIP output',
          new AggregateError([error, cleanupError]),
        )
      }
      if (error instanceof DownloadError) {
        throw error
      }
      throw new DownloadError('output-finalize', 'Could not finalize ZIP output', error)
    }
  }

  async abort(reason: unknown): Promise<void> {
    if (this.#state !== 'open') {
      return
    }
    this.#state = 'aborted'
    this.#abortController.abort(reason)
    try {
      const results = await Promise.allSettled([
        this.#active?.abort(reason) ?? Promise.resolve(),
        this.#bridge.abort(reason),
      ])
      const failures = results
        .filter((result): result is PromiseRejectedResult => result.status === 'rejected')
        .map((result) => result.reason)
      if (failures.length > 0) {
        throw new DownloadError(
          'cleanup-failed',
          'Could not abort ZIP output',
          new AggregateError(failures),
        )
      }
    } finally {
      this.#have.clear()
      this.#active = undefined
    }
  }

  async #writeSlice(slice: SelectedBlockSlice): Promise<void> {
    if (this.#active === undefined) {
      const expected = this.#nextDataEntry()
      if (expected?.path !== slice.path || slice.offset !== 0) {
        throw new DownloadError(
          'out-of-order',
          'ZIP entries must begin in manifest order at file offset zero',
        )
      }
    }
    await this.#advanceToFile(slice.path)
    const active = this.#active
    if (
      active === undefined ||
      active.entry.path !== slice.path ||
      active.written !== slice.offset
    ) {
      throw new DownloadError(
        'out-of-order',
        'ZIP entries must receive selected bytes in manifest and file-offset order',
      )
    }
    await active.write(slice.data)
    if (active.written > active.entry.size) {
      throw new DownloadError('invalid-layout', 'A ZIP entry exceeded its selected file size')
    }
    if (active.written === active.entry.size) {
      await active.close()
      this.#active = undefined
      this.#entryCursor += 1
    }
  }

  async #advanceToFile(path: string): Promise<void> {
    if (this.#active !== undefined) {
      return
    }
    while (this.#entryCursor < this.#projector.entries.length) {
      const entry = this.#projector.entries[this.#entryCursor]
      if (entry === undefined) {
        break
      }
      if (entry.kind === 'directory' || entry.size === 0) {
        await this.#addEmptyEntry(entry)
        this.#entryCursor += 1
        continue
      }
      if (entry.path !== path) {
        throw new DownloadError(
          'out-of-order',
          'ZIP blocks arrived before an earlier selected file was complete',
        )
      }
      this.#active = new ActiveZipFile(this.#zip, entry, this.#abortController.signal)
      return
    }
    throw new DownloadError('invalid-layout', 'A block references no remaining ZIP entry')
  }

  async #emitRemainingEmptyEntries(): Promise<void> {
    while (this.#entryCursor < this.#projector.entries.length) {
      const entry = this.#projector.entries[this.#entryCursor]
      if (entry === undefined) {
        break
      }
      if (entry.kind === 'file' && entry.size > 0) {
        throw new DownloadError('output-finalize', 'A ZIP file is missing selected bytes')
      }
      await this.#addEmptyEntry(entry)
      this.#entryCursor += 1
    }
  }

  async #addEmptyEntry(entry: ManifestEntry): Promise<void> {
    const name = entry.kind === 'directory' ? `${entry.path}/` : entry.path
    try {
      await this.#zip.add(
        name,
        undefined,
        entryOptions(entry, this.#abortController.signal, entry.kind === 'directory'),
      )
    } catch (error) {
      throw new DownloadError('output-write', 'Could not add an empty ZIP entry', error)
    }
  }

  #nextDataEntry(): FileManifestEntry | undefined {
    for (let cursor = this.#entryCursor; cursor < this.#projector.entries.length; cursor += 1) {
      const entry = this.#projector.entries[cursor]
      if (entry?.kind === 'file' && entry.size > 0) {
        return entry
      }
    }
    return undefined
  }

  #assertOpen(): void {
    if (this.#state !== 'open') {
      throw new DownloadError('invalid-state', 'The ZIP output is no longer open')
    }
  }
}

export function createZipDownloadSink(
  context: DownloadSinkContext,
  output: WritableStream<Uint8Array>,
): ZipDownloadSink {
  const projector = new BlockProjector(context)
  return new ZipDownloadSink(projector, output)
}
