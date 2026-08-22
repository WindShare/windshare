import { snapshotPortableCatalogPath } from '../../../catalog/path-policy'
import {
  concatDirectZipBytes,
  equalDirectZipBytes,
  type DirectZipCanonicalBytes,
} from './canonical'
import {
  encodeDirectZipDataDescriptorV2,
  encodeDirectZipLocalHeaderV2,
  planDirectZipEntryV2,
} from './layout'
import {
  DIRECT_ZIP_OWNERSHIP_EXTRA_FIELD_BYTES,
  equalDirectZipOwnershipMarkersV1,
  requireDirectZipOwnershipExtraFieldsV1,
  type DirectZipOwnershipMarkerInputV1,
  type DirectZipOwnershipMarkerV1,
} from './ownership-extra'

export const DIRECT_ZIP_LOCAL_HEADER_FIXED_BYTES = 30
export const DIRECT_ZIP_CENTRAL_HEADER_FIXED_BYTES = 46
export const DIRECT_ZIP_SIGNED_ZIP32_DESCRIPTOR_BYTES = 16
export const DIRECT_ZIP_MAXIMUM_ROOT_NAME_BYTES = 256
export const DIRECT_ZIP_MAXIMUM_OWNERSHIP_HEADER_READ_BYTES =
  DIRECT_ZIP_LOCAL_HEADER_FIXED_BYTES + DIRECT_ZIP_MAXIMUM_ROOT_NAME_BYTES +
  DIRECT_ZIP_OWNERSHIP_EXTRA_FIELD_BYTES
export const DIRECT_ZIP_MAXIMUM_BOOTSTRAP_PREFIX_BYTES =
  DIRECT_ZIP_MAXIMUM_OWNERSHIP_HEADER_READ_BYTES + DIRECT_ZIP_SIGNED_ZIP32_DESCRIPTOR_BYTES

const ZIP_LOCAL_FILE_HEADER_SIGNATURE = 0x0403_4b50
const ZIP_CENTRAL_DIRECTORY_HEADER_SIGNATURE = 0x0201_4b50
const ZIP_DATA_DESCRIPTOR_SIGNATURE = 0x0807_4b50
const ZIP_UTF8_AND_DATA_DESCRIPTOR_FLAGS = 0x0808
const ZIP_STORE_METHOD = 0
const ZIP_CLASSIC_VERSION = 20
const ZIP64_VERSION = 45
const ZIP_DIRECTORY_ATTRIBUTE = 0x10
const TEXT_ENCODER = new TextEncoder()
const TEXT_DECODER = new TextDecoder('utf-8', { fatal: true })

export interface DirectZipOwnershipHeaderV1 {
  readonly rootComponent: string
  readonly rootNameBytes: DirectZipCanonicalBytes
  readonly marker: DirectZipOwnershipMarkerV1
  readonly headerBytes: number
}

export interface DirectZipOwnershipCentralRecordV1 extends DirectZipOwnershipHeaderV1 {
  readonly recordBytes: number
}

export function deriveDirectZipOwnershipHeaderReadBytes(
  fixedLocalHeader: Uint8Array,
): number {
  if (!(fixedLocalHeader instanceof Uint8Array) ||
      fixedLocalHeader.byteLength !== DIRECT_ZIP_LOCAL_HEADER_FIXED_BYTES) {
    throw new TypeError('direct ZIP fixed local header must contain exactly 30 bytes')
  }
  const view = new DataView(
    fixedLocalHeader.buffer,
    fixedLocalHeader.byteOffset,
    fixedLocalHeader.byteLength,
  )
  if (view.getUint32(0, true) !== ZIP_LOCAL_FILE_HEADER_SIGNATURE) {
    throw new TypeError('direct ZIP local-header signature is invalid')
  }
  const nameBytes = view.getUint16(26, true)
  const extraBytes = view.getUint16(28, true)
  if (nameBytes === 0 || nameBytes > DIRECT_ZIP_MAXIMUM_ROOT_NAME_BYTES ||
      extraBytes !== DIRECT_ZIP_OWNERSHIP_EXTRA_FIELD_BYTES) {
    throw new TypeError('direct ZIP ownership header lengths are not canonical')
  }
  const readBytes = DIRECT_ZIP_LOCAL_HEADER_FIXED_BYTES + nameBytes + extraBytes
  if (readBytes > DIRECT_ZIP_MAXIMUM_OWNERSHIP_HEADER_READ_BYTES) {
    throw new RangeError('direct ZIP ownership header exceeds its derived read bound')
  }
  return readBytes
}

