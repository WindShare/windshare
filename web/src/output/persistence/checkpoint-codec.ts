import { encodeBase64Url } from '../../crypto/bytes'
import {
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FileCheckpointError,
} from './checkpoint-lifecycle'
import {
  FILE_CHECKPOINT_ID_BYTES,
  FILE_CHECKPOINT_MAX_RANGES,
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
  FILE_CHECKPOINT_V2_SCHEMA_VERSION,
  FILE_ID_BYTES,
  FILE_REVISION_BYTES,
  OPERATION_ID_BYTES,
  checkpointIdentityEqual,
  identityBytes,
  normalizeFileCheckpointSpec,
  validateCheckpointTransitionSemantics,
  type FileCheckpointSpec,
  type FileCheckpointV2,
  type NormalizedFileCheckpointSpec,
} from './checkpoint-model'

export const FILE_CHECKPOINT_RECORD_DOMAIN = 'windshare/file-checkpoint-record/v2' as const
export const FILE_CHECKPOINT_CHECKSUM_DOMAIN = 'windshare/file-checkpoint-checksum/v2' as const
export const FILE_CHECKPOINT_RECORD_MAGIC = 'WSFCPV2\0' as const

const TEXT_ENCODER = new TextEncoder()
const TEXT_DECODER = new TextDecoder('utf-8', { fatal: true })
const RECORD_PREFIX = TEXT_ENCODER.encode(`${FILE_CHECKPOINT_RECORD_DOMAIN}\0`)
const CHECKSUM_PREFIX = TEXT_ENCODER.encode(`${FILE_CHECKPOINT_CHECKSUM_DOMAIN}\0`)
const RECORD_MAGIC_BYTES = TEXT_ENCODER.encode(FILE_CHECKPOINT_RECORD_MAGIC)
const CHECKSUM_BYTES = 32
const MAX_PAYLOAD_BYTES = 4 * 1024 * 1024
const MAX_TEXT_FIELD_BYTES = 32 * 1024

export function newFileCheckpointV2(spec: FileCheckpointSpec): FileCheckpointV2 {
  const normalized = normalizeFileCheckpointSpec(spec)
  const recordId = encodeBase64Url(sha256Sync(immutableIdentityBytes(normalized)))
  const withoutChecksum: Omit<FileCheckpointV2, 'checksum'> = Object.freeze({
    schemaVersion: FILE_CHECKPOINT_V2_SCHEMA_VERSION,
    ownershipMarker: normalized.ownershipMarker,
    namespace: normalized.namespace,
    recordId,
    operationId: encodeBase64Url(normalized.operationId),
    receiveIntentDigest: encodeBase64Url(normalized.receiveIntentDigest),
    materializationBindingDigest: encodeBase64Url(normalized.materializationBindingDigest),
    fileId: encodeBase64Url(normalized.fileId),
    fileRevision: encodeBase64Url(normalized.fileRevision),
    canonicalPath: normalized.canonicalPath,
    exactSize: normalized.exactSize,
    materializerKind: normalized.materializerKind,
    authorityRef: encodeBase64Url(normalized.authorityRef),
    ownedObjectId: encodeBase64Url(normalized.ownedObjectId),
    stateGeneration: normalized.stateGeneration,
    checkpointGeneration: normalized.checkpointGeneration,
    verifiedRanges: normalized.verifiedRanges,
    phase: normalized.phase,
    commitState: normalized.commitState,
    quarantineReason: normalized.quarantineReason,
    quarantineOrigin: normalized.quarantineOrigin,
    retirementReason: normalized.retirementReason,
  })
  return Object.freeze({
    ...withoutChecksum,
    checksum: encodeBase64Url(checkpointChecksumPayload(canonicalPayload(withoutChecksum))),
  })
}

export function canonicalFileCheckpointBytes(
  record: FileCheckpointV2,
): Uint8Array<ArrayBuffer> {
  return canonicalPayload(record)
}

