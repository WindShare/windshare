import { snapshotPortableCatalogPath } from '../../catalog/path-policy'

export const ZIP_ENCODING_POLICY_VERSION = 1 as const
export const ZIP_ENCODING_POLICY = 'windshare/zip-encoding/v1-store-data-descriptor' as const
export const MAX_ZIP_SPOOL_ENTRIES = 1_000_000
export const MAX_ZIP_SPOOL_BYTES = 268_435_456n

export const ZIP_UINT16_SENTINEL = 0xffffn
export const ZIP_UINT32_SENTINEL = 0xffff_ffffn
export const ZIP_UINT64_MAXIMUM = (1n << 64n) - 1n
export const ZIP_CRC32_INITIAL_ACCUMULATOR = 0xffff_ffff

const ZIP_LOCAL_FILE_HEADER = 0x0403_4b50
const ZIP_DATA_DESCRIPTOR = 0x0807_4b50
const ZIP_CENTRAL_DIRECTORY_HEADER = 0x0201_4b50
const ZIP64_END_OF_CENTRAL_DIRECTORY = 0x0606_4b50
const ZIP64_END_OF_CENTRAL_DIRECTORY_LOCATOR = 0x0706_4b50
const ZIP_END_OF_CENTRAL_DIRECTORY = 0x0605_4b50
const ZIP64_EXTRA_FIELD = 0x0001
const ZIP_CLASSIC_VERSION = 20
const ZIP64_VERSION = 45
const ZIP_UTF8_AND_DATA_DESCRIPTOR_FLAGS = 0x0808
const ZIP_STORE_METHOD = 0
const ZIP_DIRECTORY_ATTRIBUTE = 0x10
const ZIP_LOCAL_HEADER_FIXED_BYTES = 30n
const ZIP_DATA_DESCRIPTOR_ZIP32_BYTES = 16n
const ZIP_DATA_DESCRIPTOR_ZIP64_BYTES = 24n
const ZIP_CENTRAL_HEADER_FIXED_BYTES = 46n
const ZIP64_EXTRA_HEADER_BYTES = 4n
const ZIP64_EXTRA_VALUE_BYTES = 8n
export const ZIP_CLASSIC_END_BYTES = 22n
export const ZIP64_END_BYTES = 76n
const ZIP_MINIMUM_DATE_MILLISECONDS = BigInt(Date.UTC(1980, 0, 1, 0, 0, 0))
const ZIP_MAXIMUM_DATE_MILLISECONDS = BigInt(Date.UTC(2107, 11, 31, 23, 59, 58))
const TEXT_ENCODER = new TextEncoder()
const EMPTY_ZIP_EXTRA_FIELDS = new Uint8Array(0)

export type ZipEntryKind = 'directory' | 'file'

export interface ZipDirectorySpec {
  readonly kind: 'directory'
  readonly path: readonly string[]
  readonly modifiedTimeMilliseconds?: bigint
}

export interface ZipFileSpec {
  readonly kind: 'file'
  readonly path: readonly string[]
  readonly exactSize: bigint
  readonly modifiedTimeMilliseconds?: bigint
}

export type ZipEntrySpec = ZipDirectorySpec | ZipFileSpec

export interface NormalizedZipEntry {
  readonly kind: ZipEntryKind
  readonly path: readonly string[]
  readonly artifactPath: string
  readonly artifactPathBytes: readonly number[]
  readonly nameBytes: readonly number[]
  readonly exactSize: bigint
  readonly dosTime: number
  readonly dosDate: number
}

export interface ZipEntryPlanV1 extends NormalizedZipEntry {
  readonly version: typeof ZIP_ENCODING_POLICY_VERSION
  readonly zip64Size: boolean
  readonly zip64Offset: boolean
  readonly versionNeeded: 20 | 45
  readonly localHeaderOffset: bigint
  readonly localExtraBytes: bigint
  readonly localHeaderBytes: bigint
  readonly descriptorBytes: bigint
  readonly entryStreamBytes: bigint
  readonly centralZip64ValueCount: 0 | 1 | 2 | 3
  readonly centralExtraBytes: bigint
  readonly centralRecordBytes: bigint
}

