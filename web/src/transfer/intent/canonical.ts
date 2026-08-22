import {
  canonicalizePortableCatalogPath,
  V2_CATALOG_NAME_BYTES,
  V2_CATALOG_PATH_BYTES,
  V2_CATALOG_PATH_DEPTH,
} from '../../catalog/path-policy'
import { encodeBase64Url, equalBytes } from '../../crypto/bytes'
import { sha256 } from '../../crypto/digest'
import { requireIdentity } from './identity'
import {
  AUTHORITY_REFERENCE_BYTES,
  type CanonicalBytes,
  type CanonicalDigestValue,
  type CanonicalValue,
} from './model'

export const TEXT_ENCODER = new TextEncoder()
export const TEXT_DECODER = new TextDecoder('utf-8', { fatal: true })
export const MAX_CANONICAL_PATH_ENCODING_BYTES = 8 +
  V2_CATALOG_PATH_DEPTH * 8 + V2_CATALOG_PATH_BYTES
export const INVALID_RECEIVE_INTENT_CANONICAL_BYTES = 'receive intent canonical bytes are invalid'
export const SELECTION_SPEC_DOMAIN = 'windshare/selection-spec/v1'
export const ARTIFACT_SPEC_DOMAIN = 'windshare/artifact-spec/v1'
export const RESULT_ROOT_LAYOUT_DOMAIN = 'windshare/result-root-layout/v1'
export const DESTINATION_RESERVATION_DOMAIN = 'windshare/destination-reservation/v3'
export const FSA_OWNED_FILE_BINDING_DOMAIN = 'windshare/fsa-owned-file-binding/v1'
export const ARTIFACT_CHOICE_IDENTITY_DOMAIN = 'windshare/artifact-choice/v1'
export const WORKSPACE_BINDING_DOMAIN = 'windshare/workspace-binding/v1'
export const PORTABLE_BINDING_DOMAIN = 'windshare/portable-binding/v1'
export const MATERIALIZATION_PLAN_DOMAIN = 'windshare/materialization-plan/v3'
export const RECEIVE_INTENT_DOMAIN = 'windshare/receive-intent/v3'
export const NAME_COLLISION_DOMAIN = 'windshare/name-collision/v1'

const CANONICAL_SCHEMA_VERSION = 1
const VALID_CANONICAL_VALUES = new WeakSet<object>()

// Lengths are checked as bigint before conversion so hostile persisted frames
// cannot wrap JavaScript numbers or make a truncated image look complete.
export class CanonicalDecoder {
  private offset = 0
  private readonly encoded: CanonicalBytes

  public constructor(encoded: CanonicalBytes) {
    this.encoded = encoded
  }

  public static record(
    encoded: CanonicalBytes,
    domain: string,
    version = CANONICAL_SCHEMA_VERSION,
  ): CanonicalDecoder {
    const cursor = new CanonicalDecoder(encoded)
    for (const expected of TEXT_ENCODER.encode(domain)) {
      if (cursor.readRawByte() !== expected) invalidDecodedCanonicalBytes()
    }
    if (cursor.readRawByte() !== 0 || cursor.readRawByte() !== version) {
      return invalidDecodedCanonicalBytes()
    }
    return cursor
  }

  public get remaining(): number {
    return this.encoded.byteLength - this.offset
  }

  public readRawByte(): number {
    if (this.remaining < 1) return invalidDecodedCanonicalBytes()
    return this.encoded[this.offset++]!
  }

  public readRawUint64(): bigint {
    if (this.remaining < 8) return invalidDecodedCanonicalBytes()
    const value = new DataView(
      this.encoded.buffer,
      this.encoded.byteOffset + this.offset,
      8,
    ).getBigUint64(0)
    this.offset += 8
    return value
  }