export function encodeFileCheckpointV2(record: FileCheckpointV2): Uint8Array<ArrayBuffer> {
  validateFileCheckpoint(record)
  const payload = canonicalPayload(record)
  return checkpointConcatBytes([
    RECORD_MAGIC_BYTES,
    uint32Bytes(payload.byteLength),
    payload,
    identityBytes(record.checksum, CHECKSUM_BYTES, 'checkpoint checksum'),
  ])
}

export function decodeFileCheckpointV2(encoded: Uint8Array): FileCheckpointV2 {
  const cursor = new CheckpointCursor(encoded)
  cursor.expect(RECORD_MAGIC_BYTES, 'FileCheckpointV2 magic')
  const payloadLength = cursor.u32()
  if (payloadLength > MAX_PAYLOAD_BYTES) {
    throw new FileCheckpointError('invalid', 'FileCheckpointV2 payload exceeds its bound')
  }
  const payload = cursor.take(payloadLength)
  const checksum = cursor.fixed(CHECKSUM_BYTES, 'checkpoint checksum')
  if (!cursor.done()) {
    throw new FileCheckpointError('non-canonical', 'FileCheckpointV2 envelope has trailing bytes')
  }
  if (!checkpointEqualBytes(checkpointChecksumPayload(payload), checksum)) {
    throw new FileCheckpointError('checksum', 'FileCheckpointV2 checksum is invalid')
  }
  const decoded = decodeCanonicalPayload(payload)
  if (decoded.checksum !== encodeBase64Url(checksum)) {
    throw new FileCheckpointError('checksum', 'FileCheckpointV2 checksum does not bind its payload')
  }
  if (!checkpointEqualBytes(encodeFileCheckpointV2(decoded), encoded)) {
    throw new FileCheckpointError('non-canonical', 'FileCheckpointV2 encoding is not canonical')
  }
  return decoded
}

export function validateFileCheckpoint(record: FileCheckpointV2): void {
  const rebuilt = newFileCheckpointV2(record)
  if (record.schemaVersion !== FILE_CHECKPOINT_V2_SCHEMA_VERSION ||
      record.ownershipMarker !== FILE_CHECKPOINT_OWNERSHIP_MARKER ||
      record.namespace !== FILE_CHECKPOINT_NAMESPACE ||
      record.recordId !== rebuilt.recordId) {
    throw new FileCheckpointError('binding', 'FileCheckpointV2 immutable binding is invalid')
  }
  if (record.checksum !== rebuilt.checksum ||
      !checkpointEqualBytes(canonicalPayload(record), canonicalPayload(rebuilt))) {
    throw new FileCheckpointError('checksum', 'FileCheckpointV2 canonical checksum is invalid')
  }
}

export function validateFileCheckpointTransition(
  previous: FileCheckpointV2,
  next: FileCheckpointV2,
): void {
  validateFileCheckpoint(previous)
  validateFileCheckpoint(next)
  validateCheckpointTransitionSemantics(previous, next)
}

export function selectVerifiedCheckpoint(
  ...records: readonly FileCheckpointV2[]
): FileCheckpointV2 {
  let selected: FileCheckpointV2 | undefined
  for (const record of records) {
    validateFileCheckpoint(record)
    if (record.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED) continue
    if (selected === undefined) {
      selected = record
      continue
    }
    if (!checkpointIdentityEqual(selected, record)) {
      throw new FileCheckpointError('binding', 'committed checkpoints do not share identity')
    }
    if (record.checkpointGeneration > selected.checkpointGeneration) selected = record
    else if (record.checkpointGeneration === selected.checkpointGeneration &&
        !checkpointEqualBytes(canonicalPayload(record), canonicalPayload(selected))) {
      throw new FileCheckpointError('crash-boundary', 'committed checkpoints disagree at one generation')
    }
  }
  if (selected === undefined) {
    throw new FileCheckpointError('recovery', 'no verified committed checkpoint exists')
  }
  return selected
}

/** The storage checksum is the final record digest referenced by aggregate manifests. */
export function fileCheckpointDigest(record: FileCheckpointV2): string {
  validateFileCheckpoint(record)
  return record.checksum
}

