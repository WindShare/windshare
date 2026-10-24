import { concatBytes } from '../../crypto/bytes'
import type { CryptoRuntime } from '../../crypto/webcrypto'
import {
  decodeJoin, decodeRegisterInit, encodeJoin, encodeRegisterInit,
  registrationProofPreimage, RelayProtocolError,
  type Challenge, type RegisterInit,
} from './protocol'

const CAPACITY_BYTES = 16
const REGISTER_IDENTITY_BYTES = 116
const JOIN_IDENTITY_BYTES = 20
const MAX_UINT64 = 0xffff_ffff_ffff_ffffn

/** Independent connection limits; these are neither grants nor consumed totals. */
export interface RelayReceiveCapacity {
  readonly frames: bigint
  readonly encodedBytes: bigint
}

/** Admission binds immutable receive capacity to the authenticated registration. */
export interface RelayDeliveryRegisterInit {
  readonly registration: RegisterInit
  readonly receiveCapacity: RelayReceiveCapacity
}

export interface RelayDeliveryJoin {
  readonly shareId: Uint8Array
  readonly receiveCapacity: RelayReceiveCapacity
}

function encodeCapacity(capacity: RelayReceiveCapacity): Uint8Array<ArrayBuffer> {
  for (const value of [capacity.frames, capacity.encodedBytes]) {
    if (typeof value !== 'bigint' || value <= 0n || value > MAX_UINT64) {
      throw new RelayProtocolError('malformed', 'Receive capacity must use positive unsigned 64-bit limits')
    }
  }
  const encoded = new Uint8Array(CAPACITY_BYTES)
  const view = new DataView(encoded.buffer)
  view.setBigUint64(0, capacity.frames, false)
  view.setBigUint64(8, capacity.encodedBytes, false)
  return encoded
}

function decodeCapacity(encoded: Uint8Array, identityBytes: number): RelayReceiveCapacity {
  if (encoded.byteLength !== identityBytes + CAPACITY_BYTES) {
    throw new RelayProtocolError('malformed', 'Delivery handshake requires an exact capacity declaration')
  }
  const view = new DataView(encoded.buffer, encoded.byteOffset + identityBytes, CAPACITY_BYTES)
  const capacity = Object.freeze({ frames: view.getBigUint64(0, false), encodedBytes: view.getBigUint64(8, false) })
  encodeCapacity(capacity)
  return capacity
}

export async function encodeDeliveryRegisterInit(frame: RelayDeliveryRegisterInit, runtime?: CryptoRuntime): Promise<Uint8Array<ArrayBuffer>> {
  const capacity = encodeCapacity(frame.receiveCapacity)
  return concatBytes([await encodeRegisterInit(frame.registration, runtime), capacity])
}

export async function decodeDeliveryRegisterInit(encoded: Uint8Array, runtime?: CryptoRuntime): Promise<RelayDeliveryRegisterInit> {
  const receiveCapacity = decodeCapacity(encoded, REGISTER_IDENTITY_BYTES)
  const registration = await decodeRegisterInit(encoded.subarray(0, REGISTER_IDENTITY_BYTES), runtime)
  return Object.freeze({ registration, receiveCapacity })
}

export async function deliveryRegistrationProofPreimage(
  init: RelayDeliveryRegisterInit, challenge: Challenge, relayIdentity: Uint8Array, runtime?: CryptoRuntime,
): Promise<Uint8Array<ArrayBuffer>> {
  const capacity = encodeCapacity(init.receiveCapacity)
  return concatBytes([await registrationProofPreimage(init.registration, challenge, relayIdentity, runtime), capacity])
}

export function encodeDeliveryJoin(frame: RelayDeliveryJoin): Uint8Array<ArrayBuffer> {
  return concatBytes([encodeJoin(frame.shareId), encodeCapacity(frame.receiveCapacity)])
}

export function decodeDeliveryJoin(encoded: Uint8Array): RelayDeliveryJoin {
  const receiveCapacity = decodeCapacity(encoded, JOIN_IDENTITY_BYTES)
  return Object.freeze({ shareId: decodeJoin(encoded.subarray(0, JOIN_IDENTITY_BYTES)), receiveCapacity })
}
