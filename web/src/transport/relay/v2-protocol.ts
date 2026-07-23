import { concatBytes, encodeUint32, equalBytes } from '../../crypto/bytes'
import { sha256 } from '../../crypto/digest'
import { type CryptoRuntime, defaultCryptoRuntime } from '../../crypto/webcrypto'
import type {
  V2Challenge,
  V2DescriptorDelivery,
  V2OpaqueRoute,
  V2RegisterInit,
  V2Registered,
  V2RegisterProof,
  V2RelayErrorFrame,
  V2SessionRetired,
  V2StopInit,
  V2StopProof,
} from './v2-protocol-types'

export type {
  V2Challenge,
  V2DescriptorDelivery,
  V2OpaqueRoute,
  V2RegisterInit,
  V2Registered,
  V2RegisterProof,
  V2RelayErrorFrame,
  V2SessionRetired,
  V2StopInit,
  V2StopProof,
} from './v2-protocol-types'

export const V2_RELAY_WIRE_VERSION = 2
export const V2_RELAY_SHARE_ID_BYTES = 12
export const V2_RELAY_SHARE_INSTANCE_BYTES = 16
export const V2_RELAY_PK_HASH_BYTES = 16
export const V2_RELAY_DIGEST_BYTES = 32
export const V2_RELAY_IDENTITY_BYTES = 32
export const V2_RELAY_CHALLENGE_ID_BYTES = 16
export const V2_RELAY_CHALLENGE_NONCE_BYTES = 32
export const V2_RELAY_SENDER_PUBLIC_KEY_BYTES = 32
export const V2_RELAY_SIGNATURE_BYTES = 64
export const V2_RELAY_RESUME_TOKEN_BYTES = 32
export const V2_RELAY_STOP_ID_BYTES = 16
export const V2_RELAY_SESSION_ID_BYTES = 8
export const V2_RELAY_MAX_DESCRIPTOR_BYTES = 16 << 10
export const V2_RELAY_MAX_OPAQUE_CIPHERTEXT_BYTES = 64 << 10

export const V2_REGISTRATION_MODE = Object.freeze({ fresh: 0, resume: 1 } as const)
export type V2RegistrationMode =
  (typeof V2_REGISTRATION_MODE)[keyof typeof V2_REGISTRATION_MODE]

export const V2_CHALLENGE_PURPOSE = Object.freeze({ register: 0, resume: 1, stop: 2 } as const)
export type V2ChallengePurpose =
  (typeof V2_CHALLENGE_PURPOSE)[keyof typeof V2_CHALLENGE_PURPOSE]

export const V2_RELAY_ERROR = Object.freeze({
  malformed: 1,
  unsupportedMode: 2,
  shareIdCollision: 3,
  alreadyRegistered: 4,
  challengeExpired: 5,
  invalidProof: 6,
  descriptorInvalid: 7,
  notFound: 8,
  starting: 9,
  admission: 10,
  stopped: 11,
} as const)
export type V2RelayErrorCode = (typeof V2_RELAY_ERROR)[keyof typeof V2_RELAY_ERROR]

const TEXT_ENCODER = new TextEncoder()
const SHARE_ID_DOMAIN = TEXT_ENCODER.encode('windshare/v2 share-id\0')
const REGISTER_DOMAIN = TEXT_ENCODER.encode('windshare/v2 relay-register\0')
const RESUME_DOMAIN = TEXT_ENCODER.encode('windshare/v2 relay-resume\0')
const STOP_DOMAIN = TEXT_ENCODER.encode('windshare/v2 relay-stop\0')
const MAX_UINT32 = 0xffff_ffff
const MAX_UINT64 = 0xffff_ffff_ffff_ffffn

const FRAME_BYTES = Object.freeze({
  registerInit: 116,
  challenge: 64,
  registerProof: 104,
  registered: 68,
  resumeCredential: 40,
  stopInit: 100,
  stopProof: 104,
  stopped: 24,
  join: 20,
  descriptorUploadHeader: 12,
  descriptorDeliveryHeader: 20,
  error: 12,
  opaqueHeader: 20,
  sessionRetired: 16,
})

export type V2RelayProtocolErrorKind = 'identity' | 'malformed' | 'mode' | 'purpose'

export class V2RelayProtocolError extends Error {
  readonly kind: V2RelayProtocolErrorKind