export interface ZipEndLayout {
  readonly entryCount: bigint
  readonly centralDirectoryOffset: bigint
  readonly centralDirectoryBytes: bigint
  readonly zip64EndRequired: boolean
}

export interface EncodedZipEndRecords {
  readonly zip64End?: Uint8Array<ArrayBuffer>
  readonly zip64Locator?: Uint8Array<ArrayBuffer>
  readonly classicEnd: Uint8Array<ArrayBuffer>
}

/** Sole policy authority for ZIP entry names, timestamps, ZIP32/ZIP64 choices, and lengths. */
export function normalizeZipEntry(spec: ZipEntrySpec): NormalizedZipEntry {
  if (spec === null || typeof spec !== 'object') throw new TypeError('ZIP entry is invalid')
  if (spec.kind !== 'directory' && spec.kind !== 'file') throw new TypeError('ZIP entry kind is invalid')
  const path = snapshotPortableCatalogPath(spec.path)
  const artifactPath = path.join('/')
  const artifactPathBytes = frozenBytes(TEXT_ENCODER.encode(artifactPath))
  const name = spec.kind === 'directory' ? `${artifactPath}/` : artifactPath
  const nameBytes = frozenBytes(TEXT_ENCODER.encode(name))
  if (nameBytes.length === 0 || nameBytes.length > Number(ZIP_UINT16_SENTINEL)) {
    throw new RangeError('ZIP entry name length is outside the ZIP format')
  }
  const exactSize = spec.kind === 'directory' ? 0n : spec.exactSize
  requireZipUint64(exactSize, 'ZIP entry size')
  const { dosTime, dosDate } = normalizeDosDateTime(spec.modifiedTimeMilliseconds)
  return Object.freeze({
    kind: spec.kind,
    path,
    artifactPath,
    artifactPathBytes,
    nameBytes,
    exactSize,
    dosTime,
    dosDate,
  })
}

export function compareNormalizedZipEntries(
  left: NormalizedZipEntry,
  right: NormalizedZipEntry,
): number {
  const pathOrder = compareUnsignedBytes(left.artifactPathBytes, right.artifactPathBytes)
  if (pathOrder !== 0) return pathOrder
  if (left.kind === right.kind) return 0
  return left.kind === 'directory' ? -1 : 1
}

export function planZipEntry(
  entry: NormalizedZipEntry,
  localHeaderOffset: bigint,
): ZipEntryPlanV1 {
  requireZipUint64(localHeaderOffset, 'ZIP local-header offset')
  requireNormalizedEntry(entry)
  const zip64Size = entry.exactSize >= ZIP_UINT32_SENTINEL
  const zip64Offset = localHeaderOffset >= ZIP_UINT32_SENTINEL
  const localExtraBytes = zip64Size ? 20n : 0n
  const localHeaderBytes = checkedZipAdd(
    ZIP_LOCAL_HEADER_FIXED_BYTES,
    BigInt(entry.nameBytes.length),
    localExtraBytes,
  )
  const descriptorBytes = zip64Size
    ? ZIP_DATA_DESCRIPTOR_ZIP64_BYTES
    : ZIP_DATA_DESCRIPTOR_ZIP32_BYTES
  const entryStreamBytes = checkedZipAdd(localHeaderBytes, entry.exactSize, descriptorBytes)
  const centralZip64ValueCount = (
    (zip64Size ? 2 : 0) + (zip64Offset ? 1 : 0)
  ) as 0 | 1 | 2 | 3
  const centralExtraBytes = centralZip64ValueCount === 0
    ? 0n
    : checkedZipAdd(
        ZIP64_EXTRA_HEADER_BYTES,
        checkedZipMultiply(ZIP64_EXTRA_VALUE_BYTES, BigInt(centralZip64ValueCount)),
      )
  const centralRecordBytes = checkedZipAdd(
    ZIP_CENTRAL_HEADER_FIXED_BYTES,
    BigInt(entry.nameBytes.length),
    centralExtraBytes,
  )
  return Object.freeze({
    version: ZIP_ENCODING_POLICY_VERSION,
    kind: entry.kind,
    path: entry.path,
    artifactPath: entry.artifactPath,
    artifactPathBytes: entry.artifactPathBytes,
    nameBytes: entry.nameBytes,
    exactSize: entry.exactSize,
    dosTime: entry.dosTime,
    dosDate: entry.dosDate,
    zip64Size,
    zip64Offset,
    versionNeeded: zip64Size || zip64Offset ? ZIP64_VERSION : ZIP_CLASSIC_VERSION,
    localHeaderOffset,
    localExtraBytes,
    localHeaderBytes,
    descriptorBytes,
    entryStreamBytes,
    centralZip64ValueCount,
    centralExtraBytes,
    centralRecordBytes,
  })
}

