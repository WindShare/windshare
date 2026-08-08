import { encodeBase64Url } from '../../crypto/bytes'
import {
  FILE_CHECKPOINT_ID_BYTES,
  FILE_CHECKPOINT_MAX_BACKEND_BYTES,
  FILE_CHECKPOINT_MAX_PATH_BYTES,
  FILE_CHECKPOINT_MAX_RANGES,
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
  FILE_CHECKPOINT_V1_SCHEMA_VERSION,
  FILE_ID_BYTES,
  FILE_REVISION_BYTES,
  checkpointIdentityEqual,
  identityBytes,
  normalizeFileCheckpointSpec,
  validateCheckpointTransitionSemantics,
  type FileCheckpointRange,
  type FileCheckpointSpec,
  type FileCheckpointV1,
} from './checkpoint-model'
import {
  FILE_CHECKPOINT_COMMIT_PUBLISHED,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FileCheckpointError,
} from './checkpoint-lifecycle'

export const FILE_CHECKPOINT_RECORD_DOMAIN = 'windshare/file-checkpoint-record/v1' as const
export const FILE_CHECKPOINT_CHECKSUM_DOMAIN = 'windshare/file-checkpoint-checksum/v1' as const
export const FILE_CHECKPOINT_MAGIC = new TextEncoder().encode('WSFCPV1\0')

const FILE_CHECKPOINT_TEXT_DECODER = new TextDecoder('utf-8', { fatal: true })
const CHECKSUM_BYTES = 32
const PAYLOAD_LENGTH_BYTES = 4

/** Construct an immutable record; persisted input is always rejected rather than repaired. */
export function newFileCheckpointV1(spec: FileCheckpointSpec): FileCheckpointV1 {
  const normalized = normalizeFileCheckpointSpec(spec)
  const identityPayload = checkpointConcatBytes([
    checkpointFramedText(FILE_CHECKPOINT_RECORD_DOMAIN),
    checkpointFramedBytes(Uint8Array.of(FILE_CHECKPOINT_V1_SCHEMA_VERSION)),
    checkpointFramedText(normalized.ownershipMarker),
    checkpointFramedText(normalized.namespace),
    checkpointFramedBytes(normalized.transferIntentDigest),
    checkpointFramedBytes(normalized.fileId),
    checkpointFramedBytes(normalized.fileRevision),
    checkpointFramedText(normalized.canonicalPath),
    uint64Bytes(normalized.exactSize),
    checkpointFramedText(normalized.backend),
    checkpointFramedBytes(normalized.rootIdentity),
    checkpointFramedBytes(normalized.ownedOutputObject),
  ])
  const withoutChecksum: Omit<FileCheckpointV1, 'checksum'> = {
    schemaVersion: FILE_CHECKPOINT_V1_SCHEMA_VERSION,
    ownershipMarker: FILE_CHECKPOINT_OWNERSHIP_MARKER,
    namespace: FILE_CHECKPOINT_NAMESPACE,
    recordId: encodeBase64Url(sha256Sync(identityPayload)),
    transferIntentDigest: encodeBase64Url(normalized.transferIntentDigest),
    fileId: encodeBase64Url(normalized.fileId),
    fileRevision: encodeBase64Url(normalized.fileRevision),
    canonicalPath: normalized.canonicalPath,
    exactSize: normalized.exactSize,
    backend: normalized.backend,
    rootIdentity: encodeBase64Url(normalized.rootIdentity),
    ownedOutputObject: encodeBase64Url(normalized.ownedOutputObject),
    stateGeneration: normalized.stateGeneration,
    checkpointGeneration: normalized.checkpointGeneration,
    verifiedRanges: Object.freeze(normalized.verifiedRanges),
    phase: normalized.phase,
    commitState: normalized.commitState,
    quarantineReason: normalized.quarantineReason,
    quarantineOrigin: normalized.quarantineOrigin,
    retirementReason: normalized.retirementReason,
  }
  return Object.freeze({
    ...withoutChecksum,
    checksum: encodeBase64Url(checksumFor(withoutChecksum)),
  })
}

/** Deterministic digest for metadata fields whose identity is defined by canonical content. */
export function deriveCheckpointIdentity(value: string): string {
  return encodeBase64Url(sha256Sync(new TextEncoder().encode(value)))
}

