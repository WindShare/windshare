import { concatBytes, copyBytes, encodeUint32, equalBytes } from '../crypto/bytes'
import { verifyEd25519Signature } from '../crypto/curve25519'
import { sha256 } from '../crypto/digest'
import { type CryptoRuntime, defaultCryptoRuntime } from '../crypto/webcrypto'
import {
  decodeCanonicalCbor,
  encodeCanonicalCbor,
  requireBytes,
  requireNumericMap,
  requireUnsigned,
} from '../protocol/cbor'

export const V2_LANE_WIRE_VERSION = 2
export const V2_LANE_HELLO_BODY_BYTES = 77
export const V2_LANE_HELLO_BYTES = 109
export const V2_LANE_ACCEPT_BODY_BYTES = 53
export const V2_LANE_ACCEPT_BYTES = 117
export const V2_LANE_REJECT_BODY_BYTES = 44
export const V2_LANE_REJECT_BYTES = 108
export const V2_LANE_MAX_RETRY_AFTER_MILLISECONDS = 30_000

export const V2_LANE_REJECT = Object.freeze({
  unknownSession: 1,
  staleEpoch: 2,
  grantExpired: 3,
  grantConsumed: 4,
  admissionLimited: 5,
  stopping: 6,
  grantMismatch: 7,
} as const)

export type V2LaneRejectCode = (typeof V2_LANE_REJECT)[keyof typeof V2_LANE_REJECT]

const TEXT_ENCODER = new TextEncoder()
const LANE_HELLO_MAGIC = TEXT_ENCODER.encode('WS2A')
const LANE_ACCEPT_MAGIC = TEXT_ENCODER.encode('WS2B')
const LANE_REJECT_MAGIC = TEXT_ENCODER.encode('WS2N')
const LANE_HELLO_DOMAIN = TEXT_ENCODER.encode('windshare/v2 lane-hello\0')
const LANE_ACCEPT_DOMAIN = TEXT_ENCODER.encode('windshare/v2 lane-accept\0')
const LANE_REJECT_DOMAIN = TEXT_ENCODER.encode('windshare/v2 lane-reject\0')
const MAX_UINT32 = 0xffff_ffff

export type V2LaneCodecErrorKind = 'input' | 'malformed' | 'proof' | 'signature'

export class V2LaneCodecError extends Error {
  readonly kind: V2LaneCodecErrorKind

  constructor(kind: V2LaneCodecErrorKind, message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2LaneCodecError'
    this.kind = kind
  }
}

export interface V2LaneHelloFields {
  readonly shareInstance: Uint8Array
  readonly protocolSessionId: Uint8Array
  readonly laneId: number
  readonly laneEpoch: number
  readonly grantOperationId: Uint8Array
  readonly attachNonce: Uint8Array
}

export interface V2LaneRejection {
  readonly code: V2LaneRejectCode
  readonly retryAfterMilliseconds: number
}

export interface V2LaneGrant {
  readonly laneId: number
  readonly laneEpoch: number
  readonly grantOperationId: Uint8Array<ArrayBuffer>
  readonly attachNonce: Uint8Array<ArrayBuffer>
}

const MAXIMUM_LANE_CONTROL_BODY_BYTES = 1_024

export function encodeV2LaneAttachRequest(requestedLaneId = 0): Uint8Array<ArrayBuffer> {
  return encodeCanonicalCbor(new Map<number, unknown>([
    [0, 1],
    [1, 0],
    [2, requireUint32(requestedLaneId, 'requested lane ID')],
  ]))
}

export function decodeV2LaneGrant(
  encoded: Uint8Array,
  grantOperationId: Uint8Array,
): V2LaneGrant {
  const fields = requireNumericMap(
    decodeCanonicalCbor(encoded, MAXIMUM_LANE_CONTROL_BODY_BYTES, 'lane grant'),
    [0, 1, 2, 3, 4],
    'lane grant',
  )
  if (
    requireUnsigned(fields.get(0), 'lane grant schema version') !== 1n ||
    requireUnsigned(fields.get(1), 'lane grant disposition') !== 1n
  ) {
    throw new V2LaneCodecError('malformed', 'Lane grant has an unsupported schema or disposition')
  }
  const laneId = requireWireUint32(fields.get(2), 'lane grant ID', true)
  const laneEpoch = requireWireUint32(fields.get(3), 'lane grant epoch', true)
  return Object.freeze({
    laneId,
    laneEpoch,
    grantOperationId: requireIdentity(grantOperationId, 'grant operation ID'),
    attachNonce: requireBytes(fields.get(4), 16, 'lane grant attach nonce', true),
  })
}