export function validateZipEntryPlan(
  plan: ZipEntryPlanV1,
  expectedOffset: bigint,
): ZipEntryPlanV1 {
  if (plan === null || typeof plan !== 'object' || plan.version !== ZIP_ENCODING_POLICY_VERSION) {
    throw new TypeError('ZIP entry plan version is invalid')
  }
  requireDosDateTime(plan.dosTime, plan.dosDate)
  const path = snapshotPortableCatalogPath(plan.path)
  const artifactPath = path.join('/')
  const artifactPathBytes = frozenBytes(TEXT_ENCODER.encode(artifactPath))
  const nameBytes = frozenBytes(TEXT_ENCODER.encode(
    plan.kind === 'directory' ? `${artifactPath}/` : artifactPath,
  ))
  const normalized: NormalizedZipEntry = Object.freeze({
    kind: plan.kind,
    path,
    artifactPath,
    artifactPathBytes,
    nameBytes,
    exactSize: plan.exactSize,
    dosTime: plan.dosTime,
    dosDate: plan.dosDate,
  })
  const expected = planZipEntry(normalized, expectedOffset)
  if (!sameZipEntryPlan(plan, expected)) throw new TypeError('ZIP entry plan is not canonical')
  return expected
}

export function sameZipEntryPlan(left: ZipEntryPlanV1, right: ZipEntryPlanV1): boolean {
  return left.version === right.version &&
    left.kind === right.kind &&
    equalStrings(left.path, right.path) &&
    left.artifactPath === right.artifactPath &&
    equalNumbers(left.artifactPathBytes, right.artifactPathBytes) &&
    equalNumbers(left.nameBytes, right.nameBytes) &&
    left.exactSize === right.exactSize &&
    left.dosTime === right.dosTime &&
    left.dosDate === right.dosDate &&
    left.zip64Size === right.zip64Size &&
    left.zip64Offset === right.zip64Offset &&
    left.versionNeeded === right.versionNeeded &&
    left.localHeaderOffset === right.localHeaderOffset &&
    left.localExtraBytes === right.localExtraBytes &&
    left.localHeaderBytes === right.localHeaderBytes &&
    left.descriptorBytes === right.descriptorBytes &&
    left.entryStreamBytes === right.entryStreamBytes &&
    left.centralZip64ValueCount === right.centralZip64ValueCount &&
    left.centralExtraBytes === right.centralExtraBytes &&
    left.centralRecordBytes === right.centralRecordBytes
}

export function encodeZipLocalHeader(plan: ZipEntryPlanV1): Uint8Array<ArrayBuffer> {
  return encodeZipLocalHeaderWithTrailingExtraFields(plan, EMPTY_ZIP_EXTRA_FIELDS)
}