export function canonicalFileCheckpointBytes(record: FileCheckpointV1): Uint8Array<ArrayBuffer> {
  validateFileCheckpoint(record)
  return canonicalPayload(record)
}

export function encodeFileCheckpointV1(record: FileCheckpointV1): Uint8Array<ArrayBuffer> {
  validateFileCheckpoint(record)
  const payload = canonicalPayload(record)
  const output = new Uint8Array(
    FILE_CHECKPOINT_MAGIC.byteLength + PAYLOAD_LENGTH_BYTES + payload.byteLength + CHECKSUM_BYTES,
  )
  output.set(FILE_CHECKPOINT_MAGIC, 0)
  new DataView(output.buffer).setUint32(FILE_CHECKPOINT_MAGIC.byteLength, payload.byteLength, false)
  output.set(payload, FILE_CHECKPOINT_MAGIC.byteLength + PAYLOAD_LENGTH_BYTES)
  output.set(
    checksumFor(record),
    FILE_CHECKPOINT_MAGIC.byteLength + PAYLOAD_LENGTH_BYTES + payload.byteLength,
  )
  return output
}

export function decodeFileCheckpointV1(encoded: Uint8Array): FileCheckpointV1 {
  const minimum = FILE_CHECKPOINT_MAGIC.byteLength + PAYLOAD_LENGTH_BYTES + 1 + CHECKSUM_BYTES
  if (encoded.byteLength < minimum ||
      !checkpointEqualBytes(encoded.subarray(0, FILE_CHECKPOINT_MAGIC.byteLength), FILE_CHECKPOINT_MAGIC)) {
    throw new FileCheckpointError('invalid', 'FileCheckpointV1 envelope is invalid')
  }
  const payloadLength = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength)
    .getUint32(FILE_CHECKPOINT_MAGIC.byteLength, false)
  const payloadStart = FILE_CHECKPOINT_MAGIC.byteLength + PAYLOAD_LENGTH_BYTES
  const payloadEnd = payloadStart + payloadLength
  if (payloadEnd + CHECKSUM_BYTES !== encoded.byteLength) {
    throw new FileCheckpointError('invalid', 'FileCheckpointV1 payload length is invalid')
  }
  const payload = encoded.subarray(payloadStart, payloadEnd)
  const suppliedChecksum = encoded.subarray(payloadEnd)
  if (!checkpointEqualBytes(suppliedChecksum, checkpointChecksumPayload(payload))) {
    throw new FileCheckpointError('checksum', 'FileCheckpointV1 checksum is invalid')
  }
  const cursor = new CheckpointCursor(payload)
  const domain = cursor.text(FILE_CHECKPOINT_RECORD_DOMAIN.length)
  if (domain !== FILE_CHECKPOINT_RECORD_DOMAIN) {
    throw new FileCheckpointError('non-canonical', 'FileCheckpointV1 domain is invalid')
  }
  const version = cursor.byte()
  if (version !== FILE_CHECKPOINT_V1_SCHEMA_VERSION) {
    throw new FileCheckpointError('invalid', 'FileCheckpointV1 schema version is invalid')
  }
  const marker = cursor.text(128)
  const namespace = cursor.text(256)
  const recordId = cursor.fixed(FILE_CHECKPOINT_ID_BYTES, 'record ID')
  const intent = cursor.fixed(FILE_CHECKPOINT_ID_BYTES, 'transfer intent digest')
  const fileId = cursor.fixed(FILE_ID_BYTES, 'file ID')
  const revision = cursor.fixed(FILE_REVISION_BYTES, 'file revision')
  const path = cursor.text(FILE_CHECKPOINT_MAX_PATH_BYTES)
  const exactSize = cursor.u64()
  const backend = cursor.text(FILE_CHECKPOINT_MAX_BACKEND_BYTES)
  const root = cursor.fixed(FILE_CHECKPOINT_ID_BYTES, 'root identity')
  const object = cursor.fixed(FILE_CHECKPOINT_ID_BYTES, 'owned output object')
  const stateGeneration = cursor.u64()
  const checkpointGeneration = cursor.u64()
  const count = cursor.u32()
  if (count > FILE_CHECKPOINT_MAX_RANGES) {
    throw new FileCheckpointError('invalid', 'too many checkpoint ranges')
  }
  const ranges: FileCheckpointRange[] = []
  for (let index = 0; index < count; index += 1) {
    ranges.push({ start: cursor.u64(), end: cursor.u64() })
  }
  const phase = cursor.byte()
  const commitState = cursor.byte()
  const quarantineReason = cursor.byte()
  const quarantineOrigin = cursor.byte()
  const retirementReason = cursor.byte()
  if (!cursor.done()) {
    throw new FileCheckpointError('non-canonical', 'FileCheckpointV1 payload has trailing bytes')
  }
  const record = newFileCheckpointV1({
    ownershipMarker: marker,
    namespace,
    transferIntentDigest: intent,
    fileId,
    fileRevision: revision,
    canonicalPath: path,
    exactSize,
    backend,
    rootIdentity: root,
    ownedOutputObject: object,
    stateGeneration,
    checkpointGeneration,
    verifiedRanges: ranges,
    phase,
    commitState,
    quarantineReason,
    quarantineOrigin,
    retirementReason,
  })
  if (record.recordId !== encodeBase64Url(recordId)) {
    throw new FileCheckpointError('binding', 'FileCheckpointV1 record ID does not match immutable identity')
  }
  if (record.checksum !== encodeBase64Url(suppliedChecksum)) {
    throw new FileCheckpointError('checksum', 'FileCheckpointV1 checksum does not match payload')
  }
  if (!checkpointEqualBytes(canonicalPayload(record), payload)) {
    throw new FileCheckpointError('non-canonical', 'FileCheckpointV1 encoding is not canonical')
  }
  return record
}