export function checkpointChecksumPayload(payload: Uint8Array): Uint8Array<ArrayBuffer> {
  return sha256Sync(checkpointConcatBytes([
    CHECKSUM_PREFIX,
    Uint8Array.of(FILE_CHECKPOINT_V2_SCHEMA_VERSION),
    checkpointFramedBytes(payload),
  ]))
}

function decodeCanonicalPayload(payload: Uint8Array): FileCheckpointV2 {
  const cursor = new CheckpointCursor(payload)
  cursor.expect(RECORD_PREFIX, 'FileCheckpointV2 record domain')
  if (cursor.byte() !== FILE_CHECKPOINT_V2_SCHEMA_VERSION) {
    throw new FileCheckpointError('invalid', 'FileCheckpointV2 schema version is invalid')
  }
  const ownershipMarker = cursor.text(MAX_TEXT_FIELD_BYTES)
  const namespace = cursor.text(MAX_TEXT_FIELD_BYTES)
  const recordId = encodeBase64Url(cursor.identity(FILE_CHECKPOINT_ID_BYTES, 'record ID'))
  const spec: FileCheckpointSpec = {
    ownershipMarker,
    namespace,
    operationId: cursor.identity(OPERATION_ID_BYTES, 'operation ID'),
    receiveIntentDigest: cursor.identity(FILE_CHECKPOINT_ID_BYTES, 'receive intent digest'),
    materializationBindingDigest: cursor.identity(
      FILE_CHECKPOINT_ID_BYTES,
      'materialization binding digest',
    ),
    fileId: cursor.identity(FILE_ID_BYTES, 'file ID'),
    fileRevision: cursor.identity(FILE_REVISION_BYTES, 'file revision'),
    canonicalPath: decodeCanonicalPath(cursor.frame(MAX_TEXT_FIELD_BYTES)),
    exactSize: cursor.framedU64('exact size'),
    materializerKind: cursor.framedByte('materializer kind'),
    authorityRef: cursor.identity(FILE_CHECKPOINT_ID_BYTES, 'authority reference'),
    ownedObjectId: cursor.identity(FILE_CHECKPOINT_ID_BYTES, 'owned object ID'),
    stateGeneration: cursor.framedU64('state generation'),
    checkpointGeneration: cursor.framedU64('checkpoint generation'),
    verifiedRanges: decodeRanges(cursor),
    phase: cursor.framedByte('checkpoint phase'),
    commitState: cursor.framedByte('checkpoint commit state'),
    quarantineReason: cursor.framedByte('checkpoint quarantine reason'),
    quarantineOrigin: cursor.framedByte('checkpoint quarantine origin'),
    retirementReason: cursor.framedByte('checkpoint retirement reason'),
  }
  if (!cursor.done()) {
    throw new FileCheckpointError('non-canonical', 'FileCheckpointV2 payload has trailing bytes')
  }
  const rebuilt = newFileCheckpointV2(spec)
  if (rebuilt.recordId !== recordId) {
    throw new FileCheckpointError('binding', 'FileCheckpointV2 record ID is not derived identity')
  }
  return rebuilt
}

function decodeRanges(cursor: CheckpointCursor): readonly { start: bigint; end: bigint }[] {
  const count = cursor.u64()
  if (count > BigInt(FILE_CHECKPOINT_MAX_RANGES)) {
    throw new FileCheckpointError('invalid', 'FileCheckpointV2 range count exceeds its bound')
  }
  const ranges: { start: bigint; end: bigint }[] = []
  for (let index = 0n; index < count; index += 1n) {
    ranges.push(Object.freeze({
      start: cursor.framedU64('range start'),
      end: cursor.framedU64('range end'),
    }))
  }
  return Object.freeze(ranges)
}