  constructor(kind: V2RelayProtocolErrorKind, message: string) {
    super(message)
    this.name = 'V2RelayProtocolError'
    this.kind = kind
  }
}

function nonzero(value: Uint8Array): boolean {
  return value.some((item) => item !== 0)
}

function requireBytes(
  value: Uint8Array,
  length: number,
  label: string,
  requireNonzero = false,
): Uint8Array<ArrayBuffer> {
  if (value.byteLength !== length || (requireNonzero && !nonzero(value))) {
    throw new V2RelayProtocolError('identity', `${label} has an invalid value or width`)
  }
  return value.slice()
}

function requireMode(mode: number): V2RegistrationMode {
  if (mode !== V2_REGISTRATION_MODE.fresh && mode !== V2_REGISTRATION_MODE.resume) {
    throw new V2RelayProtocolError('mode', 'registration mode is unsupported')
  }
  return mode
}

function requirePurpose(purpose: number): V2ChallengePurpose {
  if (purpose < V2_CHALLENGE_PURPOSE.register || purpose > V2_CHALLENGE_PURPOSE.stop) {
    throw new V2RelayProtocolError('purpose', 'challenge purpose is invalid')
  }
  return purpose as V2ChallengePurpose
}

function encodeUint16(value: number): Uint8Array<ArrayBuffer> {
  const encoded = new Uint8Array(2)
  new DataView(encoded.buffer).setUint16(0, value, false)
  return encoded
}

function encodeUint64(value: bigint): Uint8Array<ArrayBuffer> {
  if (value < 0n || value > MAX_UINT64) {
    throw new V2RelayProtocolError('malformed', 'value is outside the unsigned 64-bit domain')
  }
  const encoded = new Uint8Array(8)
  new DataView(encoded.buffer).setBigUint64(0, value, false)
  return encoded
}

function prefix(magic: string, discriminator: number): Uint8Array<ArrayBuffer> {
  return concatBytes([
    TEXT_ENCODER.encode(magic),
    Uint8Array.of(V2_RELAY_WIRE_VERSION, discriminator, 0, 0),
  ])
}

function reservedPrefix(magic: string): Uint8Array<ArrayBuffer> {
  return prefix(magic, 0)
}

function validPrefix(encoded: Uint8Array, magic: string, discriminator?: number): boolean {
  return (
    equalBytes(encoded.subarray(0, 4), TEXT_ENCODER.encode(magic)) &&
    encoded[4] === V2_RELAY_WIRE_VERSION &&
    (discriminator === undefined ? encoded[5] === 0 : encoded[5] === discriminator) &&
    encoded[6] === 0 &&
    encoded[7] === 0
  )
}

async function validateRouteIdentity(
  shareId: Uint8Array,
  pkHash: Uint8Array,
  runtime: CryptoRuntime,
): Promise<void> {
  requireBytes(shareId, V2_RELAY_SHARE_ID_BYTES, 'share ID')
  requireBytes(pkHash, V2_RELAY_PK_HASH_BYTES, 'pkHash')
  const expected = (await sha256(concatBytes([SHARE_ID_DOMAIN, pkHash]), runtime)).subarray(
    0,
    V2_RELAY_SHARE_ID_BYTES,
  )
  if (!equalBytes(expected, shareId)) {
    throw new V2RelayProtocolError('identity', 'share ID is not the image of pkHash')
  }
}

export async function encodeV2RegisterInit(
  frame: V2RegisterInit,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  const mode = requireMode(frame.mode)
  await validateRouteIdentity(frame.shareId, frame.pkHash, runtime)
  return concatBytes([
    prefix('WS2R', mode),
    frame.shareId,
    requireBytes(frame.shareInstance, V2_RELAY_SHARE_INSTANCE_BYTES, 'share instance', true),
    frame.pkHash,
    requireBytes(frame.descriptorDigest, V2_RELAY_DIGEST_BYTES, 'descriptor digest'),
    requireBytes(frame.resumeTokenHash, V2_RELAY_DIGEST_BYTES, 'resume token hash'),
  ])
}