export function validateFileCheckpoint(record: FileCheckpointV1): void {
  const rebuilt = newFileCheckpointV1({
    ownershipMarker: record.ownershipMarker,
    namespace: record.namespace,
    transferIntentDigest: record.transferIntentDigest,
    fileId: record.fileId,
    fileRevision: record.fileRevision,
    canonicalPath: record.canonicalPath,
    exactSize: record.exactSize,
    backend: record.backend,
    rootIdentity: record.rootIdentity,
    ownedOutputObject: record.ownedOutputObject,
    stateGeneration: record.stateGeneration,
    checkpointGeneration: record.checkpointGeneration,
    verifiedRanges: record.verifiedRanges,
    phase: record.phase,
    commitState: record.commitState,
    quarantineReason: record.quarantineReason,
    quarantineOrigin: record.quarantineOrigin,
    retirementReason: record.retirementReason,
  })
  if (rebuilt.recordId !== record.recordId) {
    throw new FileCheckpointError('binding', 'FileCheckpointV1 record ID is invalid')
  }
  if (rebuilt.checksum !== record.checksum) {
    throw new FileCheckpointError('checksum', 'FileCheckpointV1 checksum is invalid')
  }
}

export function validateFileCheckpointTransition(
  previous: FileCheckpointV1,
  next: FileCheckpointV1,
): void {
  validateFileCheckpoint(previous)
  validateFileCheckpoint(next)
  validateCheckpointTransitionSemantics(previous, next)
}

export function selectVerifiedCheckpoint(...records: readonly FileCheckpointV1[]): FileCheckpointV1 {
  let selected: FileCheckpointV1 | undefined
  for (const record of records) {
    validateFileCheckpoint(record)
    if (record.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED &&
        record.commitState !== FILE_CHECKPOINT_COMMIT_PUBLISHED) continue
    if (selected === undefined) {
      selected = record
      continue
    }
    if (!checkpointIdentityEqual(selected, record)) {
      throw new FileCheckpointError('binding', 'committed checkpoints do not share an immutable identity')
    }
    if (record.checkpointGeneration > selected.checkpointGeneration) {
      selected = record
    } else if (record.checkpointGeneration === selected.checkpointGeneration &&
        !checkpointEqualBytes(canonicalPayload(record), canonicalPayload(selected))) {
      throw new FileCheckpointError('crash-boundary', 'committed checkpoints disagree at one generation')
    }
  }
  if (selected === undefined) {
    throw new FileCheckpointError('recovery', 'no verified committed checkpoint exists')
  }
  return selected
}

