import { concatBytes, encodeUint32 } from '../../crypto/bytes'
import { RelayProtocolError, RELAY_SESSION_ID_BYTES, RELAY_WIRE_VERSION } from './protocol'

export const RELAY_RECEIVE_WINDOW_FRAMES = 256
export const RELAY_RECEIVE_WINDOW_BYTES = 16 << 20
export const RELAY_RECEIVE_CREDIT_BATCH_FRAMES = RELAY_RECEIVE_WINDOW_FRAMES / 4
export { RELAY_OPAQUE_ROUTE_HEADER_BYTES } from './protocol'
const RECEIVE_CREDIT_BYTES = 24
const PREFIX = Uint8Array.of(0x57, 0x53, 0x32, 0x42, RELAY_WIRE_VERSION, 0, 0, 0)

export interface RelayReceiveCredit {
  readonly relaySessionId: Uint8Array
  readonly frames: number
  readonly bytes: number
}

export function encodeReceiveCredit(credit: RelayReceiveCredit): Uint8Array<ArrayBuffer> {
  if (credit.relaySessionId.byteLength !== RELAY_SESSION_ID_BYTES || !credit.relaySessionId.some(byte => byte !== 0)) {
    throw new RelayProtocolError('identity', 'Receive credit requires a relay session ID')
  }
  if (!Number.isInteger(credit.frames) || credit.frames < 0 || credit.frames > RELAY_RECEIVE_WINDOW_FRAMES ||
      !Number.isInteger(credit.bytes) || credit.bytes < 0 || credit.bytes > RELAY_RECEIVE_WINDOW_BYTES ||
      (credit.frames === 0 && credit.bytes === 0)) {
    throw new RelayProtocolError('malformed', 'Receive credit exceeds the ingress window')
  }
  return concatBytes([PREFIX, credit.relaySessionId, encodeUint32(credit.frames), encodeUint32(credit.bytes)])
}

export function decodeReceiveCredit(encoded: Uint8Array): RelayReceiveCredit {
  if (encoded.byteLength !== RECEIVE_CREDIT_BYTES || !PREFIX.every((byte, index) => encoded[index] === byte)) {
    throw new RelayProtocolError('malformed', 'WS2B has an invalid header or length')
  }
  const view = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength)
  const credit = { relaySessionId: encoded.slice(8, 16), frames: view.getUint32(16), bytes: view.getUint32(20) }
  encodeReceiveCredit(credit)
  return Object.freeze(credit)
}
