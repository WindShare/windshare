import { encodeBase64Url } from '../../../crypto/bytes'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalRecord,
  canonicalText,
  canonicalU8,
  canonicalUnixMilliseconds,
  equalCanonicalBytes,
  snapshotCanonicalBytes,
  snapshotIdentity,
  type CanonicalBytes,
} from '../canonical'
import {
  RECEIPT_SCHEMA_VERSION,
  type OwnedWorkspaceObjectReceipt,
  type PreparationAdmissionReceiptV1,
  type ReceiveReceiptBase,
} from './model'

const U64_MAXIMUM = 0xffff_ffff_ffff_ffffn

export const RECEIVE_RECEIPT_PREFIX = canonicalRecord('windshare/receive-receipt/v1', 1, [])

export function receiptIdentity(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
}): { readonly operationId: string; readonly receiveIntentDigest: string } {
  return Object.freeze({
    operationId: snapshotIdentity(input.operationId, 16, 'operation ID'),
    receiveIntentDigest: digest(input.receiveIntentDigest, 'receive intent digest'),
  })
}

export async function completeReceipt(
  identity: { readonly operationId: string; readonly receiveIntentDigest: string },
  discriminant: number,
  variantFields: readonly CanonicalBytes[],
): Promise<ReceiveReceiptBase> {
  const canonicalBytes = canonicalRecord('windshare/receive-receipt/v1', 1, [
    canonicalU8(discriminant),
    canonicalFrame(canonicalIdentity(identity.operationId, 16, 'operation ID')),
    canonicalFrame(canonicalIdentity(identity.receiveIntentDigest, 32, 'receive intent digest')),
    ...variantFields,
  ])
  return Object.freeze({
    schemaVersion: RECEIPT_SCHEMA_VERSION,
    ...identity,
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

export class ReceiptReader {
  readonly #bytes: Uint8Array
  #offset = 0

  constructor(bytes: Uint8Array) {
    this.#bytes = snapshotCanonicalBytes(bytes)
  }

  prefix(expected: Uint8Array): void {
    const actual = this.#take(expected.byteLength)
    if (!equalCanonicalBytes(actual, expected)) {
      throw new TypeError('receive receipt domain or version is invalid')
    }
  }

  byte(): number {
    return this.#take(1)[0]!
  }

  frame(): CanonicalBytes {
    const size = this.#u64Raw('canonical frame length')
    if (size > BigInt(Number.MAX_SAFE_INTEGER)) {
      throw new TypeError('canonical receipt frame exceeds the runtime bound')
    }
    return snapshotCanonicalBytes(this.#take(Number(size)))
  }

  identity(width: number, label: string): string {
    const bytes = this.frame()
    if (bytes.byteLength !== width) throw new TypeError(`${label} width is invalid`)
    return snapshotIdentity(encodeBase64Url(bytes), width, label)
  }

  u64(label: string): bigint {
    const bytes = this.frame()
    if (bytes.byteLength !== 8) throw new TypeError(`${label} width is invalid`)
    return new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getBigUint64(0, false)
  }

  u8(label: string): number {
    const bytes = this.frame()
    if (bytes.byteLength !== 1) throw new TypeError(`${label} width is invalid`)
    return bytes[0]!
  }

  text(label: string): string {
    const canonical = this.frame()
    const value = new TextDecoder(undefined, { fatal: true }).decode(canonical)
    if (!equalCanonicalBytes(canonicalText(value), canonical)) {
      throw new TypeError(`${label} is not canonical text`)
    }
    return value
  }

  end(): void {
    if (this.#offset !== this.#bytes.byteLength) {
      throw new TypeError('receive receipt has trailing canonical bytes')
    }
  }

  #u64Raw(label: string): bigint {
    const bytes = this.#take(8)
    if (bytes.byteLength !== 8) throw new TypeError(`${label} is truncated`)
    return new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getBigUint64(0, false)
  }

  #take(length: number): Uint8Array {
    if (!Number.isSafeInteger(length) || length < 0 ||
        this.#offset > this.#bytes.byteLength - length) {
      throw new TypeError('receive receipt canonical bytes are truncated')
    }
    const result = this.#bytes.subarray(this.#offset, this.#offset + length)
    this.#offset += length
    return result
  }
}

export function snapshotOwnedObjects(
  input: readonly OwnedWorkspaceObjectReceipt[],
): readonly OwnedWorkspaceObjectReceipt[] {
  const values = input.map((object) => Object.freeze({
    ownedObjectId: digest(object.ownedObjectId, 'owned object ID'),
    exactBytes: checkedU64(object.exactBytes, 'owned object bytes'),
  })).sort((left, right) => compareCanonicalIdentity(left.ownedObjectId, right.ownedObjectId))
  if (values.length === 0 || values.some((value, index) =>
    index > 0 && value.ownedObjectId === values[index - 1]?.ownedObjectId)) {
    throw new TypeError('workspace seal object inventory is empty or duplicated')
  }
  return Object.freeze(values)
}

export function snapshotSortedDigests(input: readonly string[], label: string): readonly string[] {
  const values = input.map((value) => digest(value, label)).sort()
  if (values.some((value, index) => index > 0 && value === values[index - 1])) {
    throw new TypeError(`${label} inventory contains duplicates`)
  }
  return Object.freeze(values)
}

export function snapshotAdmissionLimits(input: {
  readonly estimatedQuotaBytes: bigint | null | undefined
  readonly currentUsageBytes: bigint
  readonly minimumReserveBytes: bigint
  readonly incrementalPhysicalPeakBytes: bigint
}): Pick<
  PreparationAdmissionReceiptV1,
  | 'estimatedQuotaBytes'
  | 'currentUsageBytes'
  | 'minimumReserveBytes'
  | 'incrementalPhysicalPeakBytes'
> {
  return Object.freeze({
    estimatedQuotaBytes: input.estimatedQuotaBytes == null ? null : checkedU64(input.estimatedQuotaBytes, 'estimated quota'),
    currentUsageBytes: checkedU64(input.currentUsageBytes, 'current quota usage'),
    minimumReserveBytes: checkedU64(input.minimumReserveBytes, 'quota reserve'),
    incrementalPhysicalPeakBytes: checkedU64(
      input.incrementalPhysicalPeakBytes,
      'incremental physical peak',
    ),
  })
}

export function canonicalTextValue(value: string, label: string): string {
  if (typeof value !== 'string' || value.length === 0) throw new TypeError(`${label} is empty`)
  const canonical = canonicalText(value)
  const decoded = new TextDecoder(undefined, { fatal: true }).decode(canonical)
  if (decoded !== value) throw new TypeError(`${label} is not canonical text`)
  return decoded
}

export function digest(value: string, label: string): string {
  return snapshotIdentity(value, 32, label)
}

export function unixMilliseconds(value: number, label: string): number {
  try {
    canonicalUnixMilliseconds(value)
  } catch (error) {
    throw new TypeError(`${label} is invalid`, { cause: error })
  }
  return value
}

export function checkedAdd(left: bigint, right: bigint): bigint {
  const value = left + right
  if (value > U64_MAXIMUM) throw new RangeError('receipt byte arithmetic overflow')
  return value
}

export function checkedU64(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n || value > U64_MAXIMUM) {
    throw new TypeError(`${label} is not a u64`)
  }
  return value
}

function compareCanonicalIdentity(left: string, right: string): number {
  const leftBytes = canonicalIdentity(left, 32, 'sortable identity')
  const rightBytes = canonicalIdentity(right, 32, 'sortable identity')
  for (let index = 0; index < leftBytes.length; index += 1) {
    const difference = (leftBytes[index] ?? 0) - (rightBytes[index] ?? 0)
    if (difference !== 0) return difference
  }
  return 0
}
