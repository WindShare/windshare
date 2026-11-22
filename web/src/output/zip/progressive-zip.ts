import { bigintToSafeNumber, type ByteRange } from '../../content/geometry'
import type { ObjectCheckpointCoordinator } from '../native-object/coordinator'
import type { ReceiveStore } from '../store/database'
import {
  fileRecordId,
  fromRangeTuples,
  maxRangeEnd,
  toRangeTuples,
  type ReceiveFileRecord,
  type ZipEntryLayoutRecord,
} from '../store/types'
import { completeZipEntryCrc, insertZipCrcRange, zipBytesCrc32, type ZipCrcRange } from './crc-ranges'
import { completeZipMember, zipMemberCoordinates } from './member'
import {
  checkedZipAdd,
  encodeZipCentralDirectoryRecord,
  encodeZipDataDescriptor,
  encodeZipEndRecords,
  encodeZipLocalHeader,
  normalizeZipEntry,
  planZipEntry,
  requiresZip64End,
  type ZipEntryPlanV1,
} from './policy'

const DIR_ENTRY_PREFIX = '__dir__:'

export interface ProgressiveZipEntryInput {
  readonly fileId: string
  readonly path: readonly string[]
  readonly revision: string
  readonly sourceMetadata?: string
  readonly exactSize: bigint
  readonly modifiedTimeMilliseconds?: bigint
}

export interface OpenedProgressiveZipFile {
  readonly record: ReceiveFileRecord
  readonly plan: ZipEntryPlanV1
  readonly completedRanges: readonly ByteRange[]
  readonly alreadyCompleted: boolean
  crcRanges: readonly ZipCrcRange[]
}

/**
 * Resumable single-object OPFS ZIP64 writer.
 * Allocates fixed non-overlapping slots per file inside ONE OPFS SyncAccessHandle so
 * parallel writes never duplicate disk usage, flushing physical bytes before every IDB write.
 */
export class ProgressiveZipWriter {
  readonly #taskId: string
  readonly #store: ReceiveStore
  readonly #io: ObjectCheckpointCoordinator
  readonly #entries = new Map<string, ReceiveFileRecord>()
  #allocatedLength: bigint
  #nextSequence: number

  private constructor(
    taskId: string,
    store: ReceiveStore,
    io: ObjectCheckpointCoordinator,
    existing: readonly ReceiveFileRecord[],
    allocatedLength: bigint,
  ) {
    this.#taskId = taskId
    this.#store = store
    this.#io = io
    this.#allocatedLength = allocatedLength
    let maxSeq = 0
    for (const row of existing) {
      this.#entries.set(row.fileId, row)
      const seq = row.zipEntry?.sequence ?? 0
      if (seq > maxSeq) maxSeq = seq
    }
    this.#nextSequence = maxSeq + 1
  }

  static async open(
    taskId: string,
    store: ReceiveStore,
    io: ObjectCheckpointCoordinator,
  ): Promise<ProgressiveZipWriter> {
    const existing = await store.listTaskFiles(taskId)
    const physicalSize = await io.size()
    let allocated = 0n
    const valid: ReceiveFileRecord[] = []

    for (const row of existing) {
      const zip = row.zipEntry
      if (zip === undefined) continue
      // Verify physical OPFS file actually contains every claimed completed range.
      const requiredPhysical = row.status === 'completed'
        ? zip.endOffset
        : zip.payloadOffset + maxRangeEnd(row.completedRanges)
      if (physicalSize < requiredPhysical) {
        // Truncated/corrupted range: reset coverage for this entry while preserving its slot if within physical bounds.
        const resetRow: ReceiveFileRecord = Object.freeze({
          ...row,
          completedRanges: Object.freeze([]),
          status: 'receiving',
          zipEntry: Object.freeze({ ...zip, crcRanges: Object.freeze([]) }),
        })
        valid.push(resetRow)
      } else {
        valid.push(row)
      }
      if (zip.endOffset > allocated) allocated = zip.endOffset
    }

    return new ProgressiveZipWriter(taskId, store, io, valid, allocated)
  }

  get allocatedLength(): bigint {
    return this.#allocatedLength
  }