export function encodeZipLocalHeaderWithTrailingExtraFields(
  plan: ZipEntryPlanV1,
  trailingExtraFields: Uint8Array,
): Uint8Array<ArrayBuffer> {
  validateZipEntryPlan(plan, plan.localHeaderOffset)
  const extraFields = snapshotTrailingExtraFields(
    trailingExtraFields,
    plan.localExtraBytes,
    'ZIP local header',
  )
  const encodedBytes = checkedZipAdd(plan.localHeaderBytes, BigInt(extraFields.byteLength))
  const output = new Uint8Array(toAllocationLength(encodedBytes, 'ZIP local header'))
  const view = new DataView(output.buffer)
  view.setUint32(0, ZIP_LOCAL_FILE_HEADER, true)
  view.setUint16(4, plan.versionNeeded, true)
  view.setUint16(6, ZIP_UTF8_AND_DATA_DESCRIPTOR_FLAGS, true)
  view.setUint16(8, ZIP_STORE_METHOD, true)
  view.setUint16(10, plan.dosTime, true)
  view.setUint16(12, plan.dosDate, true)
  view.setUint32(14, 0, true)
  view.setUint32(18, plan.zip64Size ? Number(ZIP_UINT32_SENTINEL) : 0, true)
  view.setUint32(22, plan.zip64Size ? Number(ZIP_UINT32_SENTINEL) : 0, true)
  view.setUint16(26, plan.nameBytes.length, true)
  view.setUint16(28, Number(plan.localExtraBytes) + extraFields.byteLength, true)
  output.set(plan.nameBytes, Number(ZIP_LOCAL_HEADER_FIXED_BYTES))
  const extraOffset = Number(ZIP_LOCAL_HEADER_FIXED_BYTES) + plan.nameBytes.length
  if (plan.zip64Size) {
    view.setUint16(extraOffset, ZIP64_EXTRA_FIELD, true)
    view.setUint16(extraOffset + 2, 16, true)
    view.setBigUint64(extraOffset + 4, plan.exactSize, true)
    view.setBigUint64(extraOffset + 12, plan.exactSize, true)
  }
  output.set(extraFields, extraOffset + Number(plan.localExtraBytes))
  assertEncodedLength(output, encodedBytes, 'ZIP local header')
  return output
}

export function encodeZipDataDescriptor(
  plan: ZipEntryPlanV1,
  crc32: number,
): Uint8Array<ArrayBuffer> {
  validateZipEntryPlan(plan, plan.localHeaderOffset)
  requireUint32(crc32, 'ZIP CRC-32')
  const output = new Uint8Array(toAllocationLength(plan.descriptorBytes, 'ZIP data descriptor'))
  const view = new DataView(output.buffer)
  view.setUint32(0, ZIP_DATA_DESCRIPTOR, true)
  view.setUint32(4, crc32, true)
  if (plan.zip64Size) {
    view.setBigUint64(8, plan.exactSize, true)
    view.setBigUint64(16, plan.exactSize, true)
  } else {
    const exactSize = Number(plan.exactSize)
    view.setUint32(8, exactSize, true)
    view.setUint32(12, exactSize, true)
  }
  assertEncodedLength(output, plan.descriptorBytes, 'ZIP data descriptor')
  return output
}

export function encodeZipCentralDirectoryRecord(
  plan: ZipEntryPlanV1,
  crc32: number,
): Uint8Array<ArrayBuffer> {
  return encodeZipCentralDirectoryRecordWithTrailingExtraFields(
    plan,
    crc32,
    EMPTY_ZIP_EXTRA_FIELDS,
  )
}

