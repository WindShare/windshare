import {
  concatDirectZipBytes,
  digestDirectZipCanonicalBytes,
  directZipCanonicalFrame,
  directZipCanonicalRecord,
  directZipCanonicalU16,
  equalDirectZipBytes,
  snapshotDirectZipFixedBytes,
  type DirectZipCanonicalBytes,
} from './canonical'

export const DIRECT_ZIP_OWNERSHIP_EXTRA_FORMAT_DOMAIN =
  'windshare/direct-zip-ownership-extra/v1' as const
export const DIRECT_ZIP_OWNERSHIP_EXTRA_HEADER_ID = 0x5357
export const DIRECT_ZIP_OWNERSHIP_EXTRA_FORMAT_VERSION = 1
export const DIRECT_ZIP_OWNERSHIP_EXTRA_FLAGS = 0
export const DIRECT_ZIP_OPERATION_ID_BYTES = 16
export const DIRECT_ZIP_CANDIDATE_ID_BYTES = 16
export const DIRECT_ZIP_OWNERSHIP_NONCE_BYTES = 32
export const DIRECT_ZIP_BINDING_DIGEST_BYTES = 32
export const DIRECT_ZIP_OWNERSHIP_EXTRA_DATA_BYTES = 116
export const DIRECT_ZIP_OWNERSHIP_EXTRA_FIELD_BYTES = 120

const EXTRA_FIELD_HEADER_BYTES = 4
const ZIP_EXTRA_FIELDS_MAXIMUM_BYTES = 0xffff
const OWNERSHIP_MAGIC_OFFSET = 0
const FORMAT_VERSION_OFFSET = 16
const FLAGS_OFFSET = 18
const OPERATION_ID_OFFSET = 20
const CANDIDATE_ID_OFFSET = 36
const OWNERSHIP_NONCE_OFFSET = 52
const BINDING_DIGEST_OFFSET = 84
const OWNERSHIP_MAGIC = Uint8Array.from([
  0x57, 0x69, 0x6e, 0x64, 0x53, 0x68, 0x61, 0x72,
  0x65, 0x5a, 0x69, 0x70, 0x4f, 0x77, 0x6e, 0x00,
])

export interface DirectZipOwnershipMarkerV1 {
  readonly version: typeof DIRECT_ZIP_OWNERSHIP_EXTRA_FORMAT_VERSION
  readonly operationId: DirectZipCanonicalBytes
  readonly candidateId: DirectZipCanonicalBytes
  readonly ownershipNonce: DirectZipCanonicalBytes
  readonly bindingDigest: DirectZipCanonicalBytes
}

export interface DirectZipOwnershipMarkerInputV1 {
  readonly operationId: Uint8Array
  readonly candidateId: Uint8Array
  readonly ownershipNonce: Uint8Array
  readonly bindingDigest: Uint8Array
}

export function snapshotDirectZipOwnershipMarkerV1(
  input: DirectZipOwnershipMarkerInputV1,
): DirectZipOwnershipMarkerV1 {
  if (input === null || typeof input !== 'object') {
    throw new TypeError('direct ZIP ownership marker is invalid')
  }
  return Object.freeze({
    version: DIRECT_ZIP_OWNERSHIP_EXTRA_FORMAT_VERSION,
    operationId: snapshotDirectZipFixedBytes(
      input.operationId,
      DIRECT_ZIP_OPERATION_ID_BYTES,
      'direct ZIP operation ID',
    ),
    candidateId: snapshotDirectZipFixedBytes(
      input.candidateId,
      DIRECT_ZIP_CANDIDATE_ID_BYTES,
      'direct ZIP candidate ID',
    ),
    ownershipNonce: snapshotDirectZipFixedBytes(
      input.ownershipNonce,
      DIRECT_ZIP_OWNERSHIP_NONCE_BYTES,
      'direct ZIP ownership nonce',
    ),
    bindingDigest: snapshotDirectZipFixedBytes(
      input.bindingDigest,
      DIRECT_ZIP_BINDING_DIGEST_BYTES,
      'direct ZIP binding digest',
    ),
  })
}

