import {
  ZIP_UINT16_SENTINEL,
  ZIP_UINT32_SENTINEL,
  requiresZip64End,
  requireZipUint64,
} from '../../zip-layout/policy'
import { requireDirectZipFsaOffset } from './offset'

export const DIRECT_ZIP_CLASSIC_END_PROOF_BYTES = 22
export const DIRECT_ZIP_ZIP64_END_PROOF_BYTES = 98

const ZIP_END_OF_CENTRAL_DIRECTORY_SIGNATURE = 0x0605_4b50
const ZIP64_END_OF_CENTRAL_DIRECTORY_SIGNATURE = 0x0606_4b50
const ZIP64_END_OF_CENTRAL_DIRECTORY_LOCATOR_SIGNATURE = 0x0706_4b50
const ZIP64_END_RECORD_BODY_BYTES = 44n
const ZIP64_VERSION = 45

export interface DirectZipEndProofV2 {
  readonly entryCount: bigint
  readonly centralDirectoryOffset: bigint
  readonly centralDirectoryBytes: bigint
  readonly exactArchiveBytes: bigint
  readonly zip64EndRequired: boolean
  readonly proofBytes: typeof DIRECT_ZIP_CLASSIC_END_PROOF_BYTES |
    typeof DIRECT_ZIP_ZIP64_END_PROOF_BYTES
}

export interface ExpectedDirectZipEndProofV2 {
  readonly entryCount: bigint
  readonly centralDirectoryOffset: bigint
  readonly centralDirectoryBytes: bigint
  readonly exactArchiveBytes: bigint
  readonly zip64EndRequired: boolean
}

export function directZipClosingTailReadBytes(
  zip64EndRequired: boolean,
): typeof DIRECT_ZIP_CLASSIC_END_PROOF_BYTES | typeof DIRECT_ZIP_ZIP64_END_PROOF_BYTES {
  if (typeof zip64EndRequired !== 'boolean') {
    throw new TypeError('direct ZIP end-record kind is invalid')
  }
  return zip64EndRequired
    ? DIRECT_ZIP_ZIP64_END_PROOF_BYTES
    : DIRECT_ZIP_CLASSIC_END_PROOF_BYTES
}

export function parseDirectZipClosingTailV2(
  tail: Uint8Array,
  exactArchiveBytes: bigint,
): DirectZipEndProofV2 {
  if (!(tail instanceof Uint8Array) ||
      (tail.byteLength !== DIRECT_ZIP_CLASSIC_END_PROOF_BYTES &&
       tail.byteLength !== DIRECT_ZIP_ZIP64_END_PROOF_BYTES)) {
    throw new TypeError('direct ZIP closing tail length is invalid')
  }
  requireDirectZipFsaOffset(exactArchiveBytes, 'direct ZIP exact archive length')
  if (exactArchiveBytes < BigInt(tail.byteLength)) {
    throw new TypeError('direct ZIP closing tail exceeds the archive')
  }
  return tail.byteLength === DIRECT_ZIP_CLASSIC_END_PROOF_BYTES
    ? parseClassicTail(tail, exactArchiveBytes)
    : parseZip64Tail(tail, exactArchiveBytes)
}

export function validateDirectZipClosingTailV2(
  tail: Uint8Array,
  expected: ExpectedDirectZipEndProofV2,
): DirectZipEndProofV2 {
  if (expected === null || typeof expected !== 'object') {
    throw new TypeError('direct ZIP expected end proof is invalid')
  }
  requireZipUint64(expected.entryCount, 'direct ZIP entry count')
  requireDirectZipFsaOffset(expected.centralDirectoryOffset, 'direct ZIP central-directory offset')
  requireDirectZipFsaOffset(expected.centralDirectoryBytes, 'direct ZIP central-directory size')
  requireDirectZipFsaOffset(expected.exactArchiveBytes, 'direct ZIP exact archive length')
  if (typeof expected.zip64EndRequired !== 'boolean' ||
      tail.byteLength !== directZipClosingTailReadBytes(expected.zip64EndRequired)) {
    throw new TypeError('direct ZIP closing tail kind disagrees with the journal')
  }
  const actual = parseDirectZipClosingTailV2(tail, expected.exactArchiveBytes)
  if (actual.entryCount !== expected.entryCount ||
      actual.centralDirectoryOffset !== expected.centralDirectoryOffset ||
      actual.centralDirectoryBytes !== expected.centralDirectoryBytes ||
      actual.zip64EndRequired !== expected.zip64EndRequired) {
    throw new TypeError('direct ZIP closing tail disagrees with the journal layout')
  }
  return actual
}