function requireIdentity(value: Uint8Array, label: string): Uint8Array<ArrayBuffer> {
  if (value.byteLength !== 16 || !value.some((item) => item !== 0)) {
    throw new V2LaneCodecError('input', `${label} must be a nonzero 16-byte identity`)
  }
  return value.slice()
}

function requireUint32(value: number, label: string, nonzero = false): number {
  if (
    !Number.isInteger(value) ||
    value < (nonzero ? 1 : 0) ||
    value > MAX_UINT32
  ) {
    throw new V2LaneCodecError('input', `${label} is outside its unsigned 32-bit domain`)
  }
  return value
}

function requireWireUint32(value: unknown, label: string, nonzero = false): number {
  const decoded = requireUnsigned(value, label)
  if (decoded > BigInt(MAX_UINT32) || (nonzero && decoded === 0n)) {
    throw new V2LaneCodecError('malformed', `${label} is outside its unsigned 32-bit domain`)
  }
  return Number(decoded)
}

function hasMagic(encoded: Uint8Array, magic: Uint8Array): boolean {
  return equalBytes(encoded.subarray(0, magic.byteLength), magic)
}

async function hmacSha256(
  keyBytes: Uint8Array,
  input: Uint8Array,
  runtime: CryptoRuntime,
): Promise<Uint8Array<ArrayBuffer>> {
  if (keyBytes.byteLength !== 32) {
    throw new V2LaneCodecError('input', 'receiver-to-sender traffic key must be 32 bytes')
  }
  try {
    const key = await runtime.subtle.importKey(
      'raw',
      copyBytes(keyBytes),
      { name: 'HMAC', hash: 'SHA-256' },
      false,
      ['sign'],
    )
    return new Uint8Array(await runtime.subtle.sign('HMAC', key, copyBytes(input)))
  } catch (cause) {
    throw new V2LaneCodecError('proof', 'Unable to compute lane HMAC proof', { cause })
  }
}

async function verifyEd25519(
  senderPublicKey: Uint8Array,
  preimage: Uint8Array,
  signature: Uint8Array,
  runtime: CryptoRuntime,
): Promise<boolean> {
  if (senderPublicKey.byteLength !== 32 || signature.byteLength !== 64) {
    throw new V2LaneCodecError('input', 'Ed25519 verification material has an invalid width')
  }
  try {
    return await verifyEd25519Signature(senderPublicKey, preimage, signature, runtime)
  } catch (cause) {
    throw new V2LaneCodecError('signature', 'Unable to verify lane response signature', {
      cause,
    })
  }
}

function laneHelloBody(fields: V2LaneHelloFields): Uint8Array<ArrayBuffer> {
  return concatBytes([
    LANE_HELLO_MAGIC,
    Uint8Array.of(V2_LANE_WIRE_VERSION),
    requireIdentity(fields.shareInstance, 'share instance'),
    requireIdentity(fields.protocolSessionId, 'protocol session ID'),
    encodeUint32(requireUint32(fields.laneId, 'lane ID', true)),
    encodeUint32(requireUint32(fields.laneEpoch, 'lane epoch', true)),
    requireIdentity(fields.grantOperationId, 'grant operation ID'),
    requireIdentity(fields.attachNonce, 'attach nonce'),
  ])
}

export async function encodeV2LaneHello(
  fields: V2LaneHelloFields,
  receiverToSenderKey: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  const body = laneHelloBody(fields)
  const proof = await hmacSha256(
    receiverToSenderKey,
    concatBytes([LANE_HELLO_DOMAIN, body]),
    runtime,
  )
  return concatBytes([body, proof])
}

