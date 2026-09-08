import type { NativeObjectWrite } from '../origin-private/native-object/write-batch'
import type { TaskEntry } from '../origin-private/task-checkpoint/model'
import {
  checkedZipAdd, encodeZipCentralDirectoryRecord, encodeZipDataDescriptor, encodeZipLocalHeader,
} from '../zip-layout/policy'
import { completeZipEntryCrc } from './crc-ranges'

const MAX_BATCH_ENTRIES = 128
const MAX_BATCH_BYTES = 256 * 1024

export interface ZipFinalizationBatch {
  readonly writes: readonly NativeObjectWrite[]
  readonly nextEntry: bigint
  readonly committedLength: bigint
}

/** Bound both recovery work and metadata memory independently of archive size or path length. */
export async function* zipFinalizationBatches(
  entries: AsyncIterable<TaskEntry>,
  centralDirectoryOffset: bigint,
): AsyncGenerator<ZipFinalizationBatch> {
  let writes: NativeObjectWrite[] = []
  let centralRecords: Uint8Array[] = []
  let batchBytes = 0
  let centralBytes = 0
  let nextEntry = 0n
  let offset = centralDirectoryOffset
  for await (const entry of entries) {
    const plan = entry.zipPlan!
    const layout = entry.zipLayout!
    const crc = completeZipEntryCrc(entry.ranges, layout.exactSize)
    if (entry.revisionFailure !== undefined || crc === undefined) {
      throw new Error('ZIP finalization requires complete committed entries')
    }
    const header = encodeZipLocalHeader(plan)
    const descriptor = encodeZipDataDescriptor(plan, crc)
    const central = encodeZipCentralDirectoryRecord(plan, crc)
    const entryBytes = header.byteLength + descriptor.byteLength + central.byteLength
    if (entryBytes > MAX_BATCH_BYTES) throw new RangeError('ZIP entry metadata exceeds the batch bound')
    if (centralRecords.length > 0 &&
        (centralRecords.length === MAX_BATCH_ENTRIES || batchBytes + entryBytes > MAX_BATCH_BYTES)) {
      yield sealBatch(writes, centralRecords, centralBytes, offset, nextEntry)
      offset = checkedZipAdd(offset, BigInt(centralBytes))
      writes = []
      centralRecords = []
      batchBytes = 0
      centralBytes = 0
    }
    writes.push({ offset: layout.localHeaderOffset, bytes: header })
    if (descriptor.byteLength > 0) writes.push({ offset: layout.descriptorOffset, bytes: descriptor })
    centralRecords.push(central)
    centralBytes += central.byteLength
    batchBytes += entryBytes
    nextEntry = layout.sequence + 1n
  }
  if (centralRecords.length > 0) yield sealBatch(writes, centralRecords, centralBytes, offset, nextEntry)
}

function sealBatch(
  writes: readonly NativeObjectWrite[],
  records: readonly Uint8Array[],
  centralBytes: number,
  offset: bigint,
  nextEntry: bigint,
): ZipFinalizationBatch {
  // Central-directory records are contiguous; merge only this bounded page, never payloads.
  const central = new Uint8Array(centralBytes)
  let cursor = 0
  for (const record of records) {
    central.set(record, cursor)
    cursor += record.byteLength
  }
  return {
    writes: [...writes, { offset, bytes: central }],
    nextEntry,
    committedLength: checkedZipAdd(offset, BigInt(centralBytes)),
  }
}