export async function decodeV2RegisterInit(
  encoded: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<V2RegisterInit> {
  if (encoded.byteLength !== FRAME_BYTES.registerInit || !validPrefix(encoded, 'WS2R', encoded[5])) {
    throw new V2RelayProtocolError('malformed', 'REGISTER_INIT has an invalid header or length')
  }
  const frame = Object.freeze({
    mode: requireMode(encoded[5] ?? -1),
    shareId: encoded.slice(8, 20),
    shareInstance: requireBytes(encoded.subarray(20, 36), 16, 'share instance', true),
    pkHash: encoded.slice(36, 52),
    descriptorDigest: encoded.slice(52, 84),
    resumeTokenHash: encoded.slice(84, 116),
  })
  await validateRouteIdentity(frame.shareId, frame.pkHash, runtime)
  return frame
}

export function encodeV2Challenge(frame: V2Challenge): Uint8Array<ArrayBuffer> {
  const purpose = requirePurpose(frame.purpose)
  if (frame.expiresAtUnixSeconds === 0n) {
    throw new V2RelayProtocolError('identity', 'challenge expiry must be nonzero')
  }
  return concatBytes([
    prefix('WS2Q', purpose),
    requireBytes(frame.id, V2_RELAY_CHALLENGE_ID_BYTES, 'challenge ID', true),
    requireBytes(frame.nonce, V2_RELAY_CHALLENGE_NONCE_BYTES, 'challenge nonce', true),
    encodeUint64(frame.expiresAtUnixSeconds),
  ])
}

export function decodeV2Challenge(encoded: Uint8Array): V2Challenge {
  if (encoded.byteLength !== FRAME_BYTES.challenge || !validPrefix(encoded, 'WS2Q', encoded[5])) {
    throw new V2RelayProtocolError('malformed', 'challenge has an invalid header or length')
  }
  const view = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength)
  const frame = Object.freeze({
    purpose: requirePurpose(encoded[5] ?? -1),
    id: requireBytes(encoded.subarray(8, 24), 16, 'challenge ID', true),
    nonce: requireBytes(encoded.subarray(24, 56), 32, 'challenge nonce', true),
    expiresAtUnixSeconds: view.getBigUint64(56, false),
  })
  if (frame.expiresAtUnixSeconds === 0n) {
    throw new V2RelayProtocolError('identity', 'challenge expiry must be nonzero')
  }
  return frame
}

function purposeForMode(mode: V2RegistrationMode): V2ChallengePurpose {
  return mode === V2_REGISTRATION_MODE.fresh
    ? V2_CHALLENGE_PURPOSE.register
    : V2_CHALLENGE_PURPOSE.resume
}

export async function v2RegistrationProofPreimage(
  init: V2RegisterInit,
  challenge: V2Challenge,
  relayIdentity: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  await encodeV2RegisterInit(init, runtime)
  if (challenge.purpose !== purposeForMode(init.mode)) {
    throw new V2RelayProtocolError('purpose', 'challenge purpose does not match registration mode')
  }
  encodeV2Challenge(challenge)
  return concatBytes([
    init.mode === V2_REGISTRATION_MODE.fresh ? REGISTER_DOMAIN : RESUME_DOMAIN,
    init.shareId,
    init.shareInstance,
    init.pkHash,
    init.descriptorDigest,
    init.resumeTokenHash,
    requireBytes(relayIdentity, V2_RELAY_IDENTITY_BYTES, 'relay identity', true),
    challenge.id,
    challenge.nonce,
    encodeUint64(challenge.expiresAtUnixSeconds),
  ])
}

export function encodeV2RegisterProof(frame: V2RegisterProof): Uint8Array<ArrayBuffer> {
  return concatBytes([
    prefix('WS2P', requireMode(frame.mode)),
    requireBytes(frame.senderPublicKey, V2_RELAY_SENDER_PUBLIC_KEY_BYTES, 'sender public key', true),
    requireBytes(frame.signature, V2_RELAY_SIGNATURE_BYTES, 'registration signature', true),
  ])
}

export function decodeV2RegisterProof(encoded: Uint8Array): V2RegisterProof {
  if (encoded.byteLength !== FRAME_BYTES.registerProof || !validPrefix(encoded, 'WS2P', encoded[5])) {
    throw new V2RelayProtocolError('malformed', 'REGISTER_PROOF has an invalid header or length')
  }
  return Object.freeze({
    mode: requireMode(encoded[5] ?? -1),
    senderPublicKey: requireBytes(encoded.subarray(8, 40), 32, 'sender public key', true),
    signature: requireBytes(encoded.subarray(40), 64, 'registration signature', true),
  })
}

