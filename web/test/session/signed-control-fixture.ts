import { createHash } from 'node:crypto'

import { concatBytes } from '../../src/crypto/bytes'
import { decodeCanonicalCbor, encodeCanonicalCbor } from '../../src/protocol/cbor'
import {
  encodeV2Message,
  type V2ControlBinding,
  type V2MessageKind,
  type V2SessionMessage,
  V2_SENDER_CONTROL_SCHEMA_VERSION,
} from '../../src/session/v2-message'

const CONTROL_SCHEMA_KEY = 0
const CONTROL_BODY_KEY = 1
const CONTROL_SIGNATURE_KEY = 255
const CONTROL_OPERATION_DOMAIN = new TextEncoder().encode('windshare/v2 control/operation\0')

export interface SenderControlKeyPair {
  readonly privateKey: CryptoKey
  readonly publicKey: Uint8Array<ArrayBuffer>
}

export interface SignedSenderControl {
  readonly message: V2SessionMessage
  readonly unsignedWrapper: Uint8Array<ArrayBuffer>
  readonly preimage: Uint8Array<ArrayBuffer>
  readonly signature: Uint8Array<ArrayBuffer>
}

export async function senderControlKeyPair(): Promise<SenderControlKeyPair> {
  const pair = await globalThis.crypto.subtle.generateKey('Ed25519', true, ['sign', 'verify']) as CryptoKeyPair
  return Object.freeze({
    privateKey: pair.privateKey,
    publicKey: new Uint8Array(await globalThis.crypto.subtle.exportKey('raw', pair.publicKey)),
  })
}

export async function signSenderOperationControl(options: {
  readonly kind: V2MessageKind
  readonly operationId: Uint8Array
  readonly semanticBody: Uint8Array
  readonly binding: V2ControlBinding
  readonly privateKey: CryptoKey
}): Promise<SignedSenderControl> {
  const semanticValue = decodeCanonicalCbor(options.semanticBody, 65_536, 'test semantic body')
  const unsignedWrapper = encodeCanonicalCbor(new Map<number, unknown>([
    [CONTROL_SCHEMA_KEY, V2_SENDER_CONTROL_SCHEMA_VERSION],
    [CONTROL_BODY_KEY, semanticValue],
  ]))
  const preimage = senderOperationControlPreimage(
    options.kind,
    options.operationId,
    options.binding,
    unsignedWrapper,
  )
  const signature = new Uint8Array(await globalThis.crypto.subtle.sign(
    'Ed25519',
    options.privateKey,
    preimage,
  ))
  const signedBody = encodeCanonicalCbor(new Map<number, unknown>([
    [CONTROL_SCHEMA_KEY, V2_SENDER_CONTROL_SCHEMA_VERSION],
    [CONTROL_BODY_KEY, semanticValue],
    [CONTROL_SIGNATURE_KEY, signature],
  ]))
  return Object.freeze({
    message: encodeV2Message(options.kind, options.operationId, signedBody),
    unsignedWrapper,
    preimage,
    signature,
  })
}

function senderOperationControlPreimage(
  kind: V2MessageKind,
  operationId: Uint8Array,
  binding: V2ControlBinding,
  unsignedWrapper: Uint8Array,
): Uint8Array<ArrayBuffer> {
  const lane = new Uint8Array(8)
  const laneView = new DataView(lane.buffer)
  laneView.setUint32(0, binding.laneId, false)
  laneView.setUint32(4, binding.laneEpoch, false)
  const sequence = new Uint8Array(8)
  new DataView(sequence.buffer).setBigUint64(0, binding.sequence, false)
  const bodyDigest = createHash('sha256').update(unsignedWrapper).digest()
  return concatBytes([
    CONTROL_OPERATION_DOMAIN,
    binding.shareInstance,
    binding.protocolSessionId,
    lane,
    Uint8Array.of(binding.direction),
    sequence,
    Uint8Array.of(kind),
    operationId,
    bodyDigest,
  ])
}
