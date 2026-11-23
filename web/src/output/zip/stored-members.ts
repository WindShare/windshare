import { emitOutputTrace, type OutputTraceSource } from '../diagnostics'
import type { ObjectCheckpointCoordinator } from '../native-object/coordinator'
import type { ReceiveStore } from '../store/database'
import { maxRangeEnd, toRangeTuples, type ReceiveFileRecord, type ZipEntryLayoutRecord } from '../store/types'
import { encodeZipDataDescriptor, encodeZipLocalHeader, normalizeZipEntry, planZipEntry } from './policy'

export type StoredZipMember = ReceiveFileRecord & { readonly zipEntry: ZipEntryLayoutRecord }

export function hasZipLayout(row: ReceiveFileRecord): row is StoredZipMember {
  return row.zipEntry !== undefined
}

export function completedZipMembers(rows: Iterable<ReceiveFileRecord>): StoredZipMember[] {
  const completed: StoredZipMember[] = []
  for (const row of rows) {
    if (row.status === 'completed' && hasZipLayout(row)) completed.push(row)
  }
  return completed.sort((left, right) => left.zipEntry.sequence - right.zipEntry.sequence)
}

export function planStoredZipMember(row: StoredZipMember, offset: bigint) {
  const { kind, modifiedTimeMilliseconds } = row.zipEntry
  const attributes = { path: row.path, ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }) }
  return planZipEntry(normalizeZipEntry(kind === 'file'
    ? { ...attributes, kind, exactSize: row.size }
    : { ...attributes, kind }), offset)
}

export function hasZipPayload(row: StoredZipMember, physicalSize: bigint): boolean {
  // Headers and descriptors can be regenerated for an empty member without any source bytes.
  return row.size === 0n || row.zipEntry.payloadOffset + row.size <= physicalSize
}

export function survivingZipCoverage(row: StoredZipMember, physicalSize: bigint): StoredZipMember {
  const layout = row.zipEntry
  const extent = maxRangeEnd(row.completedRanges)
  const lostCoverage = extent > 0n && layout.payloadOffset + extent > physicalSize
  if (row.status === 'completed' ? hasZipPayload(row, physicalSize) : !lostCoverage) return row
  // A checksum spanning EOF cannot authenticate a shorter prefix. Other complete CRC runs survive.
  const crcRanges = layout.crcRanges.filter(range => layout.payloadOffset + range.end <= physicalSize)
  const zipEntry = { ...layout, crcRanges }
  delete zipEntry.crc32
  return { ...row, status: 'receiving', completedRanges: toRangeTuples(crcRanges), zipEntry }
}

export async function recoverZipMembers(
  rows: readonly ReceiveFileRecord[],
  physicalSize: bigint,
  io: ObjectCheckpointCoordinator,
  store: ReceiveStore,
  trace?: OutputTraceSource,
): Promise<readonly StoredZipMember[]> {
  const valid: StoredZipMember[] = []
  const damaged: StoredZipMember[] = []
  for (const row of rows) {
    if (!hasZipLayout(row)) continue
    const layout = row.zipEntry
    const required = row.status === 'completed' ? layout.endOffset : layout.payloadOffset + maxRangeEnd(row.completedRanges)
    if (required > physicalSize) damaged.push(row)
    else valid.push(row)
  }
  // An extending write masks the original EOF. Persist each loss and flush its repaired structure
  // before reaching any higher slot, so a crash during recovery can restart from the new EOF.
  if (damaged.length === 0) return valid
  emitOutputTrace(trace, 'reopen', {
    backend: 'origin_private', task_id: damaged[0]!.taskId, transition: 'repairing_zip_members',
    affected_members: damaged.length, physical_bytes: String(physicalSize),
  })
  damaged.sort((left, right) => Number(left.zipEntry.localHeaderOffset - right.zipEntry.localHeaderOffset))
  for (const row of damaged) {
    const recovered = survivingZipCoverage(row, physicalSize)
    if (recovered !== row) await store.putFile(recovered)
    await repairZipStructure(recovered, physicalSize, io)
    valid.push(recovered)
  }
  return valid
}

async function repairZipStructure(row: StoredZipMember, physicalSize: bigint, io: ObjectCheckpointCoordinator): Promise<void> {
  const layout = row.zipEntry
  const missingHeader = physicalSize < layout.payloadOffset
  const missingDescriptor = row.status === 'completed' && physicalSize < layout.endOffset
  if (!missingHeader && !missingDescriptor) return
  const plan = planStoredZipMember(row, layout.localHeaderOffset)
  if (missingHeader) await io.writeAt(layout.localHeaderOffset, encodeZipLocalHeader(plan))
  if (missingDescriptor) await io.writeAt(layout.descriptorOffset, encodeZipDataDescriptor(plan, layout.crc32!))
  await io.flush()
}