export function encodeZipCentralDirectoryRecordWithTrailingExtraFields(
  plan: ZipEntryPlanV1,
  crc32: number,
  trailingExtraFields: Uint8Array,
): Uint8Array<ArrayBuffer> {
  validateZipEntryPlan(plan, plan.localHeaderOffset)
  requireUint32(crc32, 'ZIP CRC-32')
  const extraFields = snapshotTrailingExtraFields(
    trailingExtraFields,
    plan.centralExtraBytes,
    'ZIP central record',
  )
  const encodedBytes = checkedZipAdd(plan.centralRecordBytes, BigInt(extraFields.byteLength))
  const output = new Uint8Array(toAllocationLength(encodedBytes, 'ZIP central record'))
  const view = new DataView(output.buffer)
  view.setUint32(0, ZIP_CENTRAL_DIRECTORY_HEADER, true)
  view.setUint16(4, ZIP64_VERSION, true)
  view.setUint16(6, plan.versionNeeded, true)
  view.setUint16(8, ZIP_UTF8_AND_DATA_DESCRIPTOR_FLAGS, true)
  view.setUint16(10, ZIP_STORE_METHOD, true)
  view.setUint16(12, plan.dosTime, true)
  view.setUint16(14, plan.dosDate, true)
  view.setUint32(16, crc32, true)
  view.setUint32(20, plan.zip64Size ? Number(ZIP_UINT32_SENTINEL) : Number(plan.exactSize), true)
  view.setUint32(24, plan.zip64Size ? Number(ZIP_UINT32_SENTINEL) : Number(plan.exactSize), true)
  view.setUint16(28, plan.nameBytes.length, true)
  view.setUint16(30, Number(plan.centralExtraBytes) + extraFields.byteLength, true)
  view.setUint16(32, 0, true)
  view.setUint16(34, 0, true)
  view.setUint16(36, 0, true)
  view.setUint32(38, plan.kind === 'directory' ? ZIP_DIRECTORY_ATTRIBUTE : 0, true)
  view.setUint32(
    42,
    plan.zip64Offset ? Number(ZIP_UINT32_SENTINEL) : Number(plan.localHeaderOffset),
    true,
  )
  output.set(plan.nameBytes, Number(ZIP_CENTRAL_HEADER_FIXED_BYTES))
  const extraOffset = Number(ZIP_CENTRAL_HEADER_FIXED_BYTES) + plan.nameBytes.length
  if (plan.centralZip64ValueCount > 0) {
    view.setUint16(extraOffset, ZIP64_EXTRA_FIELD, true)
    view.setUint16(extraOffset + 2, plan.centralZip64ValueCount * Number(ZIP64_EXTRA_VALUE_BYTES), true)
    let zip64ValueOffset = extraOffset + Number(ZIP64_EXTRA_HEADER_BYTES)
    if (plan.zip64Size) {
      view.setBigUint64(zip64ValueOffset, plan.exactSize, true)
      view.setBigUint64(zip64ValueOffset + 8, plan.exactSize, true)
      zip64ValueOffset += 16
    }
    if (plan.zip64Offset) view.setBigUint64(zip64ValueOffset, plan.localHeaderOffset, true)
  }
  output.set(extraFields, extraOffset + Number(plan.centralExtraBytes))
  assertEncodedLength(output, encodedBytes, 'ZIP central record')
  return output
}

