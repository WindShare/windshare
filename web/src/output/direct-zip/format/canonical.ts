import { sha256 } from '../../../crypto/digest'

const TEXT_ENCODER = new TextEncoder()
const CANONICAL_SCHEMA_VERSION = 1

export type DirectZipCanonicalBytes = Uint8Array<ArrayBuffer>

export function directZipCanonicalRecord(
  domain: string,
  fields: readonly Uint8Array[],
): DirectZipCanonicalBytes {
  if (!/^[\x21-\x7e]+$/u.test(domain)) {
    throw new TypeError('direct ZIP canonical domain is invalid')
  }
  return concatDirectZipBytes([
    TEXT_ENCODER.encode(`${domain}\0`),
    Uint8Array.of(CANONICAL_SCHEMA_VERSION),
    ...fields,
  ])
}

export function directZipCanonicalFrame(value: Uint8Array): DirectZipCanonicalBytes {
  if (!(value instanceof Uint8Array)) throw new TypeError('direct ZIP canonical frame is invalid')
  return concatDirectZipBytes([directZipCanonicalU64(BigInt(value.byteLength)), value])
}

export function directZipCanonicalU16(value: number): DirectZipCanonicalBytes {
  if (!Number.isInteger(value) || value < 0 || value > 0xffff) {
    throw new TypeError('direct ZIP canonical u16 is invalid')
  }
  const bytes = new Uint8Array(2)
  new DataView(bytes.buffer).setUint16(0, value, false)
  return bytes
}

export function directZipCanonicalU64(value: bigint): DirectZipCanonicalBytes {
  if (typeof value !== 'bigint' || value < 0n || value > 0xffff_ffff_ffff_ffffn) {
    throw new TypeError('direct ZIP canonical u64 is invalid')
  }
  const bytes = new Uint8Array(8)
  new DataView(bytes.buffer).setBigUint64(0, value, false)
  return bytes
}

export function concatDirectZipBytes(
  parts: readonly Uint8Array[],
): DirectZipCanonicalBytes {
  let size = 0
  for (const part of parts) {
    if (!(part instanceof Uint8Array) || size > Number.MAX_SAFE_INTEGER - part.byteLength) {
      throw new RangeError('direct ZIP byte sequence exceeds the allocation bound')
    }
    size += part.byteLength
  }
  const bytes = new Uint8Array(size)
  let offset = 0
  for (const part of parts) {
    bytes.set(part, offset)
    offset += part.byteLength
  }
  return bytes
}

export async function digestDirectZipCanonicalBytes(
  bytes: Uint8Array,
): Promise<DirectZipCanonicalBytes> {
  if (!(bytes instanceof Uint8Array)) throw new TypeError('direct ZIP digest input is invalid')
  return sha256(bytes)
}

export function snapshotDirectZipFixedBytes(
  value: Uint8Array,
  width: number,
  label: string,
  allowZero = false,
): DirectZipCanonicalBytes {
  if (!(value instanceof Uint8Array) || value.byteLength !== width ||
      (!allowZero && value.every((byte) => byte === 0))) {
    throw new TypeError(`${label} must be a ${allowZero ? '' : 'non-zero '}${width}-byte value`)
  }
  return Uint8Array.from(value)
}

export function equalDirectZipBytes(left: Uint8Array, right: Uint8Array): boolean {
  if (!(left instanceof Uint8Array) || !(right instanceof Uint8Array) ||
      left.byteLength !== right.byteLength) return false
  let difference = 0
  for (let index = 0; index < left.byteLength; index += 1) {
    difference |= (left[index] ?? 0) ^ (right[index] ?? 0)
  }
  return difference === 0
}
