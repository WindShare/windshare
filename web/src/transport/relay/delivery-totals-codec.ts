import {
  RelayProtocolError, RELAY_MAX_OPAQUE_CIPHERTEXT_BYTES,
  RELAY_OPAQUE_ROUTE_HEADER_BYTES, RELAY_SESSION_ID_BYTES, RELAY_WIRE_VERSION,
} from './protocol'

const HEADER_BYTES = 8
const FRAMES_OFFSET = HEADER_BYTES + RELAY_SESSION_ID_BYTES
const BYTES_OFFSET = FRAMES_OFFSET + 8
const FRAME_BYTES = BYTES_OFFSET + 8
const MAX_UINT64 = 0xffff_ffff_ffff_ffffn
const MIN_OPAQUE_BYTES = BigInt(RELAY_OPAQUE_ROUTE_HEADER_BYTES + 1)
const MAX_OPAQUE_BYTES = BigInt(RELAY_OPAQUE_ROUTE_HEADER_BYTES + RELAY_MAX_OPAQUE_CIPHERTEXT_BYTES)

/** Layout shared by distinct wire controls; counts never represent a credit delta. */
export interface RelayDeliveryTotals {
  readonly relaySessionId: Uint8Array
  readonly totalFrames: bigint
  readonly totalEncodedBytes: bigint
}

function validate(totals: RelayDeliveryTotals): void {
  if (totals.relaySessionId.byteLength !== RELAY_SESSION_ID_BYTES || !totals.relaySessionId.some(byte => byte !== 0)) {
    throw new RelayProtocolError('identity', 'Delivery totals require a relay session ID')
  }
  const { totalFrames, totalEncodedBytes } = totals
  // BigInt is required throughout: long-lived flows can cross Number's exact range.
  if (typeof totalFrames !== 'bigint' || totalFrames < 0n || totalFrames > MAX_UINT64 ||
      typeof totalEncodedBytes !== 'bigint' || totalEncodedBytes < 0n || totalEncodedBytes > MAX_UINT64) {
    throw new RelayProtocolError('malformed', 'Delivery totals are outside the unsigned 64-bit domain')
  }
  if (totalEncodedBytes < totalFrames * MIN_OPAQUE_BYTES || totalEncodedBytes > totalFrames * MAX_OPAQUE_BYTES) {
    throw new RelayProtocolError('malformed', 'Delivery totals must describe complete WS2O envelopes')
  }
}

function prefix(magic: string): Uint8Array {
  return Uint8Array.of(
    magic.charCodeAt(0), magic.charCodeAt(1), magic.charCodeAt(2), magic.charCodeAt(3),
    RELAY_WIRE_VERSION, 0, 0, 0,
  )
}

export function encodeDeliveryTotals(totals: RelayDeliveryTotals, magic: string): Uint8Array<ArrayBuffer> {
  validate(totals)
  const encoded = new Uint8Array(FRAME_BYTES)
  encoded.set(prefix(magic))
  encoded.set(totals.relaySessionId, HEADER_BYTES)
  const view = new DataView(encoded.buffer)
  view.setBigUint64(FRAMES_OFFSET, totals.totalFrames, false)
  view.setBigUint64(BYTES_OFFSET, totals.totalEncodedBytes, false)
  return encoded
}

export function decodeDeliveryTotals(encoded: Uint8Array, magic: string): RelayDeliveryTotals {
  if (encoded.byteLength !== FRAME_BYTES || !prefix(magic).every((byte, index) => encoded[index] === byte)) {
    throw new RelayProtocolError('malformed', 'Delivery control has an invalid header or length')
  }
  const view = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength)
  const totals = {
    relaySessionId: encoded.slice(HEADER_BYTES, FRAMES_OFFSET),
    totalFrames: view.getBigUint64(FRAMES_OFFSET, false),
    totalEncodedBytes: view.getBigUint64(BYTES_OFFSET, false),
  }
  validate(totals)
  return Object.freeze(totals)
}
