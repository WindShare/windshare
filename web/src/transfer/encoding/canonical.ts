import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import { decodeBase64Url, encodeBase64Url } from '../../crypto/bytes'
import { sha256 } from '../../crypto/digest'

const TEXT_ENCODER = new TextEncoder()

export type CanonicalBytes = Uint8Array<ArrayBuffer>

export function canonicalRecord(
  domain: string,
  version: number,
  fields: readonly Uint8Array[],
): CanonicalBytes {
  if (!/^[\x21-\x7e]+$/u.test(domain) || !Number.isInteger(version) ||
      version < 1 || version > 0xff) {
    throw new TypeError('canonical record domain or version is invalid')
  }
  return concatCanonicalBytes([
    TEXT_ENCODER.encode(`${domain}\0`),
    Uint8Array.of(version),
    ...fields,
  ])
}

export function canonicalFrame(value: Uint8Array): CanonicalBytes {
  return concatCanonicalBytes([canonicalU64(BigInt(value.byteLength)), value])
}

export function canonicalText(value: string): CanonicalBytes {
  if (typeof value !== 'string' || value.normalize('NFC') !== value ||
      !isWellFormedUnicode(value)) {
    throw new TypeError('canonical text must be well-formed NFC Unicode')
  }
  return TEXT_ENCODER.encode(value) as CanonicalBytes
}

function isWellFormedUnicode(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const codeUnit = value.charCodeAt(index)
    if (codeUnit < 0xd800 || codeUnit > 0xdfff) continue
    if (codeUnit > 0xdbff || index + 1 >= value.length) return false
    const low = value.charCodeAt(index + 1)
    if (low < 0xdc00 || low > 0xdfff) return false
    index += 1
  }
  return true
}

export function canonicalU8(value: number): CanonicalBytes {
  if (!Number.isInteger(value) || value < 0 || value > 0xff) {
    throw new TypeError('canonical u8 is invalid')
  }
  return Uint8Array.of(value) as CanonicalBytes
}

export function canonicalBoolean(value: boolean): CanonicalBytes {
  if (typeof value !== 'boolean') throw new TypeError('canonical boolean is invalid')
  return canonicalU8(value ? 1 : 0)
}

export function canonicalU32(value: number): CanonicalBytes {
  if (!Number.isInteger(value) || value < 0 || value > 0xffff_ffff) {
    throw new TypeError('canonical u32 is invalid')
  }
  const bytes = new Uint8Array(4)
  new DataView(bytes.buffer).setUint32(0, value, false)
  return bytes as CanonicalBytes
}

export function canonicalI64(value: bigint): CanonicalBytes {
  if (typeof value !== 'bigint' || value < -0x8000_0000_0000_0000n ||
      value > 0x7fff_ffff_ffff_ffffn) {
    throw new TypeError('canonical i64 is invalid')
  }
  const bytes = new Uint8Array(8)
  new DataView(bytes.buffer).setBigInt64(0, value, false)
  return bytes as CanonicalBytes
}

export function canonicalU64(value: bigint): CanonicalBytes {
  if (typeof value !== 'bigint' || value < 0n || value > 0xffff_ffff_ffff_ffffn) {
    throw new TypeError('canonical u64 is invalid')
  }
  const bytes = new Uint8Array(8)
  new DataView(bytes.buffer).setBigUint64(0, value, false)
  return bytes as CanonicalBytes
}

export function canonicalUnixMilliseconds(value: number): CanonicalBytes {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new TypeError('Unix milliseconds must be a non-negative safe integer')
  }
  return canonicalU64(BigInt(value))
}

export function canonicalIdentity(
  value: string,
  width: number,
  label: string,
): CanonicalBytes {
  const bytes = typeof value === 'string' ? decodeBase64Url(value) : undefined
  if (bytes === undefined || bytes.byteLength !== width ||
      bytes.every((byte) => byte === 0) || encodeBase64Url(bytes) !== value) {
    throw new TypeError(`${label} must be a canonical non-zero ${width}-byte identity`)
  }
  return Uint8Array.from(bytes) as CanonicalBytes
}

export function snapshotIdentity(value: string, width: number, label: string): string {
  return encodeBase64Url(canonicalIdentity(value, width, label))
}

export function canonicalPath(path: readonly string[]): CanonicalBytes {
  const snapshot = snapshotPortableCatalogPath(path)
  return concatCanonicalBytes([
    canonicalU64(BigInt(snapshot.length)),
    ...snapshot.map((segment) => canonicalFrame(canonicalText(segment))),
  ])
}

export async function canonicalDigest(bytes: Uint8Array): Promise<string> {
  return encodeBase64Url(await sha256(bytes))
}

export function assertCanonicalRecordDomain(
  bytes: Uint8Array,
  domain: string,
  version: number,
): void {
  const prefix = concatCanonicalBytes([
    TEXT_ENCODER.encode(`${domain}\0`),
    canonicalU8(version),
  ])
  if (bytes.byteLength < prefix.byteLength ||
      !equalCanonicalBytes(bytes.subarray(0, prefix.byteLength), prefix)) {
    throw new TypeError('canonical record domain or version does not match its kind')
  }
}

export function concatCanonicalBytes(parts: readonly Uint8Array[]): CanonicalBytes {
  const size = parts.reduce((total, part) => total + part.byteLength, 0)
  if (!Number.isSafeInteger(size)) throw new TypeError('canonical byte length overflow')
  const output = new Uint8Array(size)
  let offset = 0
  for (const part of parts) {
    output.set(part, offset)
    offset += part.byteLength
  }
  return output as CanonicalBytes
}

export function equalCanonicalBytes(left: Uint8Array, right: Uint8Array): boolean {
  if (left.byteLength !== right.byteLength) return false
  let difference = 0
  for (let index = 0; index < left.byteLength; index += 1) {
    difference |= (left[index] ?? 0) ^ (right[index] ?? 0)
  }
  return difference === 0
}

export function snapshotCanonicalBytes(bytes: Uint8Array): CanonicalBytes {
  if (!(bytes instanceof Uint8Array) || bytes.byteLength === 0) {
    throw new TypeError('canonical bytes must not be empty')
  }
  return Uint8Array.from(bytes) as CanonicalBytes
}