export function encodeZipEndRecords(layout: ZipEndLayout): EncodedZipEndRecords {
  requireZipUint64(layout.entryCount, 'ZIP entry count')
  requireZipUint64(layout.centralDirectoryOffset, 'ZIP central-directory offset')
  requireZipUint64(layout.centralDirectoryBytes, 'ZIP central-directory size')
  if (layout.zip64EndRequired !== requiresZip64End(layout)) {
    throw new TypeError('ZIP end-record choice does not match its layout')
  }
  const classicEnd = new Uint8Array(Number(ZIP_CLASSIC_END_BYTES))
  const classicView = new DataView(classicEnd.buffer)
  classicView.setUint32(0, ZIP_END_OF_CENTRAL_DIRECTORY, true)
  classicView.setUint16(4, 0, true)
  classicView.setUint16(6, 0, true)
  const classicCount = layout.entryCount >= ZIP_UINT16_SENTINEL
    ? Number(ZIP_UINT16_SENTINEL)
    : Number(layout.entryCount)
  classicView.setUint16(8, classicCount, true)
  classicView.setUint16(10, classicCount, true)
  classicView.setUint32(
    12,
    layout.centralDirectoryBytes >= ZIP_UINT32_SENTINEL
      ? Number(ZIP_UINT32_SENTINEL)
      : Number(layout.centralDirectoryBytes),
    true,
  )
  classicView.setUint32(
    16,
    layout.centralDirectoryOffset >= ZIP_UINT32_SENTINEL
      ? Number(ZIP_UINT32_SENTINEL)
      : Number(layout.centralDirectoryOffset),
    true,
  )
  classicView.setUint16(20, 0, true)
  if (!layout.zip64EndRequired) return Object.freeze({ classicEnd })

  const zip64End = new Uint8Array(56)
  const endView = new DataView(zip64End.buffer)
  endView.setUint32(0, ZIP64_END_OF_CENTRAL_DIRECTORY, true)
  endView.setBigUint64(4, 44n, true)
  endView.setUint16(12, ZIP64_VERSION, true)
  endView.setUint16(14, ZIP64_VERSION, true)
  endView.setUint32(16, 0, true)
  endView.setUint32(20, 0, true)
  endView.setBigUint64(24, layout.entryCount, true)
  endView.setBigUint64(32, layout.entryCount, true)
  endView.setBigUint64(40, layout.centralDirectoryBytes, true)
  endView.setBigUint64(48, layout.centralDirectoryOffset, true)

  const zip64Locator = new Uint8Array(20)
  const locatorView = new DataView(zip64Locator.buffer)
  locatorView.setUint32(0, ZIP64_END_OF_CENTRAL_DIRECTORY_LOCATOR, true)
  locatorView.setUint32(4, 0, true)
  locatorView.setBigUint64(8, checkedZipAdd(
    layout.centralDirectoryOffset,
    layout.centralDirectoryBytes,
  ), true)
  locatorView.setUint32(16, 1, true)
  return Object.freeze({ zip64End, zip64Locator, classicEnd })
}

export function requiresZip64End(layout: Omit<ZipEndLayout, 'zip64EndRequired'>): boolean {
  return layout.entryCount >= ZIP_UINT16_SENTINEL ||
    layout.centralDirectoryBytes >= ZIP_UINT32_SENTINEL ||
    layout.centralDirectoryOffset >= ZIP_UINT32_SENTINEL
}

export function requireZipSpoolBudget(entryCount: bigint, centralBytes: bigint): void {
  requireZipUint64(entryCount, 'ZIP entry count')
  requireZipUint64(centralBytes, 'ZIP central-directory size')
  if (entryCount > BigInt(MAX_ZIP_SPOOL_ENTRIES)) {
    throw new RangeError('ZIP central-directory entry budget exceeded')
  }
  if (centralBytes > MAX_ZIP_SPOOL_BYTES) {
    throw new RangeError('ZIP central-directory byte budget exceeded')
  }
}

export function checkedZipAdd(...values: readonly bigint[]): bigint {
  let total = 0n
  for (const value of values) {
    requireZipUint64(value, 'ZIP addition operand')
    if (total > ZIP_UINT64_MAXIMUM - value) throw new RangeError('ZIP uint64 addition overflow')
    total += value
  }
  return total
}

export function checkedZipMultiply(left: bigint, right: bigint): bigint {
  requireZipUint64(left, 'ZIP multiplication operand')
  requireZipUint64(right, 'ZIP multiplication operand')
  if (left !== 0n && right > ZIP_UINT64_MAXIMUM / left) {
    throw new RangeError('ZIP uint64 multiplication overflow')
  }
  return left * right
}

export function requireZipUint64(value: bigint, label: string): void {
  if (typeof value !== 'bigint' || value < 0n || value > ZIP_UINT64_MAXIMUM) {
    throw new RangeError(`${label} exceeds ZIP uint64`)
  }
}

export class ZipCrc32 {
  #value: number