export async function decodeV2LaneHello(
  encoded: Uint8Array,
  receiverToSenderKey: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<V2LaneHelloFields> {
  if (
    encoded.byteLength !== V2_LANE_HELLO_BYTES ||
    !hasMagic(encoded, LANE_HELLO_MAGIC) ||
    encoded[4] !== V2_LANE_WIRE_VERSION
  ) {
    throw new V2LaneCodecError('malformed', 'LaneHello has an invalid header or length')
  }
  const body = encoded.subarray(0, V2_LANE_HELLO_BODY_BYTES)
  const expected = await hmacSha256(
    receiverToSenderKey,
    concatBytes([LANE_HELLO_DOMAIN, body]),
    runtime,
  )
  if (!equalBytes(expected, encoded.subarray(V2_LANE_HELLO_BODY_BYTES))) {
    throw new V2LaneCodecError('proof', 'LaneHello HMAC proof is invalid')
  }
  const view = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength)
  const fields: V2LaneHelloFields = Object.freeze({
    shareInstance: requireIdentity(encoded.subarray(5, 21), 'share instance'),
    protocolSessionId: requireIdentity(encoded.subarray(21, 37), 'protocol session ID'),
    laneId: requireUint32(view.getUint32(37, false), 'lane ID', true),
    laneEpoch: requireUint32(view.getUint32(41, false), 'lane epoch', true),
    grantOperationId: requireIdentity(encoded.subarray(45, 61), 'grant operation ID'),
    attachNonce: requireIdentity(encoded.subarray(61, 77), 'attach nonce'),
  })
  return fields
}

export async function v2LaneAcceptBody(
  hello: Uint8Array,
  senderNonce: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  if (
    hello.byteLength !== V2_LANE_HELLO_BYTES ||
    senderNonce.byteLength !== 16 ||
    !senderNonce.some((item) => item !== 0)
  ) {
    throw new V2LaneCodecError('input', 'lane accept semantic fields have invalid widths')
  }
  return concatBytes([
    LANE_ACCEPT_MAGIC,
    Uint8Array.of(V2_LANE_WIRE_VERSION),
    await sha256(hello, runtime),
    senderNonce,
  ])
}

async function laneResponsePreimage(
  domain: Uint8Array,
  body: Uint8Array,
  runtime: CryptoRuntime,
): Promise<Uint8Array<ArrayBuffer>> {
  return concatBytes([domain, await sha256(body, runtime)])
}

export async function verifyV2LaneAccept(
  encoded: Uint8Array,
  hello: Uint8Array,
  senderPublicKey: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  if (
    encoded.byteLength !== V2_LANE_ACCEPT_BYTES ||
    !hasMagic(encoded, LANE_ACCEPT_MAGIC) ||
    encoded[4] !== V2_LANE_WIRE_VERSION
  ) {
    throw new V2LaneCodecError('malformed', 'LaneAccept has an invalid header or length')
  }
  const body = encoded.subarray(0, V2_LANE_ACCEPT_BODY_BYTES)
  if (!equalBytes(body.subarray(5, 37), await sha256(hello, runtime))) {
    throw new V2LaneCodecError('signature', 'LaneAccept is bound to a different LaneHello')
  }
  const preimage = await laneResponsePreimage(LANE_ACCEPT_DOMAIN, body, runtime)
  if (
    !(await verifyEd25519(
      senderPublicKey,
      preimage,
      encoded.subarray(V2_LANE_ACCEPT_BODY_BYTES),
      runtime,
    ))
  ) {
    throw new V2LaneCodecError('signature', 'LaneAccept sender signature is invalid')
  }
  const senderNonce = body.slice(37, 53)
  if (!senderNonce.some((item) => item !== 0)) {
    throw new V2LaneCodecError('malformed', 'LaneAccept sender nonce must be nonzero')
  }
  return senderNonce
}

function validRejectCode(code: number): code is V2LaneRejectCode {
  return code >= V2_LANE_REJECT.unknownSession && code <= V2_LANE_REJECT.grantMismatch
}

export async function v2LaneRejectBody(
  hello: Uint8Array,
  rejection: V2LaneRejection,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  if (
    hello.byteLength !== V2_LANE_HELLO_BYTES ||
    !validRejectCode(rejection.code) ||
    !Number.isInteger(rejection.retryAfterMilliseconds) ||
    rejection.retryAfterMilliseconds < 0 ||
    rejection.retryAfterMilliseconds > V2_LANE_MAX_RETRY_AFTER_MILLISECONDS ||
    (rejection.code !== V2_LANE_REJECT.admissionLimited &&
      rejection.retryAfterMilliseconds !== 0)
  ) {
    throw new V2LaneCodecError('input', 'lane rejection semantic fields are invalid')
  }
  return concatBytes([
    LANE_REJECT_MAGIC,
    Uint8Array.of(V2_LANE_WIRE_VERSION, rejection.code, 0, 0),
    await sha256(hello, runtime),
    encodeUint32(rejection.retryAfterMilliseconds),
  ])
}