  public readFrame(maximum: number): CanonicalBytes {
    if (!Number.isSafeInteger(maximum) || maximum < 0) return invalidDecodedCanonicalBytes()
    const length = this.readRawUint64()
    if (length > BigInt(maximum) || length > BigInt(this.remaining)) {
      return invalidDecodedCanonicalBytes()
    }
    const size = Number(length)
    const value = this.encoded.slice(this.offset, this.offset + size)
    this.offset += size
    return value
  }

  public readFixedFrame(width: number): CanonicalBytes {
    const value = this.readFrame(width)
    if (value.byteLength !== width) return invalidDecodedCanonicalBytes()
    return value
  }

  public readFramedByte(): number {
    return this.readFixedFrame(1)[0]!
  }

  public readFramedBoolean(): boolean {
    const value = this.readFramedByte()
    if (value !== 0 && value !== 1) return invalidDecodedCanonicalBytes()
    return value === 1
  }

  public readFramedUint32(): number {
    const value = this.readFixedFrame(4)
    return new DataView(value.buffer, value.byteOffset, value.byteLength).getUint32(0)
  }

  public readFramedUint64(): bigint {
    const value = this.readFixedFrame(8)
    return new DataView(value.buffer, value.byteOffset, value.byteLength).getBigUint64(0)
  }

  public requireDone(): void {
    if (this.remaining !== 0) invalidDecodedCanonicalBytes()
  }
}

export function decodeCanonicalPath(encoded: CanonicalBytes): string {
  const cursor = new CanonicalDecoder(encoded)
  const segmentCount = decodedCount(cursor.readRawUint64(), 1, V2_CATALOG_PATH_DEPTH)
  const segments: string[] = []
  let pathBytes = 0
  for (let index = 0; index < segmentCount; index += 1) {
    const segmentBytes = cursor.readFrame(V2_CATALOG_NAME_BYTES)
    pathBytes += segmentBytes.byteLength + (index === 0 ? 0 : 1)
    if (pathBytes > V2_CATALOG_PATH_BYTES) invalidDecodedCanonicalBytes()
    segments.push(decodeCanonicalText(segmentBytes))
  }
  cursor.requireDone()
  const path = requireCanonicalPath(segments.join('/'))
  requireDecodedCanonicalBytes(encoded, canonicalPathBytes(path), 'canonical path')
  return path
}

export function decodeCanonicalText(encoded: CanonicalBytes): string {
  return TEXT_DECODER.decode(encoded)
}

export function decodedCount(value: bigint, minimum: number, maximum: number): number {
  if (!Number.isSafeInteger(minimum) || !Number.isSafeInteger(maximum) ||
      minimum < 0 || maximum < minimum ||
      value < BigInt(minimum) || value > BigInt(maximum)) {
    return invalidDecodedCanonicalBytes()
  }
  return Number(value)
}

export function requireDecodedCanonicalBytes(
  encoded: Uint8Array,
  rebuilt: Uint8Array,
  label: string,
): void {
  if (!equalBytes(encoded, rebuilt)) {
    throw new TypeError(label + ' bytes are not canonical')
  }
}

export function invalidDecodedCanonicalBytes(): never {
  throw new TypeError(INVALID_RECEIVE_INTENT_CANONICAL_BYTES)
}

export function requireCanonicalPath(value: string): string {
  if (typeof value !== 'string') throw new TypeError('canonical path must be text')
  const canonical = canonicalizePortableCatalogPath(value)
  if (canonical !== value) throw new TypeError('path is not in canonical form')
  return canonical
}

export function canonicalPathBytes(pathInput: string): CanonicalBytes {
  const path = requireCanonicalPath(pathInput)
  const segments = path.split('/')
  return concat([
    uint64(BigInt(segments.length)),
    ...segments.map((segment) => frame(TEXT_ENCODER.encode(segment))),
  ])
}

