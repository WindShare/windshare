import type { ReceiveStore } from '../store/database'
import { spoolChunkId } from '../store/types'
import { completeZipMember } from './member'
import {
  ZipCrc32,
  checkedZipAdd,
  encodeZipEndRecords,
  encodeZipLocalHeader,
  normalizeZipEntry,
  planZipEntry,
  requiresZip64End,
  type ZipEntryPlanV1,
} from './policy'

const SPOOL_CHUNK_BYTES = 256 * 1024

interface SpoolSink {
  append(record: Uint8Array): Promise<void>
  readAll(): AsyncGenerator<Uint8Array>
  clear(): Promise<void>
}

class MemorySpool implements SpoolSink {
  readonly #records: Uint8Array[] = []
  async append(record: Uint8Array): Promise<void> { this.#records.push(record) }
  async *readAll(): AsyncGenerator<Uint8Array> {
    for (const record of this.#records) yield record
  }
  async clear(): Promise<void> { this.#records.length = 0 }
}

class StoreSpool implements SpoolSink {
  readonly #store: ReceiveStore
  readonly #taskId: string
  readonly #pending: Uint8Array[] = []
  #pendingBytes = 0
  #chunkCount = 0

  constructor(store: ReceiveStore, taskId: string) {
    this.#store = store
    this.#taskId = taskId
  }

  async append(record: Uint8Array): Promise<void> {
    this.#pending.push(record)
    this.#pendingBytes += record.byteLength
    if (this.#pendingBytes >= SPOOL_CHUNK_BYTES) {
      await this.#flushChunk()
    }
  }

  async *readAll(): AsyncGenerator<Uint8Array> {
    await this.#flushChunk()
    for (let index = 0; index < this.#chunkCount; index++) {
      const chunk = await this.#store.getZipSpoolChunk(this.#taskId, index)
      if (chunk !== undefined) yield chunk
    }
  }

  async clear(): Promise<void> {
    this.#pending.length = 0
    this.#pendingBytes = 0
    if (this.#chunkCount > 0) {
      await this.#store.deleteZipSpool(this.#taskId).catch(() => undefined)
      this.#chunkCount = 0
    }
  }

  async #flushChunk(): Promise<void> {
    if (this.#pending.length === 0) return
    const merged = new Uint8Array(this.#pendingBytes)
    let offset = 0
    for (const part of this.#pending) {
      merged.set(part, offset)
      offset += part.byteLength
    }
    this.#pending.length = 0
    this.#pendingBytes = 0
    const index = this.#chunkCount++
    await this.#store.putZipSpoolChunk({
      id: spoolChunkId(this.#taskId, index),
      taskId: this.#taskId,
      index,
      bytes: merged,
    })
  }
}

export interface StreamingZipWriterOptions {
  readonly taskId: string
  readonly store?: ReceiveStore
  readonly maxBytes: bigint
}

/**
 * Simple Store-mode ZIP64 writer assembling into bounded Blob parts for portable downloads.
 * Spools central directory entries into the unified `zip-spool` IndexedDB store when available.
 */
export class StreamingZipWriter {
  readonly #parts: Uint8Array<ArrayBuffer>[] = []
  readonly #spool: SpoolSink
  readonly #maxBytes: bigint
  #offset = 0n
  #entryCount = 0n
  #centralBytes = 0n

  constructor(options: StreamingZipWriterOptions) {
    this.#maxBytes = options.maxBytes
    this.#spool = options.store === undefined
      ? new MemorySpool()
      : new StoreSpool(options.store, options.taskId)
  }

  async addDirectory(path: readonly string[], modifiedTimeMilliseconds?: bigint): Promise<void> {
    const normalized = normalizeZipEntry({
      kind: 'directory',
      path,
      ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }),
    })
    const plan = planZipEntry(normalized, this.#offset)
    this.#push(encodeZipLocalHeader(plan))
    const done = completeZipMember(plan, 0n, 0)
    this.#push(done.descriptor)
    await this.#recordCentral(done.centralRecord)
  }

  beginFile(
    path: readonly string[],
    exactSize: bigint,
    modifiedTimeMilliseconds?: bigint,
  ): StreamingZipFileWriter {
    const normalized = normalizeZipEntry({
      kind: 'file',
      path,
      exactSize,
      ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }),
    })
    const plan = planZipEntry(normalized, this.#offset)
    this.#push(encodeZipLocalHeader(plan))
    return new StreamingZipFileWriter(
      plan,
      chunk => this.#push(chunk),
      crc32 => this.#finishFile(plan, crc32),
    )
  }

  async finalize(): Promise<Blob> {
    try {
      const centralOffset = this.#offset
      for await (const chunk of this.#spool.readAll()) {
        this.#push(chunk)
      }
      const base = {
        entryCount: this.#entryCount,
        centralDirectoryOffset: centralOffset,
        centralDirectoryBytes: this.#centralBytes,
      }
      const ends = encodeZipEndRecords({ ...base, zip64EndRequired: requiresZip64End(base) })
      if (ends.zip64End !== undefined) this.#push(ends.zip64End)
      if (ends.zip64Locator !== undefined) this.#push(ends.zip64Locator)
      this.#push(ends.classicEnd)
      return new Blob(this.#parts, { type: 'application/zip' })
    } finally {
      await this.#spool.clear()
    }
  }

  async abort(): Promise<void> {
    this.#parts.length = 0
    await this.#spool.clear()
  }

  async #finishFile(plan: ZipEntryPlanV1, crc32: number): Promise<void> {
    const done = completeZipMember(plan, plan.exactSize, crc32)
    this.#push(done.descriptor)
    await this.#recordCentral(done.centralRecord)
  }

  async #recordCentral(record: Uint8Array): Promise<void> {
    this.#centralBytes = checkedZipAdd(this.#centralBytes, BigInt(record.byteLength))
    this.#entryCount += 1n
    this.#checkBudget(checkedZipAdd(this.#offset, this.#centralBytes))
    await this.#spool.append(record)
  }