export function encodeDirectZipBootstrapPrefixV1(
  rootComponent: string,
  marker: DirectZipOwnershipMarkerInputV1,
): DirectZipCanonicalBytes {
  const plan = planDirectZipEntryV2({
    ordinal: 0n,
    localHeaderOffset: 0n,
    entry: { kind: 'directory', path: [rootComponent] },
    ownershipMarker: marker,
  })
  const prefix = concatDirectZipBytes([
    encodeDirectZipLocalHeaderV2(plan),
    encodeDirectZipDataDescriptorV2(plan, 0),
  ])
  if (prefix.byteLength > DIRECT_ZIP_MAXIMUM_BOOTSTRAP_PREFIX_BYTES) {
    throw new Error('direct ZIP bootstrap prefix exceeds its derived bound')
  }
  return prefix
}

export function parseDirectZipOwnershipLocalHeaderV1(
  bytes: Uint8Array,
): DirectZipOwnershipHeaderV1 {
  if (!(bytes instanceof Uint8Array) || bytes.byteLength < DIRECT_ZIP_LOCAL_HEADER_FIXED_BYTES) {
    throw new TypeError('direct ZIP ownership local header is truncated')
  }
  const fixed = bytes.subarray(0, DIRECT_ZIP_LOCAL_HEADER_FIXED_BYTES)
  const headerBytes = deriveDirectZipOwnershipHeaderReadBytes(fixed)
  if (bytes.byteLength !== headerBytes) {
    throw new TypeError('direct ZIP ownership local header has trailing or truncated bytes')
  }
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength)
  requireCanonicalRootHeaderFields(view, 4, 6, 8, 14, 18, 22)
  const nameBytes = view.getUint16(26, true)
  const rootNameBytes = Uint8Array.from(bytes.subarray(
    DIRECT_ZIP_LOCAL_HEADER_FIXED_BYTES,
    DIRECT_ZIP_LOCAL_HEADER_FIXED_BYTES + nameBytes,
  ))
  const marker = requireDirectZipOwnershipExtraFieldsV1(
    bytes.subarray(DIRECT_ZIP_LOCAL_HEADER_FIXED_BYTES + nameBytes),
  )
  return Object.freeze({
    ...decodeRootName(rootNameBytes),
    marker,
    headerBytes,
  })
}

export function parseDirectZipBootstrapPrefixV1(
  bytes: Uint8Array,
): DirectZipOwnershipHeaderV1 {
  if (!(bytes instanceof Uint8Array) ||
      bytes.byteLength < DIRECT_ZIP_LOCAL_HEADER_FIXED_BYTES + DIRECT_ZIP_SIGNED_ZIP32_DESCRIPTOR_BYTES ||
      bytes.byteLength > DIRECT_ZIP_MAXIMUM_BOOTSTRAP_PREFIX_BYTES) {
    throw new TypeError('direct ZIP bootstrap prefix length is invalid')
  }
  const headerBytes = deriveDirectZipOwnershipHeaderReadBytes(
    bytes.subarray(0, DIRECT_ZIP_LOCAL_HEADER_FIXED_BYTES),
  )
  if (bytes.byteLength !== headerBytes + DIRECT_ZIP_SIGNED_ZIP32_DESCRIPTOR_BYTES) {
    throw new TypeError('direct ZIP bootstrap prefix has trailing or truncated bytes')
  }
  const header = parseDirectZipOwnershipLocalHeaderV1(bytes.subarray(0, headerBytes))
  const descriptor = new DataView(
    bytes.buffer,
    bytes.byteOffset + headerBytes,
    DIRECT_ZIP_SIGNED_ZIP32_DESCRIPTOR_BYTES,
  )
  if (descriptor.getUint32(0, true) !== ZIP_DATA_DESCRIPTOR_SIGNATURE ||
      descriptor.getUint32(4, true) !== 0 || descriptor.getUint32(8, true) !== 0 ||
      descriptor.getUint32(12, true) !== 0) {
    throw new TypeError('direct ZIP bootstrap descriptor is invalid')
  }
  return header
}