export function encodeDirectZipOwnershipExtraFieldV1(
  input: DirectZipOwnershipMarkerInputV1,
): DirectZipCanonicalBytes {
  const marker = snapshotDirectZipOwnershipMarkerV1(input)
  const bytes = new Uint8Array(DIRECT_ZIP_OWNERSHIP_EXTRA_FIELD_BYTES)
  const view = new DataView(bytes.buffer)
  view.setUint16(0, DIRECT_ZIP_OWNERSHIP_EXTRA_HEADER_ID, true)
  view.setUint16(2, DIRECT_ZIP_OWNERSHIP_EXTRA_DATA_BYTES, true)
  bytes.set(OWNERSHIP_MAGIC, EXTRA_FIELD_HEADER_BYTES + OWNERSHIP_MAGIC_OFFSET)
  view.setUint16(EXTRA_FIELD_HEADER_BYTES + FORMAT_VERSION_OFFSET, marker.version, true)
  view.setUint16(EXTRA_FIELD_HEADER_BYTES + FLAGS_OFFSET, DIRECT_ZIP_OWNERSHIP_EXTRA_FLAGS, true)
  bytes.set(marker.operationId, EXTRA_FIELD_HEADER_BYTES + OPERATION_ID_OFFSET)
  bytes.set(marker.candidateId, EXTRA_FIELD_HEADER_BYTES + CANDIDATE_ID_OFFSET)
  bytes.set(marker.ownershipNonce, EXTRA_FIELD_HEADER_BYTES + OWNERSHIP_NONCE_OFFSET)
  bytes.set(marker.bindingDigest, EXTRA_FIELD_HEADER_BYTES + BINDING_DIGEST_OFFSET)
  return bytes
}

export function decodeDirectZipOwnershipExtraFieldsV1(
  extraFields: Uint8Array,
): DirectZipOwnershipMarkerV1 | undefined {
  if (!(extraFields instanceof Uint8Array) ||
      extraFields.byteLength > ZIP_EXTRA_FIELDS_MAXIMUM_BYTES) {
    throw new TypeError('ZIP extra fields are invalid')
  }
  let offset = 0
  let ownership: DirectZipOwnershipMarkerV1 | undefined
  while (offset < extraFields.byteLength) {
    if (extraFields.byteLength - offset < EXTRA_FIELD_HEADER_BYTES) {
      throw new TypeError('ZIP extra-field header is truncated')
    }
    const view = new DataView(extraFields.buffer, extraFields.byteOffset + offset)
    const headerId = view.getUint16(0, true)
    const dataBytes = view.getUint16(2, true)
    const dataStart = offset + EXTRA_FIELD_HEADER_BYTES
    const dataEnd = dataStart + dataBytes
    if (dataEnd > extraFields.byteLength) throw new TypeError('ZIP extra-field payload is truncated')
    const data = extraFields.subarray(dataStart, dataEnd)
    offset = dataEnd

    if (headerId !== DIRECT_ZIP_OWNERSHIP_EXTRA_HEADER_ID ||
        data.byteLength < OWNERSHIP_MAGIC.byteLength ||
        !equalDirectZipBytes(data.subarray(0, OWNERSHIP_MAGIC.byteLength), OWNERSHIP_MAGIC)) {
      continue
    }
    if (ownership !== undefined) throw new TypeError('ZIP contains multiple WindShare ownership fields')
    ownership = decodeMatchingOwnershipPayload(data)
  }
  return ownership
}

export function requireDirectZipOwnershipExtraFieldsV1(
  extraFields: Uint8Array,
): DirectZipOwnershipMarkerV1 {
  const marker = decodeDirectZipOwnershipExtraFieldsV1(extraFields)
  if (marker === undefined) throw new TypeError('ZIP WindShare ownership field is absent')
  return marker
}

