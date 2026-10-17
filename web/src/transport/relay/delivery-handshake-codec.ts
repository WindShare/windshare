import { concatBytes } from '../../crypto/bytes'
import type { CryptoRuntime } from '../../crypto/webcrypto'
import {
  decodeV2Join, decodeV2RegisterInit, encodeV2Join, encodeV2RegisterInit,
  v2RegistrationProofPreimage, V2RelayProtocolError,
  type V2Challenge, type V2RegisterInit,
} from './v2-protocol'

const CAPACITY_BYTES = 16
const REGISTER_IDENTITY_BYTES = 116
const JOIN_IDENTITY_BYTES = 20
const MAX_UINT64 = 0xffff_ffff_ffff_ffffn

/** Independent connection limits; these are neither grants nor consumed totals. */
export interface RelayReceiveCapacity {
  readonly frames: bigint
  readonly encodedBytes: bigint
}

/** Staged contract: activate with cumulative consumption and final settlement. */
export interface RelayDeliveryRegisterInit {
  readonly registration: V2RegisterInit
  readonly receiveCapacity: RelayReceiveCapacity
}

export interface RelayDeliveryJoin {
  readonly shareId: Uint8Array
  readonly receiveCapacity: RelayReceiveCapacity
}

function encodeCapacity(capacity: RelayReceiveCapacity): Uint8Array<ArrayBuffer> {
  for (const value of [capacity.frames, capacity.encodedBytes]) {
    if (typeof value !== 'bigint' || value <= 0n || value > MAX_UINT64) {
      throw new V2RelayProtocolError('malformed', 'Receive capacity must use positive unsigned 64-bit limits')
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
    throw new V2RelayProtocolError('malformed', 'Delivery handshake requires an exact capacity declaration')
  }
  const view = new DataView(encoded.buffer, encoded.byteOffset + identityBytes, CAPACITY_BYTES)
  const capacity = Object.freeze({ frames: view.getBigUint64(0, false), encodedBytes: view.getBigUint64(8, false) })
  encodeCapacity(capacity)
  return capacity
}

export async function encodeDeliveryRegisterInit(frame: RelayDeliveryRegisterInit, runtime?: CryptoRuntime): Promise<Uint8Array<ArrayBuffer>> {
  const capacity = encodeCapacity(frame.receiveCapacity)
  return concatBytes([await encodeV2RegisterInit(frame.registration, runtime), capacity])
}

export async function decodeDeliveryRegisterInit(encoded: Uint8Array, runtime?: CryptoRuntime): Promise<RelayDeliveryRegisterInit> {
  const receiveCapacity = decodeCapacity(encoded, REGISTER_IDENTITY_BYTES)
  const registration = await decodeV2RegisterInit(encoded.subarray(0, REGISTER_IDENTITY_BYTES), runtime)
  return Object.freeze({ registration, receiveCapacity })
}

export async function deliveryRegistrationProofPreimage(
  init: RelayDeliveryRegisterInit, challenge: V2Challenge, relayIdentity: Uint8Array, runtime?: CryptoRuntime,
): Promise<Uint8Array<ArrayBuffer>> {
  const capacity = encodeCapacity(init.receiveCapacity)
  return concatBytes([await v2RegistrationProofPreimage(init.registration, challenge, relayIdentity, runtime), capacity])
}

export function encodeDeliveryJoin(frame: RelayDeliveryJoin): Uint8Array<ArrayBuffer> {
  return concatBytes([encodeV2Join(frame.shareId), encodeCapacity(frame.receiveCapacity)])
}

export function decodeDeliveryJoin(encoded: Uint8Array): RelayDeliveryJoin {
  const receiveCapacity = decodeCapacity(encoded, JOIN_IDENTITY_BYTES)
  return Object.freeze({ shareId: decodeV2Join(encoded.subarray(0, JOIN_IDENTITY_BYTES)), receiveCapacity })
}