export function encodeV2ResumeCredential(token: Uint8Array): Uint8Array<ArrayBuffer> {
  return concatBytes([
    reservedPrefix('WS2T'),
    requireBytes(token, V2_RELAY_RESUME_TOKEN_BYTES, 'resume token', true),
  ])
}

export function decodeV2ResumeCredential(encoded: Uint8Array): Uint8Array<ArrayBuffer> {
  if (encoded.byteLength !== FRAME_BYTES.resumeCredential || !validPrefix(encoded, 'WS2T')) {
    throw new V2RelayProtocolError('malformed', 'RESUME_CREDENTIAL has an invalid header or length')
  }
  return requireBytes(encoded.subarray(8), 32, 'resume token', true)
}

export async function encodeV2StopInit(
  frame: V2StopInit,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  await validateRouteIdentity(frame.shareId, frame.pkHash, runtime)
  return concatBytes([
    reservedPrefix('WS2X'),
    frame.shareId,
    requireBytes(frame.shareInstance, 16, 'share instance', true),
    frame.pkHash,
    requireBytes(frame.relayIdentity, 32, 'relay identity', true),
    requireBytes(frame.stopId, 16, 'stop ID', true),
  ])
}

export async function decodeV2StopInit(
  encoded: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<V2StopInit> {
  if (encoded.byteLength !== FRAME_BYTES.stopInit || !validPrefix(encoded, 'WS2X')) {
    throw new V2RelayProtocolError('malformed', 'STOP_INIT has an invalid header or length')
  }
  const frame = Object.freeze({
    shareId: encoded.slice(8, 20),
    shareInstance: requireBytes(encoded.subarray(20, 36), 16, 'share instance', true),
    pkHash: encoded.slice(36, 52),
    relayIdentity: requireBytes(encoded.subarray(52, 84), 32, 'relay identity', true),
    stopId: requireBytes(encoded.subarray(84), 16, 'stop ID', true),
  })
  await validateRouteIdentity(frame.shareId, frame.pkHash, runtime)
  return frame
}

export async function v2StopProofPreimage(
  init: V2StopInit,
  challenge: V2Challenge,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  await encodeV2StopInit(init, runtime)
  if (challenge.purpose !== V2_CHALLENGE_PURPOSE.stop) {
    throw new V2RelayProtocolError('purpose', 'STOP requires a purpose-2 challenge')
  }
  encodeV2Challenge(challenge)
  return concatBytes([
    STOP_DOMAIN,
    init.shareId,
    init.shareInstance,
    init.pkHash,
    init.relayIdentity,
    init.stopId,
    challenge.id,
    challenge.nonce,
    encodeUint64(challenge.expiresAtUnixSeconds),
  ])
}

export function encodeV2StopProof(frame: V2StopProof): Uint8Array<ArrayBuffer> {
  return concatBytes([
    reservedPrefix('WS2V'),
    requireBytes(frame.senderPublicKey, 32, 'sender public key', true),
    requireBytes(frame.signature, 64, 'stop signature', true),
  ])
}

export function decodeV2StopProof(encoded: Uint8Array): V2StopProof {
  if (encoded.byteLength !== FRAME_BYTES.stopProof || !validPrefix(encoded, 'WS2V')) {
    throw new V2RelayProtocolError('malformed', 'STOP_PROOF has an invalid header or length')
  }
  return Object.freeze({
    senderPublicKey: requireBytes(encoded.subarray(8, 40), 32, 'sender public key', true),
    signature: requireBytes(encoded.subarray(40), 64, 'stop signature', true),
  })
}

export function encodeV2Registered(frame: V2Registered): Uint8Array<ArrayBuffer> {
  return concatBytes([
    reservedPrefix('WS2K'),
    requireBytes(frame.shareId, 12, 'share ID', true),
    requireBytes(frame.shareInstance, 16, 'share instance', true),
    requireBytes(frame.descriptorDigest, 32, 'descriptor digest'),
  ])
}

export function decodeV2Registered(encoded: Uint8Array): V2Registered {
  if (encoded.byteLength !== FRAME_BYTES.registered || !validPrefix(encoded, 'WS2K')) {
    throw new V2RelayProtocolError('malformed', 'REGISTERED has an invalid header or length')
  }
  return Object.freeze({
    shareId: requireBytes(encoded.subarray(8, 20), 12, 'share ID', true),
    shareInstance: requireBytes(encoded.subarray(20, 36), 16, 'share instance', true),
    descriptorDigest: encoded.slice(36),
  })
}

export function encodeV2Stopped(stopId: Uint8Array): Uint8Array<ArrayBuffer> {
  return concatBytes([reservedPrefix('WS2Y'), requireBytes(stopId, 16, 'stop ID', true)])
}

export function decodeV2Stopped(encoded: Uint8Array): Uint8Array<ArrayBuffer> {
  if (encoded.byteLength !== FRAME_BYTES.stopped || !validPrefix(encoded, 'WS2Y')) {
    throw new V2RelayProtocolError('malformed', 'STOPPED has an invalid header or length')
  }
  return requireBytes(encoded.subarray(8), 16, 'stop ID', true)
}

export function encodeV2Join(shareId: Uint8Array): Uint8Array<ArrayBuffer> {
  return concatBytes([reservedPrefix('WS2J'), requireBytes(shareId, 12, 'share ID', true)])
}

export function decodeV2Join(encoded: Uint8Array): Uint8Array<ArrayBuffer> {
  if (encoded.byteLength !== FRAME_BYTES.join || !validPrefix(encoded, 'WS2J')) {
    throw new V2RelayProtocolError('malformed', 'JOIN has an invalid header or length')
  }
  return requireBytes(encoded.subarray(8), 12, 'share ID', true)
}

function encodeVariable(
  magic: 'WS2U' | 'WS2D',
  relaySessionId: Uint8Array | undefined,
  object: Uint8Array,
): Uint8Array<ArrayBuffer> {
  if (object.byteLength === 0 || object.byteLength > V2_RELAY_MAX_DESCRIPTOR_BYTES) {
    throw new V2RelayProtocolError('malformed', 'descriptor object has an invalid length')
  }
  const identity = relaySessionId === undefined
    ? new Uint8Array(0)
    : requireBytes(relaySessionId, 8, 'relay session ID', true)
  return concatBytes([
    reservedPrefix(magic),
    identity,
    encodeUint32(object.byteLength),
    object,
  ])
}

function decodeVariable(
  encoded: Uint8Array,
  magic: 'WS2U' | 'WS2D',
  headerBytes: number,
): Uint8Array<ArrayBuffer> {
  if (encoded.byteLength < headerBytes || !validPrefix(encoded, magic)) {
    throw new V2RelayProtocolError('malformed', `${magic} has an invalid header or length`)
  }
  const lengthOffset = headerBytes - 4
  const length = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength).getUint32(
    lengthOffset,
    false,
  )
  if (
    length === 0 ||
    length > V2_RELAY_MAX_DESCRIPTOR_BYTES ||
    headerBytes + length !== encoded.byteLength
  ) {
    throw new V2RelayProtocolError('malformed', `${magic} descriptor length is inconsistent`)
  }
  return encoded.slice(headerBytes)
}

