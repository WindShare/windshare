import { portableLimitExceededError } from '../../transfer/fault'
import { completeZipMember } from './member'
import {
  ZIP64_END_BYTES,
  ZIP_CLASSIC_END_BYTES,
  ZipCrc32,
  checkedZipAdd,
  encodeZipEndRecords,
  encodeZipLocalHeader,
  normalizeZipEntry,
  planZipEntry,
  requiresZip64End,
  type NormalizedZipEntry,
  type ZipEntryPlanV1,
} from './policy'

// Coalesce central-directory records into bounded slabs so archives with many small
// entries avoid per-entry TypedArray overhead while staying purely in memory.
const CENTRAL_DIRECTORY_SLAB_BYTES = 256 * 1024

class CentralDirectoryBuffer {
  readonly #slabs: Uint8Array<ArrayBuffer>[] = []
  readonly #pending: Uint8Array[] = []
  #pendingBytes = 0

  append(record: Uint8Array): void {
    this.#pending.push(record)
    this.#pendingBytes += record.byteLength
    if (this.#pendingBytes >= CENTRAL_DIRECTORY_SLAB_BYTES) {
      this.#coalescePending()
    }
  }

  drain(): readonly Uint8Array<ArrayBuffer>[] {
    this.#coalescePending()
    const slabs = this.#slabs.slice()
    this.#slabs.length = 0
    return slabs
  }

  clear(): void {
    this.#slabs.length = 0
    this.#pending.length = 0
    this.#pendingBytes = 0
  }

  #coalescePending(): void {
    if (this.#pending.length === 0) return
    const merged = new Uint8Array(this.#pendingBytes)
    let offset = 0
    for (const part of this.#pending) {
      merged.set(part, offset)
      offset += part.byteLength
    }
    this.#pending.length = 0
    this.#pendingBytes = 0
    this.#slabs.push(merged)
  }
}

export interface StreamingZipWriterOptions {
  readonly maxBytes: bigint
}

type StreamingZipState = 'idle' | 'file-open' | 'finalized' | 'aborted'

interface PendingZipDirectory {
  readonly entry: NormalizedZipEntry
  readonly streamBytes: bigint
  readonly centralBytes: bigint
}

/**
 * In-memory Store-mode ZIP64 writer assembling bounded Blob parts for portable downloads.
 * Keeps central-directory records in memory within the archive byte cap so abandoned
 * portable downloads leave no persistent browser storage behind.
 */
export class StreamingZipWriter {
  readonly #parts: Uint8Array<ArrayBuffer>[] = []
  readonly #central = new CentralDirectoryBuffer()
  readonly #maxBytes: bigint
  readonly #pendingDirectories: PendingZipDirectory[] = []
  #offset = 0n
  #entryCount = 0n
  #centralBytes = 0n
  #pendingDirectoryStreamBytes = 0n
  #pendingDirectoryCentralBytes = 0n
  #state: StreamingZipState = 'idle'
  #activeFile: StreamingZipFileWriter | undefined
  #activePlan: ZipEntryPlanV1 | undefined

  constructor(options: StreamingZipWriterOptions) {
    this.#maxBytes = options.maxBytes
  }