function decodeCanonicalPath(bytes: Uint8Array): readonly string[] {
  const cursor = new CheckpointCursor(bytes)
  const count = cursor.u64()
  if (count === 0n || count > 256n) {
    throw new FileCheckpointError('binding', 'checkpoint path segment count is invalid')
  }
  const segments: string[] = []
  for (let index = 0n; index < count; index += 1n) {
    segments.push(cursor.text(MAX_TEXT_FIELD_BYTES))
  }
  if (!cursor.done()) {
    throw new FileCheckpointError('non-canonical', 'checkpoint path has trailing bytes')
  }
  return segments
}

function canonicalPayload(
  record: Omit<FileCheckpointV2, 'checksum'> | FileCheckpointV2,
): Uint8Array<ArrayBuffer> {
  return checkpointConcatBytes([
    RECORD_PREFIX,
    Uint8Array.of(FILE_CHECKPOINT_V2_SCHEMA_VERSION),
    checkpointFramedText(record.ownershipMarker),
    checkpointFramedText(record.namespace),
    checkpointFramedBytes(identityBytes(record.recordId, FILE_CHECKPOINT_ID_BYTES, 'record ID')),
    checkpointFramedBytes(identityBytes(record.operationId, OPERATION_ID_BYTES, 'operation ID')),
    checkpointFramedBytes(identityBytes(
      record.receiveIntentDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'receive intent digest',
    )),
    checkpointFramedBytes(identityBytes(
      record.materializationBindingDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'materialization binding digest',
    )),
    checkpointFramedBytes(identityBytes(record.fileId, FILE_ID_BYTES, 'file ID')),
    checkpointFramedBytes(identityBytes(record.fileRevision, FILE_REVISION_BYTES, 'file revision')),
    checkpointFramedBytes(canonicalPathBytes(record.canonicalPath)),
    checkpointFramedBytes(uint64Bytes(record.exactSize)),
    checkpointFramedBytes(Uint8Array.of(record.materializerKind)),
    checkpointFramedBytes(identityBytes(
      record.authorityRef,
      FILE_CHECKPOINT_ID_BYTES,
      'authority reference',
    )),
    checkpointFramedBytes(identityBytes(
      record.ownedObjectId,
      FILE_CHECKPOINT_ID_BYTES,
      'owned object ID',
    )),
    checkpointFramedBytes(uint64Bytes(record.stateGeneration)),
    checkpointFramedBytes(uint64Bytes(record.checkpointGeneration)),
    uint64Bytes(BigInt(record.verifiedRanges.length)),
    ...record.verifiedRanges.flatMap((range) => [
      checkpointFramedBytes(uint64Bytes(range.start)),
      checkpointFramedBytes(uint64Bytes(range.end)),
    ]),
    checkpointFramedBytes(Uint8Array.of(record.phase)),
    checkpointFramedBytes(Uint8Array.of(record.commitState)),
    checkpointFramedBytes(Uint8Array.of(record.quarantineReason)),
    checkpointFramedBytes(Uint8Array.of(record.quarantineOrigin)),
    checkpointFramedBytes(Uint8Array.of(record.retirementReason)),
  ])
}

function immutableIdentityBytes(record: NormalizedFileCheckpointSpec): Uint8Array<ArrayBuffer> {
  return checkpointConcatBytes([
    RECORD_PREFIX,
    Uint8Array.of(FILE_CHECKPOINT_V2_SCHEMA_VERSION),
    checkpointFramedText(record.ownershipMarker),
    checkpointFramedText(record.namespace),
    checkpointFramedBytes(record.operationId),
    checkpointFramedBytes(record.receiveIntentDigest),
    checkpointFramedBytes(record.materializationBindingDigest),
    checkpointFramedBytes(record.fileId),
    checkpointFramedBytes(record.fileRevision),
    checkpointFramedBytes(canonicalPathBytes(record.canonicalPath)),
    checkpointFramedBytes(uint64Bytes(record.exactSize)),
    checkpointFramedBytes(Uint8Array.of(record.materializerKind)),
    checkpointFramedBytes(record.authorityRef),
    checkpointFramedBytes(record.ownedObjectId),
  ])
}

function canonicalPathBytes(path: readonly string[]): Uint8Array<ArrayBuffer> {
  return checkpointConcatBytes([
    uint64Bytes(BigInt(path.length)),
    ...path.map(checkpointFramedText),
  ])
}