  async ensureDirectory(directoryId: string, path: readonly string[], modifiedTimeMilliseconds?: bigint): Promise<void> {
    const key = `${DIR_ENTRY_PREFIX}${directoryId}`
    if (this.#entries.has(key)) return
    const normalized = normalizeZipEntry({
      kind: 'directory',
      path,
      ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }),
    })
    const plan = planZipEntry(normalized, this.#allocatedLength)
    const coords = zipMemberCoordinates(plan)
    const header = encodeZipLocalHeader(plan)
    const done = completeZipMember(plan, 0n, 0)

    await this.#io.writeAt(plan.localHeaderOffset, header)
    await this.#io.writeAt(coords.descriptorOffset, done.descriptor)

    const zipEntry: ZipEntryLayoutRecord = Object.freeze({
      sequence: this.#nextSequence++,
      kind: 'directory',
      localHeaderOffset: plan.localHeaderOffset,
      payloadOffset: coords.payloadOffset,
      descriptorOffset: coords.descriptorOffset,
      endOffset: coords.endOffset,
      crcRanges: Object.freeze([]),
      crc32: 0,
      ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }),
    })
    const row: ReceiveFileRecord = Object.freeze({
      id: fileRecordId(this.#taskId, key),
      taskId: this.#taskId,
      fileId: key,
      revision: 'dir',
      path,
      size: 0n,
      completedRanges: Object.freeze([]),
      status: 'completed',
      zipEntry,
    })
    this.#allocatedLength = coords.endOffset
    this.#entries.set(key, row)
    await this.#io.checkpoint('zip-directory', () => this.#store.putFile(row))
  }

  async openFile(input: ProgressiveZipEntryInput): Promise<OpenedProgressiveZipFile> {
    const existing = this.#entries.get(input.fileId)
    if (existing?.zipEntry !== undefined) {
      const normalized = normalizeZipEntry({
        kind: 'file',
        path: existing.path,
        exactSize: existing.size,
        ...(existing.zipEntry.modifiedTimeMilliseconds === undefined
          ? {}
          : { modifiedTimeMilliseconds: existing.zipEntry.modifiedTimeMilliseconds }),
      })
      const plan = planZipEntry(normalized, existing.zipEntry.localHeaderOffset)
      return {
        record: existing,
        plan,
        completedRanges: fromRangeTuples(existing.completedRanges),
        alreadyCompleted: existing.status === 'completed',
        crcRanges: existing.zipEntry.crcRanges,
      }
    }

    const normalized = normalizeZipEntry({
      kind: 'file',
      path: input.path,
      exactSize: input.exactSize,
      ...(input.modifiedTimeMilliseconds === undefined
        ? {}
        : { modifiedTimeMilliseconds: input.modifiedTimeMilliseconds }),
    })
    const plan = planZipEntry(normalized, this.#allocatedLength)
    const coords = zipMemberCoordinates(plan)
    this.#allocatedLength = coords.endOffset

    await this.#io.writeAt(plan.localHeaderOffset, encodeZipLocalHeader(plan))
    const zipEntry: ZipEntryLayoutRecord = Object.freeze({
      sequence: this.#nextSequence++,
      kind: 'file',
      localHeaderOffset: plan.localHeaderOffset,
      payloadOffset: coords.payloadOffset,
      descriptorOffset: coords.descriptorOffset,
      endOffset: coords.endOffset,
      crcRanges: Object.freeze([]),
      ...(input.modifiedTimeMilliseconds === undefined
        ? {}
        : { modifiedTimeMilliseconds: input.modifiedTimeMilliseconds }),
    })
    const row: ReceiveFileRecord = Object.freeze({
      id: fileRecordId(this.#taskId, input.fileId),
      taskId: this.#taskId,
      fileId: input.fileId,
      revision: input.revision,
      ...(input.sourceMetadata === undefined ? {} : { sourceMetadata: input.sourceMetadata }),
      path: input.path,
      size: input.exactSize,
      completedRanges: Object.freeze([]),
      status: 'receiving',
      zipEntry,
    })
    this.#entries.set(input.fileId, row)
    await this.#io.checkpoint('zip-file-allocate', () => this.#store.putFile(row))
    return {
      record: row,
      plan,
      completedRanges: Object.freeze([]),
      alreadyCompleted: false,
      crcRanges: Object.freeze([]),
    }
  }

  async writeFileSlice(
    opened: OpenedProgressiveZipFile,
    offset: bigint,
    data: Uint8Array,
  ): Promise<void> {
    if (data.byteLength === 0) return
    const end = offset + BigInt(data.byteLength)
    const crc32 = zipBytesCrc32(data)
    await this.#io.writeAt(opened.record.zipEntry!.payloadOffset + offset, data)
    opened.crcRanges = insertZipCrcRange(opened.crcRanges, { start: offset, end, crc32 })
  }

  async checkpointFile(
    opened: OpenedProgressiveZipFile,
    ranges: readonly ByteRange[],
  ): Promise<void> {
    const updated: ReceiveFileRecord = Object.freeze({
      ...opened.record,
      completedRanges: toRangeTuples(ranges),
      zipEntry: Object.freeze({
        ...opened.record.zipEntry!,
        crcRanges: opened.crcRanges,
      }),
    })
    this.#entries.set(updated.fileId, updated)
    await this.#io.checkpoint('zip-file-checkpoint', () => this.#store.putFile(updated))
  }

  async commitFile(
    opened: OpenedProgressiveZipFile,
    ranges: readonly ByteRange[],
  ): Promise<void> {
    const crc32 = completeZipEntryCrc(opened.crcRanges, opened.plan.exactSize)
    if (crc32 === undefined) {
      throw new Error('ZIP file payload CRC ranges are incomplete at commit')
    }
    const descriptor = encodeZipDataDescriptor(opened.plan, crc32)
    await this.#io.writeAt(opened.record.zipEntry!.descriptorOffset, descriptor)
    const updated: ReceiveFileRecord = Object.freeze({
      ...opened.record,
      completedRanges: toRangeTuples(ranges),
      status: 'completed',
      zipEntry: Object.freeze({
        ...opened.record.zipEntry!,
        crcRanges: opened.crcRanges,
        crc32,
      }),
    })
    this.#entries.set(updated.fileId, updated)
    await this.#io.checkpoint('zip-file-commit', () => this.#store.putFile(updated))
  }

  /** Appends Central Directory + EOCD records to the OPFS file and returns its exact sealed length. */
  async finalize(): Promise<bigint> {
    const ordered = [...this.#entries.values()]
      .filter(entry => entry.status === 'completed' && entry.zipEntry !== undefined)
      .sort((left, right) => left.zipEntry!.sequence - right.zipEntry!.sequence)

    let offset = this.#allocatedLength
    const centralStart = offset
    let centralBytes = 0n

    for (const entry of ordered) {
      const zip = entry.zipEntry!
      const normalized = normalizeZipEntry(
        zip.kind === 'directory'
          ? {
              kind: 'directory',
              path: entry.path,
              ...(zip.modifiedTimeMilliseconds === undefined
                ? {}
                : { modifiedTimeMilliseconds: zip.modifiedTimeMilliseconds }),
            }
          : {
              kind: 'file',
              path: entry.path,
              exactSize: entry.size,
              ...(zip.modifiedTimeMilliseconds === undefined
                ? {}
                : { modifiedTimeMilliseconds: zip.modifiedTimeMilliseconds }),
            },
      )
      const plan = planZipEntry(normalized, zip.localHeaderOffset)
      const record = encodeZipCentralDirectoryRecord(plan, zip.crc32 ?? 0)
      await this.#io.writeAt(offset, record)
      const len = BigInt(record.byteLength)
      offset = checkedZipAdd(offset, len)
      centralBytes = checkedZipAdd(centralBytes, len)
    }

    const endBase = {
      entryCount: BigInt(ordered.length),
      centralDirectoryOffset: centralStart,
      centralDirectoryBytes: centralBytes,
    }
    const ends = encodeZipEndRecords({ ...endBase, zip64EndRequired: requiresZip64End(endBase) })
    for (const part of [ends.zip64End, ends.zip64Locator, ends.classicEnd]) {
      if (part === undefined) continue
      await this.#io.writeAt(offset, part)
      offset = checkedZipAdd(offset, BigInt(part.byteLength))
    }
    await this.#io.truncate(offset)
    await this.#io.flush()
    return offset
  }
}

/**
 * Builds a valid partial ZIP Blob from completed entries inside a paused OPFS progressive ZIP
 * without modifying the underlying resumable OPFS file.
 */
export function buildPartialZipBlob(
  sourceFile: File,
  files: readonly ReceiveFileRecord[],
): Blob {
  const completed = files
    .filter(entry => entry.status === 'completed' && entry.zipEntry !== undefined)
    .sort((left, right) => left.zipEntry!.sequence - right.zipEntry!.sequence)

  const parts: BlobPart[] = []
  const centralRecords: Uint8Array<ArrayBuffer>[] = []
  let offset = 0n
  let centralBytes = 0n

  for (const entry of completed) {
    const zip = entry.zipEntry!
    const normalized = normalizeZipEntry(
      zip.kind === 'directory'
        ? {
            kind: 'directory',
            path: entry.path,
            ...(zip.modifiedTimeMilliseconds === undefined
              ? {}
              : { modifiedTimeMilliseconds: zip.modifiedTimeMilliseconds }),
          }
        : {
            kind: 'file',
            path: entry.path,
            exactSize: entry.size,
            ...(zip.modifiedTimeMilliseconds === undefined
              ? {}
              : { modifiedTimeMilliseconds: zip.modifiedTimeMilliseconds }),
          },
    )
    const plan = planZipEntry(normalized, offset)
    const header = encodeZipLocalHeader(plan)
    parts.push(header)
    if (zip.kind === 'file' && entry.size > 0n) {
      const start = bigintToSafeNumber(zip.payloadOffset, 'partial ZIP slice start')
      const end = bigintToSafeNumber(zip.payloadOffset + entry.size, 'partial ZIP slice end')
      parts.push(sourceFile.slice(start, end))
    }
    const done = completeZipMember(plan, entry.size, zip.crc32 ?? 0)
    parts.push(done.descriptor)
    centralRecords.push(done.centralRecord)
    offset = checkedZipAdd(offset, plan.entryStreamBytes)
    centralBytes = checkedZipAdd(centralBytes, BigInt(done.centralRecord.byteLength))
  }

  for (const record of centralRecords) parts.push(record)
  const endBase = {
    entryCount: BigInt(completed.length),
    centralDirectoryOffset: offset,
    centralDirectoryBytes: centralBytes,
  }
  const ends = encodeZipEndRecords({ ...endBase, zip64EndRequired: requiresZip64End(endBase) })
  if (ends.zip64End !== undefined) parts.push(ends.zip64End)
  if (ends.zip64Locator !== undefined) parts.push(ends.zip64Locator)
  parts.push(ends.classicEnd)
  return new Blob(parts, { type: 'application/zip' })
}