export async function verifyV2LaneReject(
  encoded: Uint8Array,
  hello: Uint8Array,
  senderPublicKey: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<V2LaneRejection> {
  if (
    encoded.byteLength !== V2_LANE_REJECT_BYTES ||
    !hasMagic(encoded, LANE_REJECT_MAGIC) ||
    encoded[4] !== V2_LANE_WIRE_VERSION ||
    encoded[6] !== 0 ||
    encoded[7] !== 0
  ) {
    throw new V2LaneCodecError('malformed', 'LaneReject has an invalid header or length')
  }
  const code = encoded[5] ?? 0
  const body = encoded.subarray(0, V2_LANE_REJECT_BODY_BYTES)
  const retryAfterMilliseconds = new DataView(
    encoded.buffer,
    encoded.byteOffset,
    encoded.byteLength,
  ).getUint32(40, false)
  if (
    !validRejectCode(code) ||
    retryAfterMilliseconds > V2_LANE_MAX_RETRY_AFTER_MILLISECONDS ||
    (code !== V2_LANE_REJECT.admissionLimited && retryAfterMilliseconds !== 0)
  ) {
    throw new V2LaneCodecError('malformed', 'LaneReject code or retry value is invalid')
  }
  if (!equalBytes(body.subarray(8, 40), await sha256(hello, runtime))) {
    throw new V2LaneCodecError('signature', 'LaneReject is bound to a different LaneHello')
  }
  const preimage = await laneResponsePreimage(LANE_REJECT_DOMAIN, body, runtime)
  if (
    !(await verifyEd25519(
      senderPublicKey,
      preimage,
      encoded.subarray(V2_LANE_REJECT_BODY_BYTES),
      runtime,
    ))
  ) {
    throw new V2LaneCodecError('signature', 'LaneReject sender signature is invalid')
  }
  return Object.freeze({ code, retryAfterMilliseconds })
}

type V2LaneResponseState = 'pending' | 'verifying' | 'settled'

// The response authority owns the one-use grant boundary on the receiver. Keeping
// this state beside signature verification prevents reconnect code from treating a
// valid but replayed accept/reject as a second channel decision.
export class V2LaneResponseAuthority {
  readonly #hello: Uint8Array<ArrayBuffer>
  readonly #senderPublicKey: Uint8Array<ArrayBuffer>
  readonly #runtime: CryptoRuntime
  #state: V2LaneResponseState = 'pending'

  constructor(
    hello: Uint8Array,
    senderPublicKey: Uint8Array,
    runtime: CryptoRuntime = defaultCryptoRuntime(),
  ) {
    if (hello.byteLength !== V2_LANE_HELLO_BYTES || senderPublicKey.byteLength !== 32) {
      throw new V2LaneCodecError('input', 'lane response authority has invalid identity widths')
    }
    this.#hello = hello.slice()
    this.#senderPublicKey = senderPublicKey.slice()
    this.#runtime = runtime
  }

  async accept(encoded: Uint8Array): Promise<Uint8Array<ArrayBuffer>> {
    return this.#consume(() =>
      verifyV2LaneAccept(
        encoded,
        this.#hello,
        this.#senderPublicKey,
        this.#runtime,
      ),
    )
  }

  async reject(encoded: Uint8Array): Promise<V2LaneRejection> {
    return this.#consume(() =>
      verifyV2LaneReject(
        encoded,
        this.#hello,
        this.#senderPublicKey,
        this.#runtime,
      ),
    )
  }

  async #consume<T>(verify: () => Promise<T>): Promise<T> {
    if (this.#state !== 'pending') {
      throw new V2LaneCodecError('proof', 'lane response authority is already consumed')
    }
    this.#state = 'verifying'
    try {
      const result = await verify()
      this.#state = 'settled'
      return result
    } catch (cause) {
      this.#state = 'pending'
      throw cause
    }
  }
}
