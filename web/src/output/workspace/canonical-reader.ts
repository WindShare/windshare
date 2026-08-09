import { encodeBase64Url } from '../../crypto/bytes'
import {
  assertCanonicalRecordDomain,
  snapshotCanonicalBytes,
  snapshotIdentity,
  type CanonicalBytes,
} from './canonical'

const TEXT_ENCODER = new TextEncoder()
const U64_BYTES = 8

/**
 * Decodes only the framing shared by workspace-owned canonical records. Domain
 * modules still rebuild their semantic value, so framing never becomes a second
 * authority for ReceiveIntent or another domain contract.
 */
export class CanonicalRecordReader {
  readonly #bytes: Uint8Array
  #offset: number

  private constructor(bytes: Uint8Array, offset: number) {
    this.#bytes = bytes
    this.#offset = offset
  }

  static open(bytesInput: Uint8Array, domain: string, version = 1): CanonicalRecordReader {
    const bytes = snapshotCanonicalBytes(bytesInput)
    assertCanonicalRecordDomain(bytes, domain, version)
    const prefixLength = TEXT_ENCODER.encode(`${domain}\0`).byteLength + 1
    return new CanonicalRecordReader(bytes, prefixLength)
  }

  static value(bytesInput: Uint8Array): CanonicalRecordReader {
    return new CanonicalRecordReader(snapshotCanonicalBytes(bytesInput), 0)
  }

  frame(label = 'canonical frame'): CanonicalBytes {
    const length = this.#rawU64(`${label} length`)
    if (length > BigInt(Number.MAX_SAFE_INTEGER)) {
      throw new TypeError(`${label} exceeds the runtime bound`)
    }
    return this.#take(Number(length), label)
  }

  byte(label: string): number {
    return this.#take(1, label)[0]!
  }

  framedU64(label: string): bigint {
    const value = this.frame(label)
    if (value.byteLength !== U64_BYTES) throw new TypeError(`${label} width is invalid`)
    return new DataView(value.buffer, value.byteOffset, U64_BYTES).getBigUint64(0, false)
  }

  u64(label: string): bigint {
    return this.#rawU64(label)
  }

  framedIdentity(width: number, label: string): string {
    const value = this.frame(label)
    if (value.byteLength !== width) throw new TypeError(`${label} width is invalid`)
    return snapshotIdentity(encodeBase64Url(value), width, label)
  }

  identity(width: number, label: string): string {
    const value = this.#take(width, label)
    return snapshotIdentity(encodeBase64Url(value), width, label)
  }

  finish(label = 'canonical record'): void {
    if (this.#offset !== this.#bytes.byteLength) {
      throw new TypeError(`${label} has trailing canonical bytes`)
    }
  }

  #rawU64(label: string): bigint {
    const value = this.#take(U64_BYTES, label)
    return new DataView(value.buffer, value.byteOffset, U64_BYTES).getBigUint64(0, false)
  }

  #take(length: number, label: string): CanonicalBytes {
    if (!Number.isSafeInteger(length) || length < 0 ||
        this.#offset > this.#bytes.byteLength - length) {
      throw new TypeError(`${label} is truncated`)
    }
    const value = this.#bytes.subarray(this.#offset, this.#offset + length)
    this.#offset += length
    // Persisted canonical inputs must never retain a mutable view into an IDB row.
    return Uint8Array.from(value) as CanonicalBytes
  }
}