export function canonicalValue<T extends object>(
  fields: T,
  canonicalBytes: Uint8Array,
): T & CanonicalValue {
  const stored = Uint8Array.from(canonicalBytes)
  const value = { ...fields } as T & CanonicalValue
  Object.defineProperty(value, 'canonicalBytes', {
    enumerable: true,
    get: () => Uint8Array.from(stored),
  })
  const frozen = Object.freeze(value)
  VALID_CANONICAL_VALUES.add(frozen)
  return frozen
}

export function requireCanonicalValueProvenance(value: object, label: string): void {
  if (!VALID_CANONICAL_VALUES.has(value)) {
    throw new TypeError(label + ' must be created or validated by the canonical codec')
  }
}

export function canonicalDigestValue<T extends object>(
  fields: T,
  digest: string,
  canonicalBytes: Uint8Array,
): T & CanonicalDigestValue {
  requireIdentity(digest, AUTHORITY_REFERENCE_BYTES, 'canonical digest')
  return canonicalValue({ ...fields, digest }, canonicalBytes)
}

export function requireSameCanonicalValue<T extends CanonicalValue>(
  input: T,
  rebuilt: T,
  label: string,
): T {
  if (!(input.canonicalBytes instanceof Uint8Array) ||
      !equalBytes(input.canonicalBytes, rebuilt.canonicalBytes)) {
    throw new TypeError(label + ' canonical bytes do not match its semantic fields')
  }
  return rebuilt
}

export function requireSameDigestRecord<T extends CanonicalDigestValue>(
  input: T,
  rebuilt: T,
  label: string,
): T {
  requireSameCanonicalValue(input, rebuilt, label)
  requireIdentity(input.digest, AUTHORITY_REFERENCE_BYTES, label + ' digest')
  if (input.digest !== rebuilt.digest) {
    throw new TypeError(label + ' digest does not match its canonical bytes')
  }
  return rebuilt
}

export async function digestText(value: Uint8Array): Promise<string> {
  return encodeBase64Url(await sha256(value))
}

export function canonicalRecord(
  domain: string,
  fields: readonly Uint8Array[],
  version = CANONICAL_SCHEMA_VERSION,
): CanonicalBytes {
  if (!Number.isInteger(version) || version < 1 || version > 0xff) {
    throw new RangeError('canonical record version must be a non-zero byte')
  }
  return concat([
    TEXT_ENCODER.encode(domain),
    Uint8Array.of(0, version),
    ...fields,
  ])
}

export function frame(value: Uint8Array): CanonicalBytes {
  return concat([uint64(BigInt(value.byteLength)), value])
}

export function uint32(value: number): CanonicalBytes {
  requireUint32(value, 'u32')
  const result = new Uint8Array(4)
  new DataView(result.buffer).setUint32(0, value)
  return result
}

export function uint64(value: bigint): CanonicalBytes {
  if (value < 0n || value > 0xffff_ffff_ffff_ffffn) throw new RangeError('u64 is outside its range')
  const result = new Uint8Array(8)
  new DataView(result.buffer).setBigUint64(0, value)
  return result
}

export function requireUint32(value: number, label: string): void {
  if (!Number.isInteger(value) || value < 0 || value > 0xffff_ffff) {
    throw new RangeError(label + ' must be an unsigned 32-bit integer')
  }
}

export function concat(parts: readonly Uint8Array[]): CanonicalBytes {
  const total = parts.reduce((sum, part) => sum + part.byteLength, 0)
  const output = new Uint8Array(total)
  let offset = 0
  for (const part of parts) {
    output.set(part, offset)
    offset += part.byteLength
  }
  return output
}

export function compareTextBytes(left: string, right: string): number {
  return compareBytes(TEXT_ENCODER.encode(left), TEXT_ENCODER.encode(right))
}

export function compareBytes(left: Uint8Array, right: Uint8Array): number {
  const length = Math.min(left.byteLength, right.byteLength)
  for (let index = 0; index < length; index += 1) {
    const difference = left[index]! - right[index]!
    if (difference !== 0) return difference
  }
  return left.byteLength - right.byteLength
}
