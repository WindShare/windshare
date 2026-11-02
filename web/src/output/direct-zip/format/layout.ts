import {
  checkedZipAdd,
  compareNormalizedZipEntries,
  encodeZipCentralDirectoryRecordWithTrailingExtraFields,
  encodeZipDataDescriptor,
  encodeZipLocalHeaderWithTrailingExtraFields,
  normalizeZipEntry,
  planZipEntry,
  requireZipUint64,
  sameZipEntryPlan,
  type NormalizedZipEntry,
  type ZipEntryPlanV1,
  type ZipEntrySpec,
} from '../../zip-layout/policy'
import {
  encodeDirectZipOwnershipExtraFieldV1,
  equalDirectZipOwnershipMarkersV1,
  snapshotDirectZipOwnershipMarkerV1,
  type DirectZipOwnershipMarkerInputV1,
  type DirectZipOwnershipMarkerV1,
} from './ownership-extra'
import { requireDirectZipFsaOffset } from './offset'

export const DIRECT_ZIP_ENTRY_LAYOUT_VERSION = 2 as const

const NO_EXTRA_FIELDS = new Uint8Array(0)

export interface DirectZipEntryPlanV2 {
  readonly version: typeof DIRECT_ZIP_ENTRY_LAYOUT_VERSION
  readonly ordinal: bigint
  readonly zipEntry: ZipEntryPlanV1
  readonly ownershipMarker?: DirectZipOwnershipMarkerV1
  readonly localHeaderBytes: bigint
  readonly descriptorBytes: bigint
  readonly entryStreamBytes: bigint
  readonly centralRecordBytes: bigint
}

export interface PlanDirectZipEntryInputV2 {
  readonly ordinal: bigint
  readonly localHeaderOffset: bigint
  readonly entry: ZipEntrySpec
  readonly ownershipMarker?: DirectZipOwnershipMarkerInputV1
}

export function compareDirectZipEntriesV2(left: ZipEntrySpec, right: ZipEntrySpec): number {
  return compareNormalizedZipEntries(normalizeZipEntry(left), normalizeZipEntry(right))
}

export function compareDirectZipEntryPlansV2(
  left: DirectZipEntryPlanV2,
  right: DirectZipEntryPlanV2,
): number {
  return compareNormalizedZipEntries(left.zipEntry, right.zipEntry)
}

export interface DirectZipDosDateTimeV2 {
  readonly dosTime: number
  readonly dosDate: number
}

export function planDirectZipEntryV2(
  input: PlanDirectZipEntryInputV2,
): DirectZipEntryPlanV2 {
  if (input === null || typeof input !== 'object') throw new TypeError('direct ZIP entry input is invalid')
  return planNormalizedDirectZipEntry(input, normalizeZipEntry(input.entry))
}

/**
 * Plans an entry whose DOS date/time fields are already fixed (a committed local header or a
 * persisted plan). They are kept verbatim: re-deriving them from the source instant would
 * depend on the current time zone, which may differ from the one that wrote them.
 */
export function planDirectZipEntryWithDosDateTimeV2(
  input: PlanDirectZipEntryInputV2,
  dateTime: DirectZipDosDateTimeV2,
): DirectZipEntryPlanV2 {
  if (input === null || typeof input !== 'object') throw new TypeError('direct ZIP entry input is invalid')
  if (dateTime === null || typeof dateTime !== 'object') throw new TypeError('ZIP DOS timestamp is invalid')
  return planNormalizedDirectZipEntry(input, Object.freeze({
    ...normalizeZipEntry(input.entry),
    dosTime: dateTime.dosTime,
    dosDate: dateTime.dosDate,
  }))
}

function planNormalizedDirectZipEntry(
  input: PlanDirectZipEntryInputV2,
  normalized: NormalizedZipEntry,
): DirectZipEntryPlanV2 {
  requireZipUint64(input.ordinal, 'direct ZIP entry ordinal')
  requireDirectZipFsaOffset(input.localHeaderOffset, 'direct ZIP local-header offset')
  const isOwnershipEntry = input.ordinal === 0n
  if (isOwnershipEntry) {
    if (input.localHeaderOffset !== 0n || normalized.kind !== 'directory' ||
        normalized.path.length !== 1 || input.ownershipMarker === undefined) {
      throw new TypeError('direct ZIP must begin with its marker-bearing result-root directory')
    }
  } else if (input.localHeaderOffset === 0n || input.ownershipMarker !== undefined) {
    throw new TypeError('direct ZIP ownership marker is legal only on entry zero')
  }

  const zipEntry = planZipEntry(normalized, input.localHeaderOffset)
  const ownershipMarker = input.ownershipMarker === undefined
    ? undefined
    : snapshotDirectZipOwnershipMarkerV1(input.ownershipMarker)
  const ownershipExtraBytes = ownershipMarker === undefined
    ? 0n
    : BigInt(encodeDirectZipOwnershipExtraFieldV1(ownershipMarker).byteLength)
  const localHeaderBytes = checkedZipAdd(zipEntry.localHeaderBytes, ownershipExtraBytes)
  const entryStreamBytes = checkedZipAdd(zipEntry.entryStreamBytes, ownershipExtraBytes)
  const centralRecordBytes = checkedZipAdd(zipEntry.centralRecordBytes, ownershipExtraBytes)
  requireDirectZipFsaOffset(
    checkedZipAdd(input.localHeaderOffset, entryStreamBytes),
    'direct ZIP member end offset',
  )
  return Object.freeze({
    version: DIRECT_ZIP_ENTRY_LAYOUT_VERSION,
    ordinal: input.ordinal,
    zipEntry,
    ...(ownershipMarker === undefined ? {} : { ownershipMarker }),
    localHeaderBytes,
    descriptorBytes: zipEntry.descriptorBytes,
    entryStreamBytes,
    centralRecordBytes,
  })
}