  #push(chunk: Uint8Array): void {
    const next = checkedZipAdd(this.#offset, BigInt(chunk.byteLength))
    this.#checkBudget(next)
    const copy = new Uint8Array(chunk.byteLength)
    copy.set(chunk)
    this.#parts.push(copy)
    this.#offset = next
  }

  #checkBudget(projectedBytes: bigint): void {
    if (projectedBytes > this.#maxBytes) {
      throw new DOMException(
        `Portable browser download is limited to ${Number(this.#maxBytes / (1024n * 1024n))} MiB`,
        'NotSupportedError',
      )
    }
  }
}

export class StreamingZipFileWriter {
  readonly #plan: ZipEntryPlanV1
  readonly #writeChunk: (chunk: Uint8Array) => void
  readonly #commit: (crc32: number) => Promise<void>
  readonly #crc = new ZipCrc32()
  #written = 0n

  constructor(
    plan: ZipEntryPlanV1,
    writeChunk: (chunk: Uint8Array) => void,
    commit: (crc32: number) => Promise<void>,
  ) {
    this.#plan = plan
    this.#writeChunk = writeChunk
    this.#commit = commit
  }

  write(data: Uint8Array): void {
    if (data.byteLength === 0) return
    const next = checkedZipAdd(this.#written, BigInt(data.byteLength))
    if (next > this.#plan.exactSize) {
      throw new RangeError('ZIP entry received more bytes than declared')
    }
    this.#crc.update(data)
    this.#writeChunk(data)
    this.#written = next
  }

  async close(): Promise<void> {
    if (this.#written !== this.#plan.exactSize) {
      throw new Error('ZIP entry closed before reaching its expected size')
    }
    await this.#commit(this.#crc.digest())
  }
}