  constructor(unfinalizedAccumulator = ZIP_CRC32_INITIAL_ACCUMULATOR) {
    requireUint32(unfinalizedAccumulator, 'ZIP CRC-32 accumulator')
    this.#value = unfinalizedAccumulator >>> 0
  }

  update(bytes: Uint8Array): void {
    let current = this.#value >>> 0
    for (const byte of bytes) {
      current = CRC32_TABLE[(current ^ byte) & 0xff]! ^ (current >>> 8)
    }
    this.#value = current >>> 0
  }

  digest(): number {
    return (this.#value ^ 0xffff_ffff) >>> 0
  }

  snapshot(): number {
    return this.#value >>> 0
  }
}

export interface ZipEncodingPolicyV1 {
  readonly version: typeof ZIP_ENCODING_POLICY_VERSION
  readonly discriminant: typeof ZIP_ENCODING_POLICY
  readonly normalizeEntry: typeof normalizeZipEntry
  readonly compareEntries: typeof compareNormalizedZipEntries
  readonly planEntry: typeof planZipEntry
  readonly validateEntryPlan: typeof validateZipEntryPlan
  readonly sameEntryPlan: typeof sameZipEntryPlan
  readonly encodeLocalHeader: typeof encodeZipLocalHeader
  readonly encodeDataDescriptor: typeof encodeZipDataDescriptor
  readonly encodeCentralRecord: typeof encodeZipCentralDirectoryRecord
  readonly encodeEndRecords: typeof encodeZipEndRecords
  readonly requiresZip64End: typeof requiresZip64End
  readonly requireSpoolBudget: typeof requireZipSpoolBudget
}

export const ZIP_ENCODING_POLICY_V1: ZipEncodingPolicyV1 = Object.freeze({
  version: ZIP_ENCODING_POLICY_VERSION,
  discriminant: ZIP_ENCODING_POLICY,
  normalizeEntry: normalizeZipEntry,
  compareEntries: compareNormalizedZipEntries,
  planEntry: planZipEntry,
  validateEntryPlan: validateZipEntryPlan,
  sameEntryPlan: sameZipEntryPlan,
  encodeLocalHeader: encodeZipLocalHeader,
  encodeDataDescriptor: encodeZipDataDescriptor,
  encodeCentralRecord: encodeZipCentralDirectoryRecord,
  encodeEndRecords: encodeZipEndRecords,
  requiresZip64End,
  requireSpoolBudget: requireZipSpoolBudget,
})

function normalizeDosDateTime(
  modifiedTimeMilliseconds: bigint | undefined,
): { readonly dosTime: number; readonly dosDate: number } {
  if (modifiedTimeMilliseconds !== undefined && typeof modifiedTimeMilliseconds !== 'bigint') {
    throw new TypeError('ZIP modified time must be an integer millisecond value')
  }
  let clamped = modifiedTimeMilliseconds ?? ZIP_MINIMUM_DATE_MILLISECONDS
  if (clamped < ZIP_MINIMUM_DATE_MILLISECONDS) clamped = ZIP_MINIMUM_DATE_MILLISECONDS
  if (clamped > ZIP_MAXIMUM_DATE_MILLISECONDS) clamped = ZIP_MAXIMUM_DATE_MILLISECONDS
  const value = new Date(Number(clamped))
  return {
    dosTime: (value.getUTCHours() << 11) |
      (value.getUTCMinutes() << 5) |
      (value.getUTCSeconds() >>> 1),
    dosDate: ((value.getUTCFullYear() - 1980) << 9) |
      ((value.getUTCMonth() + 1) << 5) |
      value.getUTCDate(),
  }
}

function requireDosDateTime(time: number, date: number): void {
  if (!Number.isInteger(time) || time < 0 || time > 0xffff ||
      !Number.isInteger(date) || date < 0 || date > 0xffff) {
    throw new TypeError('ZIP DOS timestamp is invalid')
  }
  const second = (time & 0x1f) * 2
  const minute = (time >>> 5) & 0x3f
  const hour = (time >>> 11) & 0x1f
  const day = date & 0x1f
  const month = (date >>> 5) & 0x0f
  const year = 1980 + ((date >>> 9) & 0x7f)
  const candidate = new Date(Date.UTC(year, month - 1, day, hour, minute, second))
  if (hour > 23 || minute > 59 || second > 58 || month < 1 || month > 12 || day < 1 ||
      candidate.getUTCFullYear() !== year || candidate.getUTCMonth() + 1 !== month ||
      candidate.getUTCDate() !== day) {
    throw new TypeError('ZIP DOS timestamp is invalid')
  }
}

function requireNormalizedEntry(entry: NormalizedZipEntry): void {
  if (entry.kind !== 'directory' && entry.kind !== 'file') throw new TypeError('ZIP entry kind is invalid')
  if (entry.kind === 'directory' && entry.exactSize !== 0n) {
    throw new TypeError('ZIP directory entries must be empty')
  }
  requireZipUint64(entry.exactSize, 'ZIP entry size')
  requireDosDateTime(entry.dosTime, entry.dosDate)
  const canonicalPath = snapshotPortableCatalogPath(entry.path)
  const artifactPath = canonicalPath.join('/')
  if (entry.artifactPath !== artifactPath ||
      !equalNumbers(entry.artifactPathBytes, TEXT_ENCODER.encode(artifactPath)) ||
      !equalNumbers(entry.nameBytes, TEXT_ENCODER.encode(
        entry.kind === 'directory' ? `${artifactPath}/` : artifactPath,
      ))) {
    throw new TypeError('ZIP entry name is not canonical')
  }
}

function compareUnsignedBytes(left: readonly number[], right: readonly number[]): number {
  const shared = Math.min(left.length, right.length)
  for (let index = 0; index < shared; index += 1) {
    const difference = (left[index] ?? 0) - (right[index] ?? 0)
    if (difference !== 0) return difference
  }
  return left.length - right.length
}

function frozenBytes(bytes: Uint8Array): readonly number[] {
  return Object.freeze([...bytes])
}

function equalNumbers(left: readonly number[], right: ArrayLike<number>): boolean {
  if (left.length !== right.length) return false
  for (let index = 0; index < left.length; index += 1) {
    if (left[index] !== right[index]) return false
  }
  return true
}

function equalStrings(left: readonly string[], right: readonly string[]): boolean {
  if (left.length !== right.length) return false
  return left.every((value, index) => value === right[index])
}

function toAllocationLength(value: bigint, label: string): number {
  if (value > BigInt(Number.MAX_SAFE_INTEGER)) throw new RangeError(`${label} is too large to allocate`)
  return Number(value)
}

function assertEncodedLength(bytes: Uint8Array, expected: bigint, label: string): void {
  if (BigInt(bytes.byteLength) !== expected) throw new Error(`${label} length disagrees with its plan`)
}

function snapshotTrailingExtraFields(
  bytes: Uint8Array,
  plannedExtraBytes: bigint,
  label: string,
): Uint8Array<ArrayBuffer> {
  if (!(bytes instanceof Uint8Array)) throw new TypeError(`${label} trailing extra fields are invalid`)
  if (plannedExtraBytes + BigInt(bytes.byteLength) > ZIP_UINT16_SENTINEL) {
    throw new RangeError(`${label} extra fields exceed the ZIP format`)
  }
  return Uint8Array.from(bytes)
}

function requireUint32(value: number, label: string): void {
  if (!Number.isInteger(value) || value < 0 || value > 0xffff_ffff) {
    throw new RangeError(`${label} is outside uint32`)
  }
}

const CRC32_TABLE = (() => {
  const table = new Uint32Array(256)
  for (let index = 0; index < table.length; index += 1) {
    let value = index
    for (let bit = 0; bit < 8; bit += 1) {
      value = (value & 1) === 0 ? value >>> 1 : 0xedb8_8320 ^ (value >>> 1)
    }
    table[index] = value >>> 0
  }
  return table
})()