export function parseDirectZipOwnershipCentralRecordV1(
  bytes: Uint8Array,
): DirectZipOwnershipCentralRecordV1 {
  if (!(bytes instanceof Uint8Array) || bytes.byteLength < DIRECT_ZIP_CENTRAL_HEADER_FIXED_BYTES) {
    throw new TypeError('direct ZIP ownership central record is truncated')
  }
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength)
  if (view.getUint32(0, true) !== ZIP_CENTRAL_DIRECTORY_HEADER_SIGNATURE ||
      view.getUint16(4, true) !== ZIP64_VERSION) {
    throw new TypeError('direct ZIP ownership central record signature or creator is invalid')
  }
  requireCanonicalRootHeaderFields(view, 6, 8, 10, 16, 20, 24)
  const nameBytes = view.getUint16(28, true)
  const extraBytes = view.getUint16(30, true)
  const commentBytes = view.getUint16(32, true)
  const recordBytes = DIRECT_ZIP_CENTRAL_HEADER_FIXED_BYTES + nameBytes + extraBytes + commentBytes
  if (nameBytes === 0 || nameBytes > DIRECT_ZIP_MAXIMUM_ROOT_NAME_BYTES ||
      extraBytes !== DIRECT_ZIP_OWNERSHIP_EXTRA_FIELD_BYTES || commentBytes !== 0 ||
      recordBytes !== bytes.byteLength || view.getUint16(34, true) !== 0 ||
      view.getUint16(36, true) !== 0 || view.getUint32(38, true) !== ZIP_DIRECTORY_ATTRIBUTE ||
      view.getUint32(42, true) !== 0) {
    throw new TypeError('direct ZIP ownership central record is not canonical')
  }
  const rootNameBytes = Uint8Array.from(bytes.subarray(
    DIRECT_ZIP_CENTRAL_HEADER_FIXED_BYTES,
    DIRECT_ZIP_CENTRAL_HEADER_FIXED_BYTES + nameBytes,
  ))
  const marker = requireDirectZipOwnershipExtraFieldsV1(bytes.subarray(
    DIRECT_ZIP_CENTRAL_HEADER_FIXED_BYTES + nameBytes,
    DIRECT_ZIP_CENTRAL_HEADER_FIXED_BYTES + nameBytes + extraBytes,
  ))
  return Object.freeze({
    ...decodeRootName(rootNameBytes),
    marker,
    headerBytes: DIRECT_ZIP_CENTRAL_HEADER_FIXED_BYTES + nameBytes + extraBytes,
    recordBytes,
  })
}

export function requireMatchingDirectZipOwnershipRecordsV1(
  local: DirectZipOwnershipHeaderV1,
  central: DirectZipOwnershipCentralRecordV1,
): void {
  if (local.rootComponent !== central.rootComponent ||
      !equalDirectZipBytes(local.rootNameBytes, central.rootNameBytes) ||
      !equalDirectZipOwnershipMarkersV1(local.marker, central.marker)) {
    throw new TypeError('direct ZIP local and central ownership records disagree')
  }
}

function requireCanonicalRootHeaderFields(
  view: DataView,
  versionOffset: number,
  flagsOffset: number,
  methodOffset: number,
  crcOffset: number,
  compressedSizeOffset: number,
  uncompressedSizeOffset: number,
): void {
  if (view.getUint16(versionOffset, true) !== ZIP_CLASSIC_VERSION ||
      view.getUint16(flagsOffset, true) !== ZIP_UTF8_AND_DATA_DESCRIPTOR_FLAGS ||
      view.getUint16(methodOffset, true) !== ZIP_STORE_METHOD ||
      view.getUint32(crcOffset, true) !== 0 ||
      view.getUint32(compressedSizeOffset, true) !== 0 ||
      view.getUint32(uncompressedSizeOffset, true) !== 0) {
    throw new TypeError('direct ZIP ownership root header fields are not canonical')
  }
}

function decodeRootName(rootNameBytes: Uint8Array): {
  readonly rootComponent: string
  readonly rootNameBytes: DirectZipCanonicalBytes
} {
  let rootName: string
  try {
    rootName = TEXT_DECODER.decode(rootNameBytes)
  } catch {
    throw new TypeError('direct ZIP result-root name is not UTF-8')
  }
  if (!rootName.endsWith('/') || rootName.indexOf('/') !== rootName.length - 1) {
    throw new TypeError('direct ZIP ownership member is not a result-root directory')
  }
  const rootComponent = rootName.slice(0, -1)
  snapshotPortableCatalogPath([rootComponent])
  if (!equalDirectZipBytes(TEXT_ENCODER.encode(`${rootComponent}/`), rootNameBytes)) {
    throw new TypeError('direct ZIP result-root name is not canonical')
  }
  return Object.freeze({ rootComponent, rootNameBytes: Uint8Array.from(rootNameBytes) })
}