export function checkpointFramedText(value: string): Uint8Array<ArrayBuffer> {
  return checkpointFramedBytes(TEXT_ENCODER.encode(value))
}

export function checkpointFramedBytes(value: Uint8Array): Uint8Array<ArrayBuffer> {
  return checkpointConcatBytes([uint64Bytes(BigInt(value.byteLength)), value])
}

function uint32Bytes(value: number): Uint8Array<ArrayBuffer> {
  if (!Number.isInteger(value) || value < 0 || value > 0xffff_ffff) {
    throw new FileCheckpointError('invalid', 'checkpoint u32 is invalid')
  }
  const result = new Uint8Array(4)
  new DataView(result.buffer).setUint32(0, value, false)
  return result
}

export function uint64Bytes(value: bigint): Uint8Array<ArrayBuffer> {
  if (typeof value !== 'bigint' || value < 0n || value > 0xffff_ffff_ffff_ffffn) {
    throw new FileCheckpointError('invalid', 'checkpoint u64 is invalid')
  }
  const result = new Uint8Array(8)
  new DataView(result.buffer).setBigUint64(0, value, false)
  return result
}

export function checkpointConcatBytes(parts: readonly Uint8Array[]): Uint8Array<ArrayBuffer> {
  const byteLength = parts.reduce((total, part) => total + part.byteLength, 0)
  const result = new Uint8Array(byteLength)
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

  expect(expected: Uint8Array, label: string): void {
    if (!checkpointEqualBytes(this.take(expected.byteLength), expected)) {
      throw new FileCheckpointError('non-canonical', `${label} is invalid`)
    }
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

  frame(maxBytes: number): Uint8Array {
    const length = this.u64()
    if (length > BigInt(maxBytes)) {
      throw new FileCheckpointError('invalid', 'checkpoint frame exceeds its bound')
    }
    return this.take(Number(length))
  }

  text(maxBytes: number): string {
    try {
      return TEXT_DECODER.decode(this.frame(maxBytes))
    } catch {
      throw new FileCheckpointError('invalid', 'checkpoint text is not canonical UTF-8')
    }
  }

  identity(length: number, label: string): Uint8Array<ArrayBuffer> {
    const value = this.frame(length)
    if (value.byteLength !== length || value.every((byte) => byte === 0)) {
      throw new FileCheckpointError('binding', `${label} is invalid`)
    }
    return Uint8Array.from(value) as Uint8Array<ArrayBuffer>
  }

  framedU64(label: string): bigint {
    const value = this.frame(8)
    if (value.byteLength !== 8) {
      throw new FileCheckpointError('invalid', `${label} is not a canonical u64`)
    }
    return new DataView(value.buffer, value.byteOffset, value.byteLength).getBigUint64(0, false)
  }

  framedByte(label: string): number {
    const value = this.frame(1)
    if (value.byteLength !== 1) {
      throw new FileCheckpointError('invalid', `${label} is not a canonical u8`)
    }
    return value[0]!
  }

  fixed(length: number, label: string): Uint8Array<ArrayBuffer> {
    const value = this.take(length)
    if (value.every((byte) => byte === 0)) {
      throw new FileCheckpointError('binding', `${label} must not be zero`)
    }
    return Uint8Array.from(value) as Uint8Array<ArrayBuffer>
  }

  take(length: number): Uint8Array {
    if (!Number.isSafeInteger(length) || length < 0 || this.#offset + length > this.#bytes.byteLength) {
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

// Repository transitions need a digest before IndexedDB may auto-commit, so this
// deliberately small synchronous primitive keeps canonical validation transaction-safe.
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

// IndexedDB lineage classification must not yield to WebCrypto while a
// read-write transaction is live, so local derived indexes share this exact
// synchronous primitive with the physical checkpoint codec.
export function checkpointSha256Sync(input: Uint8Array): Uint8Array<ArrayBuffer> {
  return sha256Sync(input)
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