export function encodeV2DescriptorUpload(object: Uint8Array): Uint8Array<ArrayBuffer> {
  return encodeVariable('WS2U', undefined, object)
}

export function decodeV2DescriptorUpload(encoded: Uint8Array): Uint8Array<ArrayBuffer> {
  return decodeVariable(encoded, 'WS2U', FRAME_BYTES.descriptorUploadHeader)
}

export function encodeV2DescriptorDelivery(
  frame: V2DescriptorDelivery,
): Uint8Array<ArrayBuffer> {
  return encodeVariable('WS2D', frame.relaySessionId, frame.object)
}

export function decodeV2DescriptorDelivery(encoded: Uint8Array): V2DescriptorDelivery {
  if (encoded.byteLength < FRAME_BYTES.descriptorDeliveryHeader || !validPrefix(encoded, 'WS2D')) {
    throw new V2RelayProtocolError('malformed', 'WS2D has an invalid header or length')
  }
  return Object.freeze({
    relaySessionId: requireBytes(encoded.subarray(8, 16), 8, 'relay session ID', true),
    object: decodeVariable(encoded, 'WS2D', FRAME_BYTES.descriptorDeliveryHeader),
  })
}

export function encodeV2OpaqueRoute(frame: V2OpaqueRoute): Uint8Array<ArrayBuffer> {
  if (
    frame.ciphertext.byteLength === 0 ||
    frame.ciphertext.byteLength > V2_RELAY_MAX_OPAQUE_CIPHERTEXT_BYTES
  ) {
    throw new V2RelayProtocolError('malformed', 'opaque ciphertext has an invalid length')
  }
  return concatBytes([
    reservedPrefix('WS2O'),
    requireBytes(frame.relaySessionId, 8, 'relay session ID', true),
    encodeUint32(frame.ciphertext.byteLength),
    frame.ciphertext,
  ])
}