  addDirectory(path: readonly string[], modifiedTimeMilliseconds?: bigint): void {
    this.#requireWritable('add a directory')
    const normalized = normalizeZipEntry({
      kind: 'directory',
      path,
      ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }),
    })
    if (this.#activeFile !== undefined) {
      // Defer directory headers until the active file finishes its payload and descriptor
      // so sequential stream bytes never interleave.
      this.#queueDirectory(normalized)
      return
    }
    this.#writeDirectory(normalized)
  }

  beginFile(
    path: readonly string[],
    exactSize: bigint,
    modifiedTimeMilliseconds?: bigint,
  ): StreamingZipFileWriter {
    this.#requireWritable('open a file')
    if (this.#activeFile !== undefined) {
      throw new Error('Cannot open a new ZIP file while another file is still open')
    }
    const normalized = normalizeZipEntry({
      kind: 'file',
      path,
      exactSize,
      ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }),
    })
    const plan = planZipEntry(normalized, this.#offset)
    // Reject before reading any payload when the full entry and ZIP end records cannot fit.
    this.#checkBudget(this.#projectedArchiveBytes(plan.entryStreamBytes, plan.centralRecordBytes, 1n))
    this.#pushOwned(encodeZipLocalHeader(plan))
    this.#state = 'file-open'
    this.#activePlan = plan
    const writer: StreamingZipFileWriter = new StreamingZipFileWriter(
      plan,
      chunk => this.#writeActiveChunk(writer, chunk),
      crc32 => this.#finishFile(writer, plan, crc32),
      () => this.#abortActiveFile(writer),
    )
    this.#activeFile = writer
    return writer
  }

  finalize(): Blob {
    this.#requireWritable('finalize the archive')
    if (this.#activeFile !== undefined) {
      throw new Error('Cannot finalize ZIP while a file entry is still open')
    }
    this.#state = 'finalized'
    try {
      const centralOffset = this.#offset
      for (const slab of this.#central.drain()) {
        this.#pushOwned(slab)
      }
      const base = {
        entryCount: this.#entryCount,
        centralDirectoryOffset: centralOffset,
        centralDirectoryBytes: this.#centralBytes,
      }
      const ends = encodeZipEndRecords({ ...base, zip64EndRequired: requiresZip64End(base) })
      if (ends.zip64End !== undefined) this.#pushOwned(ends.zip64End)
      if (ends.zip64Locator !== undefined) this.#pushOwned(ends.zip64Locator)
      this.#pushOwned(ends.classicEnd)
      return new Blob(this.#parts, { type: 'application/zip' })
    } finally {
      this.#parts.length = 0
      this.#central.clear()
    }
  }

  abort(): void {
    this.#markAborted()
  }

  #requireWritable(action: string): void {
    if (this.#state === 'aborted') {
      throw new Error(`Cannot ${action} after ZIP writer was aborted`)
    }
    if (this.#state === 'finalized') {
      throw new Error(`Cannot ${action} after ZIP writer was finalized`)
    }
  }

  #queueDirectory(entry: NormalizedZipEntry): void {
    const activeEnd = this.#activePlan === undefined
      ? this.#offset
      : checkedZipAdd(this.#activePlan.localHeaderOffset, this.#activePlan.entryStreamBytes)
    const estimate = planZipEntry(entry, checkedZipAdd(activeEnd, this.#pendingDirectoryStreamBytes))
    this.#checkBudget(this.#projectedArchiveBytes(estimate.entryStreamBytes, estimate.centralRecordBytes, 1n))
    this.#pendingDirectoryStreamBytes = checkedZipAdd(this.#pendingDirectoryStreamBytes, estimate.entryStreamBytes)
    this.#pendingDirectoryCentralBytes = checkedZipAdd(this.#pendingDirectoryCentralBytes, estimate.centralRecordBytes)
    this.#pendingDirectories.push({
      entry,
      streamBytes: estimate.entryStreamBytes,
      centralBytes: estimate.centralRecordBytes,
    })
  }

  #writeDirectory(entry: NormalizedZipEntry): void {
    const plan = planZipEntry(entry, this.#offset)
    this.#checkBudget(this.#projectedArchiveBytes(plan.entryStreamBytes, plan.centralRecordBytes, 1n))
    this.#pushOwned(encodeZipLocalHeader(plan))
    const done = completeZipMember(plan, 0n, 0)
    this.#pushOwned(done.descriptor)
    this.#recordCentral(done.centralRecord)
  }

  #writeActiveChunk(writer: StreamingZipFileWriter, chunk: Uint8Array): void {
    if (this.#state !== 'file-open' || this.#activeFile !== writer) {
      throw new Error('ZIP entry is no longer active')
    }
    this.#pushCopy(chunk)
  }

  #finishFile(
    writer: StreamingZipFileWriter,
    plan: ZipEntryPlanV1,
    crc32: number,
  ): void {
    if (this.#state !== 'file-open' || this.#activeFile !== writer) {
      throw new Error('ZIP entry is no longer active')
    }
    try {
      const done = completeZipMember(plan, plan.exactSize, crc32)
      this.#pushOwned(done.descriptor)
      this.#activePlan = undefined
      this.#recordCentral(done.centralRecord)
      this.#flushPendingDirectories()
    } catch (error) {
      this.#markAborted()
      throw error
    }
    this.#activeFile = undefined
    this.#state = 'idle'
  }

  #flushPendingDirectories(): void {
    while (this.#pendingDirectories.length > 0) {
      const pending = this.#pendingDirectories.shift()!
      this.#pendingDirectoryStreamBytes -= pending.streamBytes
      this.#pendingDirectoryCentralBytes -= pending.centralBytes
      this.#writeDirectory(pending.entry)
    }
  }

  #abortActiveFile(writer: StreamingZipFileWriter): void {
    if (this.#activeFile !== writer) return
    // A sequential ZIP stream cannot rewind a partially written local header/payload.
    this.#markAborted()
  }

  #markAborted(): void {
    this.#state = 'aborted'
    this.#activeFile = undefined
    this.#activePlan = undefined
    this.#parts.length = 0
    this.#central.clear()
    this.#pendingDirectories.length = 0
    this.#pendingDirectoryStreamBytes = 0n
    this.#pendingDirectoryCentralBytes = 0n
  }

  #recordCentral(record: Uint8Array): void {
    const nextCentral = checkedZipAdd(this.#centralBytes, BigInt(record.byteLength))
    const pendingBytes = checkedZipAdd(this.#pendingDirectoryStreamBytes, this.#pendingDirectoryCentralBytes)
    this.#checkBudget(checkedZipAdd(this.#offset, nextCentral, pendingBytes))
    this.#centralBytes = nextCentral
    this.#entryCount += 1n
    this.#central.append(record)
  }

  #pushOwned(chunk: Uint8Array<ArrayBuffer>): void {
    const next = this.#reservePush(chunk.byteLength)
    this.#parts.push(chunk)
    this.#offset = next
  }

  #pushCopy(chunk: Uint8Array): void {
    const next = this.#reservePush(chunk.byteLength)
    const copy = new Uint8Array(chunk.byteLength)
    copy.set(chunk)
    this.#parts.push(copy)
    this.#offset = next
  }

  #reservePush(byteLength: number): bigint {
    const next = checkedZipAdd(this.#offset, BigInt(byteLength))
    const pendingBytes = checkedZipAdd(this.#pendingDirectoryStreamBytes, this.#pendingDirectoryCentralBytes)
    const projected = this.#state === 'finalized'
      ? next
      : checkedZipAdd(next, this.#centralBytes, pendingBytes)
    this.#checkBudget(projected)
    return next
  }

  #projectedArchiveBytes(addedStreamBytes: bigint, addedCentralBytes: bigint, addedEntries: bigint): bigint {
    const baseStreamEnd = this.#activePlan === undefined
      ? this.#offset
      : checkedZipAdd(this.#activePlan.localHeaderOffset, this.#activePlan.entryStreamBytes)
    const baseCentral = this.#activePlan === undefined
      ? this.#centralBytes
      : checkedZipAdd(this.#centralBytes, this.#activePlan.centralRecordBytes)
    const baseEntries = this.#activePlan === undefined ? this.#entryCount : this.#entryCount + 1n

    const streamEnd = checkedZipAdd(baseStreamEnd, this.#pendingDirectoryStreamBytes, addedStreamBytes)
    const centralEnd = checkedZipAdd(baseCentral, this.#pendingDirectoryCentralBytes, addedCentralBytes)
    const totalEntries = checkedZipAdd(baseEntries, BigInt(this.#pendingDirectories.length), addedEntries)
    const endBytes = requiresZip64End({
      entryCount: totalEntries,
      centralDirectoryOffset: streamEnd,
      centralDirectoryBytes: centralEnd,
    })
      ? checkedZipAdd(ZIP_CLASSIC_END_BYTES, ZIP64_END_BYTES)
      : ZIP_CLASSIC_END_BYTES
    return checkedZipAdd(streamEnd, centralEnd, endBytes)
  }

  #checkBudget(projectedBytes: bigint): void {
    if (projectedBytes > this.#maxBytes) {
      throw portableLimitExceededError(this.#maxBytes)
    }
  }
}

export class StreamingZipFileWriter {
  readonly #plan: ZipEntryPlanV1
  readonly #writeChunk: (chunk: Uint8Array) => void
  readonly #commit: (crc32: number) => void
  readonly #onAbort: () => void
  readonly #crc = new ZipCrc32()
  #written = 0n
  #state: 'open' | 'closed' | 'aborted' = 'open'

  constructor(
    plan: ZipEntryPlanV1,
    writeChunk: (chunk: Uint8Array) => void,
    commit: (crc32: number) => void,
    onAbort: () => void = () => undefined,
  ) {
    this.#plan = plan
    this.#writeChunk = writeChunk
    this.#commit = commit
    this.#onAbort = onAbort
  }

  write(data: Uint8Array): void {
    this.#requireOpen()
    if (data.byteLength === 0) return
    const next = checkedZipAdd(this.#written, BigInt(data.byteLength))
    if (next > this.#plan.exactSize) {
      throw new RangeError('ZIP entry received more bytes than declared')
    }
    this.#writeChunk(data)
    this.#crc.update(data)
    this.#written = next
  }

  close(): void {
    this.#requireOpen()
    if (this.#written !== this.#plan.exactSize) {
      throw new Error('ZIP entry closed before reaching its expected size')
    }
    try {
      this.#commit(this.#crc.digest())
      this.#state = 'closed'
    } catch (error) {
      this.#state = 'aborted'
      throw error
    }
  }

  abort(): void {
    if (this.#state === 'closed' || this.#state === 'aborted') return
    this.#state = 'aborted'
    this.#onAbort()
  }

  #requireOpen(): void {
    if (this.#state !== 'open') {
      throw new Error('ZIP entry is no longer open')
    }
  }
}