export function equalDirectZipOwnershipMarkersV1(
  left: DirectZipOwnershipMarkerInputV1,
  right: DirectZipOwnershipMarkerInputV1,
): boolean {
  const leftMarker = snapshotDirectZipOwnershipMarkerV1(left)
  const rightMarker = snapshotDirectZipOwnershipMarkerV1(right)
  return equalDirectZipBytes(leftMarker.operationId, rightMarker.operationId) &&
    equalDirectZipBytes(leftMarker.candidateId, rightMarker.candidateId) &&
    equalDirectZipBytes(leftMarker.ownershipNonce, rightMarker.ownershipNonce) &&
    equalDirectZipBytes(leftMarker.bindingDigest, rightMarker.bindingDigest)
}

export function directZipOwnershipExtraFormatCanonicalV1(): DirectZipCanonicalBytes {
  return directZipCanonicalRecord(DIRECT_ZIP_OWNERSHIP_EXTRA_FORMAT_DOMAIN, [
    directZipCanonicalFrame(directZipCanonicalU16(DIRECT_ZIP_OWNERSHIP_EXTRA_HEADER_ID)),
    directZipCanonicalFrame(OWNERSHIP_MAGIC),
    directZipCanonicalFrame(directZipCanonicalU16(DIRECT_ZIP_OWNERSHIP_EXTRA_FORMAT_VERSION)),
    directZipCanonicalFrame(directZipCanonicalU16(DIRECT_ZIP_OWNERSHIP_EXTRA_FLAGS)),
    directZipCanonicalFrame(directZipCanonicalU16(DIRECT_ZIP_OWNERSHIP_NONCE_BYTES)),
    directZipCanonicalFrame(directZipCanonicalU16(DIRECT_ZIP_OWNERSHIP_EXTRA_DATA_BYTES)),
  ])
}

export async function directZipOwnershipExtraFormatDigestV1(): Promise<DirectZipCanonicalBytes> {
  return digestDirectZipCanonicalBytes(directZipOwnershipExtraFormatCanonicalV1())
}

function decodeMatchingOwnershipPayload(data: Uint8Array): DirectZipOwnershipMarkerV1 {
  if (data.byteLength !== DIRECT_ZIP_OWNERSHIP_EXTRA_DATA_BYTES) {
    throw new TypeError('WindShare ownership extra-field payload length is invalid')
  }
  const view = new DataView(data.buffer, data.byteOffset, data.byteLength)
  const version = view.getUint16(FORMAT_VERSION_OFFSET, true)
  const flags = view.getUint16(FLAGS_OFFSET, true)
  if (version !== DIRECT_ZIP_OWNERSHIP_EXTRA_FORMAT_VERSION) {
    throw new TypeError('WindShare ownership extra-field version is unsupported')
  }
  if (flags !== DIRECT_ZIP_OWNERSHIP_EXTRA_FLAGS) {
    throw new TypeError('WindShare ownership extra-field flags are unsupported')
  }
  return snapshotDirectZipOwnershipMarkerV1({
    operationId: data.subarray(OPERATION_ID_OFFSET, CANDIDATE_ID_OFFSET),
    candidateId: data.subarray(CANDIDATE_ID_OFFSET, OWNERSHIP_NONCE_OFFSET),
    ownershipNonce: data.subarray(OWNERSHIP_NONCE_OFFSET, BINDING_DIGEST_OFFSET),
    bindingDigest: data.subarray(BINDING_DIGEST_OFFSET),
  })
}

export function concatenateDirectZipExtraFields(
  fields: readonly Uint8Array[],
): DirectZipCanonicalBytes {
  const bytes = concatDirectZipBytes(fields)
  if (bytes.byteLength > ZIP_EXTRA_FIELDS_MAXIMUM_BYTES) {
    throw new RangeError('ZIP extra fields exceed the ZIP format')
  }
  return bytes
}