export function decodeV2OpaqueRoute(encoded: Uint8Array): V2OpaqueRoute {
  if (encoded.byteLength < FRAME_BYTES.opaqueHeader || !validPrefix(encoded, 'WS2O')) {
    throw new V2RelayProtocolError('malformed', 'WS2O has an invalid header or length')
  }
  const length = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength).getUint32(
    16,
    false,
  )
  if (
    length === 0 ||
    length > V2_RELAY_MAX_OPAQUE_CIPHERTEXT_BYTES ||
    FRAME_BYTES.opaqueHeader + length !== encoded.byteLength
  ) {
    throw new V2RelayProtocolError('malformed', 'WS2O ciphertext length is inconsistent')
  }
  return Object.freeze({
    relaySessionId: requireBytes(encoded.subarray(8, 16), 8, 'relay session ID', true),
    ciphertext: encoded.slice(FRAME_BYTES.opaqueHeader),
  })
}

export function encodeV2SessionRetired(frame: V2SessionRetired): Uint8Array<ArrayBuffer> {
  return concatBytes([
    reservedPrefix('WS2F'),
    requireBytes(frame.relaySessionId, V2_RELAY_SESSION_ID_BYTES, 'relay session ID', true),
  ])
}

export function decodeV2SessionRetired(encoded: Uint8Array): V2SessionRetired {
  if (encoded.byteLength !== FRAME_BYTES.sessionRetired || !validPrefix(encoded, 'WS2F')) {
    throw new V2RelayProtocolError('malformed', 'WS2F has an invalid header or length')
  }
  return Object.freeze({
    relaySessionId: requireBytes(
      encoded.subarray(8),
      V2_RELAY_SESSION_ID_BYTES,
      'relay session ID',
      true,
    ),
  })
}

function validErrorCode(code: number): code is V2RelayErrorCode {
  return code >= V2_RELAY_ERROR.malformed && code <= V2_RELAY_ERROR.stopped
}

export function encodeV2RelayError(frame: V2RelayErrorFrame): Uint8Array<ArrayBuffer> {
  const retryAllowed =
    frame.code === V2_RELAY_ERROR.starting || frame.code === V2_RELAY_ERROR.admission
  if (
    !validErrorCode(frame.code) ||
    !Number.isInteger(frame.retryAfterMilliseconds) ||
    frame.retryAfterMilliseconds < 0 ||
    frame.retryAfterMilliseconds > 30_000 ||
    (!retryAllowed && frame.retryAfterMilliseconds !== 0) ||
    frame.retryAfterMilliseconds > MAX_UINT32
  ) {
    throw new V2RelayProtocolError('malformed', 'relay error code or retry value is invalid')
  }
  return concatBytes([
    TEXT_ENCODER.encode('WS2E'),
    Uint8Array.of(V2_RELAY_WIRE_VERSION, 0),
    encodeUint16(frame.code),
    encodeUint32(frame.retryAfterMilliseconds),
  ])
}

export function decodeV2RelayError(encoded: Uint8Array): V2RelayErrorFrame {
  if (
    encoded.byteLength !== FRAME_BYTES.error ||
    !equalBytes(encoded.subarray(0, 4), TEXT_ENCODER.encode('WS2E')) ||
    encoded[4] !== V2_RELAY_WIRE_VERSION ||
    encoded[5] !== 0
  ) {
    throw new V2RelayProtocolError('malformed', 'WS2E has an invalid header or length')
  }
  const view = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength)
  const code = view.getUint16(6, false)
  const retryAfterMilliseconds = view.getUint32(8, false)
  if (!validErrorCode(code)) {
    throw new V2RelayProtocolError('malformed', 'relay error code is unknown')
  }
  const frame = { code, retryAfterMilliseconds }
  encodeV2RelayError(frame)
  return Object.freeze(frame)
}