function parseClassicTail(
  tail: Uint8Array,
  exactArchiveBytes: bigint,
): DirectZipEndProofV2 {
  const view = new DataView(tail.buffer, tail.byteOffset, tail.byteLength)
  requireClassicEnvelope(view, 0)
  const entryCount = BigInt(view.getUint16(8, true))
  const centralDirectoryBytes = BigInt(view.getUint32(12, true))
  const centralDirectoryOffset = BigInt(view.getUint32(16, true))
  if (entryCount === 0n || entryCount >= ZIP_UINT16_SENTINEL ||
      centralDirectoryBytes >= ZIP_UINT32_SENTINEL ||
      centralDirectoryOffset >= ZIP_UINT32_SENTINEL || requiresZip64End({
        entryCount,
        centralDirectoryOffset,
        centralDirectoryBytes,
      })) {
    throw new TypeError('direct ZIP classic tail uses a ZIP64 sentinel')
  }
  requireCentralDirectoryEndsAtTail(
    centralDirectoryOffset,
    centralDirectoryBytes,
    exactArchiveBytes - BigInt(DIRECT_ZIP_CLASSIC_END_PROOF_BYTES),
  )
  return Object.freeze({
    entryCount,
    centralDirectoryOffset,
    centralDirectoryBytes,
    exactArchiveBytes,
    zip64EndRequired: false,
    proofBytes: DIRECT_ZIP_CLASSIC_END_PROOF_BYTES,
  })
}

function parseZip64Tail(
  tail: Uint8Array,
  exactArchiveBytes: bigint,
): DirectZipEndProofV2 {
  const view = new DataView(tail.buffer, tail.byteOffset, tail.byteLength)
  const locatorOffset = 56
  const classicOffset = 76
  if (view.getUint32(0, true) !== ZIP64_END_OF_CENTRAL_DIRECTORY_SIGNATURE ||
      view.getBigUint64(4, true) !== ZIP64_END_RECORD_BODY_BYTES ||
      view.getUint16(12, true) !== ZIP64_VERSION || view.getUint16(14, true) !== ZIP64_VERSION ||
      view.getUint32(16, true) !== 0 || view.getUint32(20, true) !== 0 ||
      view.getBigUint64(24, true) !== view.getBigUint64(32, true)) {
    throw new TypeError('direct ZIP64 end record is invalid')
  }
  const entryCount = view.getBigUint64(24, true)
  const centralDirectoryBytes = view.getBigUint64(40, true)
  const centralDirectoryOffset = view.getBigUint64(48, true)
  requireDirectZipFsaOffset(centralDirectoryOffset, 'direct ZIP central-directory offset')
  requireDirectZipFsaOffset(centralDirectoryBytes, 'direct ZIP central-directory size')
  if (entryCount === 0n ||
      !requiresZip64End({ entryCount, centralDirectoryOffset, centralDirectoryBytes })) {
    throw new TypeError('direct ZIP64 end record is not required by the layout')
  }
  const centralEnd = exactArchiveBytes - BigInt(DIRECT_ZIP_ZIP64_END_PROOF_BYTES)
  requireCentralDirectoryEndsAtTail(centralDirectoryOffset, centralDirectoryBytes, centralEnd)

  if (view.getUint32(locatorOffset, true) !== ZIP64_END_OF_CENTRAL_DIRECTORY_LOCATOR_SIGNATURE ||
      view.getUint32(locatorOffset + 4, true) !== 0 ||
      view.getBigUint64(locatorOffset + 8, true) !== centralEnd ||
      view.getUint32(locatorOffset + 16, true) !== 1) {
    throw new TypeError('direct ZIP64 end locator is invalid')
  }
  requireClassicEnvelope(view, classicOffset)
  if (view.getUint16(classicOffset + 8, true) !== classicU16(entryCount) ||
      view.getUint32(classicOffset + 12, true) !== classicU32(centralDirectoryBytes) ||
      view.getUint32(classicOffset + 16, true) !== classicU32(centralDirectoryOffset)) {
    throw new TypeError('direct ZIP classic and ZIP64 end records disagree')
  }
  return Object.freeze({
    entryCount,
    centralDirectoryOffset,
    centralDirectoryBytes,
    exactArchiveBytes,
    zip64EndRequired: true,
    proofBytes: DIRECT_ZIP_ZIP64_END_PROOF_BYTES,
  })
}

function requireClassicEnvelope(view: DataView, offset: number): void {
  if (view.getUint32(offset, true) !== ZIP_END_OF_CENTRAL_DIRECTORY_SIGNATURE ||
      view.getUint16(offset + 4, true) !== 0 || view.getUint16(offset + 6, true) !== 0 ||
      view.getUint16(offset + 8, true) !== view.getUint16(offset + 10, true) ||
      view.getUint16(offset + 20, true) !== 0) {
    throw new TypeError('direct ZIP classic end record is invalid')
  }
}

function requireCentralDirectoryEndsAtTail(
  offset: bigint,
  bytes: bigint,
  tailOffset: bigint,
): void {
  if (offset + bytes !== tailOffset) {
    throw new TypeError('direct ZIP central directory does not end at the bounded closing tail')
  }
}

function classicU16(value: bigint): number {
  return value >= ZIP_UINT16_SENTINEL ? Number(ZIP_UINT16_SENTINEL) : Number(value)
}

function classicU32(value: bigint): number {
  return value >= ZIP_UINT32_SENTINEL ? Number(ZIP_UINT32_SENTINEL) : Number(value)
}