function canonicalPayload(
  record: Omit<FileCheckpointV1, 'checksum'> | FileCheckpointV1,
): Uint8Array<ArrayBuffer> {
  return checkpointConcatBytes([
    checkpointFramedText(FILE_CHECKPOINT_RECORD_DOMAIN),
    Uint8Array.of(FILE_CHECKPOINT_V1_SCHEMA_VERSION),
    checkpointFramedText(record.ownershipMarker),
    checkpointFramedText(record.namespace),
    identityBytes(record.recordId, FILE_CHECKPOINT_ID_BYTES, 'record ID'),
    identityBytes(record.transferIntentDigest, FILE_CHECKPOINT_ID_BYTES, 'transfer intent digest'),
    identityBytes(record.fileId, FILE_ID_BYTES, 'file ID'),
    identityBytes(record.fileRevision, FILE_REVISION_BYTES, 'file revision'),
    checkpointFramedText(record.canonicalPath),
    uint64Bytes(record.exactSize),
    checkpointFramedText(record.backend),
    identityBytes(record.rootIdentity, FILE_CHECKPOINT_ID_BYTES, 'root identity'),
    identityBytes(record.ownedOutputObject, FILE_CHECKPOINT_ID_BYTES, 'owned output object'),
    uint64Bytes(record.stateGeneration),
    uint64Bytes(record.checkpointGeneration),
    uint32Bytes(record.verifiedRanges.length),
    ...record.verifiedRanges.flatMap((range) => [uint64Bytes(range.start), uint64Bytes(range.end)]),
    Uint8Array.of(record.phase),
    Uint8Array.of(record.commitState),
    Uint8Array.of(record.quarantineReason),
    Uint8Array.of(record.quarantineOrigin),
    Uint8Array.of(record.retirementReason),
  ])
}

function checksumFor(
  record: Omit<FileCheckpointV1, 'checksum'> | FileCheckpointV1,
): Uint8Array<ArrayBuffer> {
  return checkpointChecksumPayload(canonicalPayload(record))
}

export function checkpointChecksumPayload(payload: Uint8Array): Uint8Array<ArrayBuffer> {
  return sha256Sync(checkpointConcatBytes([
    new TextEncoder().encode(`${FILE_CHECKPOINT_CHECKSUM_DOMAIN}\0`),
    payload,
  ]))
}

export function checkpointFramedText(value: string): Uint8Array<ArrayBuffer> {
  return checkpointFramedBytes(new TextEncoder().encode(value))
}

function checkpointFramedBytes(value: Uint8Array): Uint8Array<ArrayBuffer> {
  return checkpointConcatBytes([uint32Bytes(value.byteLength), value])
}

function uint32Bytes(value: number): Uint8Array<ArrayBuffer> {
  const result = new Uint8Array(4)
  new DataView(result.buffer).setUint32(0, value, false)
  return result
}

function uint64Bytes(value: bigint): Uint8Array<ArrayBuffer> {
  const result = new Uint8Array(8)
  new DataView(result.buffer).setBigUint64(0, value, false)
  return result
}

export function checkpointConcatBytes(parts: readonly Uint8Array[]): Uint8Array<ArrayBuffer> {
  const result = new Uint8Array(parts.reduce((total, part) => total + part.byteLength, 0))
  let offset = 0
  for (const part of parts) {
    result.set(part, offset)
    offset += part.byteLength
  }
  return result as Uint8Array<ArrayBuffer>
}

export function checkpointEqualBytes(left: Uint8Array, right: Uint8Array): boolean {
  if (left.byteLength !== right.byteLength) return false
  let difference = 0
  for (let index = 0; index < left.byteLength; index += 1) {
    difference |= (left[index] ?? 0) ^ (right[index] ?? 0)
  }
  return difference === 0
}

export class CheckpointCursor {
  #offset = 0
  readonly #bytes: Uint8Array

  constructor(bytes: Uint8Array) {
    this.#bytes = bytes
  }

  byte(): number {
    if (this.#offset >= this.#bytes.byteLength) {
      throw new FileCheckpointError('invalid', 'checkpoint payload is truncated')
    }
    return this.#bytes[this.#offset++]!
  }

  u32(): number {
    const value = this.take(4)
    return new DataView(value.buffer, value.byteOffset, value.byteLength).getUint32(0, false)
  }

  u64(): bigint {
    const value = this.take(8)
    return new DataView(value.buffer, value.byteOffset, value.byteLength).getBigUint64(0, false)
  }

