import { RelayProtocolError } from './protocol'

export const CAPACITY_WAIT_MAGIC = 'WS2Z'
export const CAPACITY_WAIT_BYTES = 20
export const MAX_CAPACITY_WAIT_MILLISECONDS = 60_000

export function decodeCapacityWait(encoded: Uint8Array): {
  relaySessionId: Uint8Array<ArrayBuffer>; direction: 0 | 1; milliseconds: number
} {
  if (encoded.byteLength !== CAPACITY_WAIT_BYTES ||
      String.fromCharCode(...encoded.subarray(0, 4)) !== CAPACITY_WAIT_MAGIC ||
      encoded[4] !== 2 || (encoded[5] !== 0 && encoded[5] !== 1) ||
      encoded[6] !== 0 || encoded[7] !== 0 || !encoded.subarray(8, 16).some(byte => byte !== 0)) {
    throw new RelayProtocolError('malformed', 'Invalid relay capacity wait')
  }
  const milliseconds = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength).getUint32(16, false)
  if (milliseconds > MAX_CAPACITY_WAIT_MILLISECONDS) {
    throw new RelayProtocolError('malformed', 'Unbounded relay capacity wait')
  }
  return { relaySessionId: encoded.slice(8, 16), direction: encoded[5], milliseconds }
}