export function validateDirectZipEntryPlanV2(
  candidate: DirectZipEntryPlanV2,
): DirectZipEntryPlanV2 {
  if (candidate === null || typeof candidate !== 'object' ||
      candidate.version !== DIRECT_ZIP_ENTRY_LAYOUT_VERSION) {
    throw new TypeError('direct ZIP entry plan version is invalid')
  }
  // planZipEntry checks the DOS fields' structure; their values are not re-derived.
  const rebuilt = planDirectZipEntryWithDosDateTimeV2({
    ordinal: candidate.ordinal,
    localHeaderOffset: candidate.zipEntry.localHeaderOffset,
    entry: candidate.zipEntry.kind === 'directory'
      ? { kind: 'directory', path: candidate.zipEntry.path }
      : { kind: 'file', path: candidate.zipEntry.path, exactSize: candidate.zipEntry.exactSize },
    ...(candidate.ownershipMarker === undefined
      ? {}
      : { ownershipMarker: candidate.ownershipMarker }),
  }, candidate.zipEntry)
  if (!sameZipEntryPlan(candidate.zipEntry, rebuilt.zipEntry) ||
      candidate.localHeaderBytes !== rebuilt.localHeaderBytes ||
      candidate.descriptorBytes !== rebuilt.descriptorBytes ||
      candidate.entryStreamBytes !== rebuilt.entryStreamBytes ||
      candidate.centralRecordBytes !== rebuilt.centralRecordBytes ||
      !sameOptionalMarker(candidate.ownershipMarker, rebuilt.ownershipMarker)) {
    throw new TypeError('direct ZIP entry plan is not canonical')
  }
  return rebuilt
}

export function encodeDirectZipLocalHeaderV2(
  plan: DirectZipEntryPlanV2,
): Uint8Array<ArrayBuffer> {
  const canonical = validateDirectZipEntryPlanV2(plan)
  const ownershipExtra = canonical.ownershipMarker === undefined
    ? NO_EXTRA_FIELDS
    : encodeDirectZipOwnershipExtraFieldV1(canonical.ownershipMarker)
  return encodeZipLocalHeaderWithTrailingExtraFields(canonical.zipEntry, ownershipExtra)
}

export function encodeDirectZipDataDescriptorV2(
  plan: DirectZipEntryPlanV2,
  crc32: number,
): Uint8Array<ArrayBuffer> {
  const canonical = validateDirectZipEntryPlanV2(plan)
  return encodeZipDataDescriptor(canonical.zipEntry, crc32)
}

export function encodeDirectZipCentralDirectoryRecordV2(
  plan: DirectZipEntryPlanV2,
  crc32: number,
): Uint8Array<ArrayBuffer> {
  const canonical = validateDirectZipEntryPlanV2(plan)
  const ownershipExtra = canonical.ownershipMarker === undefined
    ? NO_EXTRA_FIELDS
    : encodeDirectZipOwnershipExtraFieldV1(canonical.ownershipMarker)
  return encodeZipCentralDirectoryRecordWithTrailingExtraFields(
    canonical.zipEntry,
    crc32,
    ownershipExtra,
  )
}

export function requireDirectZipCanonicalSuccessorV2(
  previous: DirectZipEntryPlanV2,
  next: DirectZipEntryPlanV2,
): void {
  const left = validateDirectZipEntryPlanV2(previous)
  const right = validateDirectZipEntryPlanV2(next)
  if (right.ordinal !== left.ordinal + 1n ||
      right.zipEntry.localHeaderOffset !== left.zipEntry.localHeaderOffset + left.entryStreamBytes ||
      compareNormalizedZipEntries(left.zipEntry, right.zipEntry) >= 0) {
    throw new TypeError('direct ZIP entry is not the canonical successor')
  }
}

function sameOptionalMarker(
  left: DirectZipOwnershipMarkerV1 | undefined,
  right: DirectZipOwnershipMarkerV1 | undefined,
): boolean {
  if (left === undefined || right === undefined) return left === right
  return equalDirectZipOwnershipMarkersV1(left, right)
}