  text(maxBytes: number): string {
    const length = this.u32()
    if (length > maxBytes) {
      throw new FileCheckpointError('invalid', 'checkpoint text field is too long')
    }
    const bytes = this.take(length)
    try {
      return FILE_CHECKPOINT_TEXT_DECODER.decode(bytes)
    } catch {
      throw new FileCheckpointError('invalid', 'checkpoint text field is invalid UTF-8')
    }
  }

  fixed(length: number, label: string): Uint8Array<ArrayBuffer> {
    const bytes = this.take(length)
    if (bytes.every((byte) => byte === 0)) {
      throw new FileCheckpointError('binding', `${label} must not be zero`)
    }
    return Uint8Array.from(bytes) as Uint8Array<ArrayBuffer>
  }

  take(length: number): Uint8Array {
    if (length < 0 || this.#offset + length > this.#bytes.byteLength) {
      throw new FileCheckpointError('invalid', 'checkpoint payload is truncated')
    }
    const result = this.#bytes.subarray(this.#offset, this.#offset + length)
    this.#offset += length
    return result
  }

  done(): boolean {
    return this.#offset === this.#bytes.byteLength
  }
}

// This synchronous digest keeps record construction valid inside browser storage
// transactions. WebCrypto remains the authority for asynchronous TransferIntent hashes.
function sha256Sync(input: Uint8Array): Uint8Array<ArrayBuffer> {
  const bitLength = BigInt(input.byteLength) * 8n
  const paddedLength = ((input.byteLength + 9 + 63) >> 6) << 6
  const data = new Uint8Array(paddedLength)
  data.set(input)
  data[input.byteLength] = 0x80
  new DataView(data.buffer).setBigUint64(paddedLength - 8, bitLength, false)
  let h0 = 0x6a09e667; let h1 = 0xbb67ae85; let h2 = 0x3c6ef372; let h3 = 0xa54ff53a
  let h4 = 0x510e527f; let h5 = 0x9b05688c; let h6 = 0x1f83d9ab; let h7 = 0x5be0cd19
  const words = new Uint32Array(64)
  for (let offset = 0; offset < data.byteLength; offset += 64) {
    const view = new DataView(data.buffer, offset, 64)
    for (let index = 0; index < 16; index += 1) words[index] = view.getUint32(index * 4, false)
    for (let index = 16; index < 64; index += 1) {
      const x = words[index - 15]!
      const y = words[index - 2]!
      words[index] = (smallSigma1(y) + words[index - 7]! + smallSigma0(x) + words[index - 16]!) >>> 0
    }
    let a = h0; let b = h1; let c = h2; let d = h3; let e = h4; let f = h5; let g = h6; let h = h7
    for (let index = 0; index < 64; index += 1) {
      const t1 = (h + bigSigma1(e) + ((e & f) ^ (~e & g)) + SHA256_CONSTANTS[index]! + words[index]!) >>> 0
      const t2 = (bigSigma0(a) + ((a & b) ^ (a & c) ^ (b & c))) >>> 0
      h = g; g = f; f = e; e = (d + t1) >>> 0; d = c; c = b; b = a; a = (t1 + t2) >>> 0
    }
    h0 = (h0 + a) >>> 0; h1 = (h1 + b) >>> 0; h2 = (h2 + c) >>> 0; h3 = (h3 + d) >>> 0
    h4 = (h4 + e) >>> 0; h5 = (h5 + f) >>> 0; h6 = (h6 + g) >>> 0; h7 = (h7 + h) >>> 0
  }
  const output = new Uint8Array(32)
  const view = new DataView(output.buffer)
  ;[h0, h1, h2, h3, h4, h5, h6, h7].forEach((value, index) =>
    view.setUint32(index * 4, value, false))
  return output as Uint8Array<ArrayBuffer>
}

function rotateRight(value: number, count: number): number {
  return (value >>> count) | (value << (32 - count))
}

function smallSigma0(value: number): number {
  return rotateRight(value, 7) ^ rotateRight(value, 18) ^ (value >>> 3)
}

function smallSigma1(value: number): number {
  return rotateRight(value, 17) ^ rotateRight(value, 19) ^ (value >>> 10)
}

function bigSigma0(value: number): number {
  return rotateRight(value, 2) ^ rotateRight(value, 13) ^ rotateRight(value, 22)
}

function bigSigma1(value: number): number {
  return rotateRight(value, 6) ^ rotateRight(value, 11) ^ rotateRight(value, 25)
}

const SHA256_CONSTANTS = Uint32Array.from([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
])
