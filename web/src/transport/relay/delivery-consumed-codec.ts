import {
  V2RelayProtocolError, V2_RELAY_MAX_OPAQUE_CIPHERTEXT_BYTES,
  V2_RELAY_OPAQUE_ROUTE_HEADER_BYTES, V2_RELAY_SESSION_ID_BYTES, V2_RELAY_WIRE_VERSION,
} from './v2-protocol'

const PREFIX = Uint8Array.of(0x57, 0x53, 0x32, 0x42, V2_RELAY_WIRE_VERSION, 0, 0, 0)
const FRAMES_OFFSET = PREFIX.byteLength + V2_RELAY_SESSION_ID_BYTES
const BYTES_OFFSET = FRAMES_OFFSET + 8
const FRAME_BYTES = BYTES_OFFSET + 8
const MAX_UINT64 = 0xffff_ffff_ffff_ffffn
const MIN_OPAQUE_BYTES = BigInt(V2_RELAY_OPAQUE_ROUTE_HEADER_BYTES + 1)
const MAX_OPAQUE_BYTES = BigInt(V2_RELAY_OPAQUE_ROUTE_HEADER_BYTES + V2_RELAY_MAX_OPAQUE_CIPHERTEXT_BYTES)

/** Cumulative feedback for the coordinated cutover; not the live receive-credit codec. */
export interface RelayDeliveryConsumed {
  readonly relaySessionId: Uint8Array
  readonly totalFrames: bigint
  readonly totalEncodedBytes: bigint
}

function validate(feedback: RelayDeliveryConsumed): void {
  if (feedback.relaySessionId.byteLength !== V2_RELAY_SESSION_ID_BYTES || !feedback.relaySessionId.some(byte => byte !== 0)) {
    throw new V2RelayProtocolError('identity', 'Delivery consumption requires a relay session ID')
  }
  const { totalFrames, totalEncodedBytes } = feedback
  // BigInt is required throughout: long-lived flows can cross Number's exact range.
  if (typeof totalFrames !== 'bigint' || totalFrames < 0n || totalFrames > MAX_UINT64 ||
      typeof totalEncodedBytes !== 'bigint' || totalEncodedBytes < 0n || totalEncodedBytes > MAX_UINT64) {
    throw new V2RelayProtocolError('malformed', 'Delivery totals are outside the unsigned 64-bit domain')
  }
  if (totalEncodedBytes < totalFrames * MIN_OPAQUE_BYTES || totalEncodedBytes > totalFrames * MAX_OPAQUE_BYTES) {
    throw new V2RelayProtocolError('malformed', 'Delivery totals must describe complete WS2O envelopes')
  }
}

export function encodeDeliveryConsumed(feedback: RelayDeliveryConsumed): Uint8Array<ArrayBuffer> {
  validate(feedback)
  const encoded = new Uint8Array(FRAME_BYTES)
  encoded.set(PREFIX)
  encoded.set(feedback.relaySessionId, PREFIX.byteLength)
  const view = new DataView(encoded.buffer)
  view.setBigUint64(FRAMES_OFFSET, feedback.totalFrames, false)
  view.setBigUint64(BYTES_OFFSET, feedback.totalEncodedBytes, false)
  return encoded
}

export function decodeDeliveryConsumed(encoded: Uint8Array): RelayDeliveryConsumed {
  if (encoded.byteLength !== FRAME_BYTES || !PREFIX.every((byte, index) => encoded[index] === byte)) {
    throw new V2RelayProtocolError('malformed', 'Cumulative WS2B has an invalid header or length')
  }
  const view = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength)
  const feedback = {
    relaySessionId: encoded.slice(PREFIX.byteLength, FRAMES_OFFSET),
    totalFrames: view.getBigUint64(FRAMES_OFFSET, false),
    totalEncodedBytes: view.getBigUint64(BYTES_OFFSET, false),
  }
  validate(feedback)
  return Object.freeze(feedback)
}
