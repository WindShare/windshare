import {
  V2_RELAY_MAX_OPAQUE_CIPHERTEXT_BYTES, V2_RELAY_OPAQUE_ROUTE_HEADER_BYTES,
} from '../v2-protocol'
import { ReceiveLedgerError } from './types'
import { type ReceiveCounts } from '../../../contracts/relay-delivery-observation'

export const MAX_UINT64 = 0xffff_ffff_ffff_ffffn
export const ZERO_COUNTS: ReceiveCounts = Object.freeze({ frames: 0n, encodedBytes: 0n })
const MIN_ENVELOPE_BYTES = V2_RELAY_OPAQUE_ROUTE_HEADER_BYTES + 1
const MAX_ENVELOPE_BYTES = V2_RELAY_OPAQUE_ROUTE_HEADER_BYTES + V2_RELAY_MAX_OPAQUE_CIPHERTEXT_BYTES

export function positiveUint64(value: bigint): boolean {
  return typeof value === 'bigint' && value > 0n && value <= MAX_UINT64
}

export function addCounts(left: ReceiveCounts, right: ReceiveCounts): ReceiveCounts {
  return Object.freeze({ frames: left.frames + right.frames, encodedBytes: left.encodedBytes + right.encodedBytes })
}

export function subtractCounts(left: ReceiveCounts, right: ReceiveCounts): ReceiveCounts {
  return Object.freeze({ frames: left.frames - right.frames, encodedBytes: left.encodedBytes - right.encodedBytes })
}

export function sameCounts(left: ReceiveCounts, right: ReceiveCounts): boolean {
  return left.frames === right.frames && left.encodedBytes === right.encodedBytes
}

/** Byte lengths are bounded numbers; cumulative arithmetic never passes through Number. */
export function appendEnvelope(prefix: ReceiveCounts, encodedBytes: number): ReceiveCounts {
  if (!Number.isSafeInteger(encodedBytes) || encodedBytes < MIN_ENVELOPE_BYTES || encodedBytes > MAX_ENVELOPE_BYTES) {
    throw new ReceiveLedgerError('frame-size')
  }
  const after = addCounts(prefix, { frames: 1n, encodedBytes: BigInt(encodedBytes) })
  if (after.frames > MAX_UINT64 || after.encodedBytes > MAX_UINT64) {
    throw new ReceiveLedgerError('counter-exhausted')
  }
  return after
}
