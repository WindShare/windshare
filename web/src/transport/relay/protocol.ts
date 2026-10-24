import { concatBytes, encodeUint32, equalBytes } from '../../crypto/bytes'
import { sha256 } from '../../crypto/digest'
import { type CryptoRuntime, defaultCryptoRuntime } from '../../crypto/webcrypto'
import type {
  Challenge,
  DescriptorDelivery,
  OpaqueRoute,
  RegisterInit,
  Registered,
  RegisterProof,
  RelayErrorFrame,
  SessionRetired,
  SessionAdmitted,
  SessionCredit,
  StopInit,
  StopProof,
} from './protocol-types'

export type {
  Challenge,
  DescriptorDelivery,
  OpaqueRoute,
  RegisterInit,
  Registered,
  RegisterProof,
  RelayErrorFrame,
  SessionRetired,
  SessionAdmitted,
  SessionCredit,
  StopInit,
  StopProof,
} from './protocol-types'

export const RELAY_WIRE_VERSION = 2
export const RELAY_SHARE_ID_BYTES = 12
export const RELAY_SHARE_INSTANCE_BYTES = 16
export const RELAY_PK_HASH_BYTES = 16
export const RELAY_DIGEST_BYTES = 32
export const RELAY_IDENTITY_BYTES = 32
export const RELAY_CHALLENGE_ID_BYTES = 16
export const RELAY_CHALLENGE_NONCE_BYTES = 32
export const RELAY_SENDER_PUBLIC_KEY_BYTES = 32
export const RELAY_SIGNATURE_BYTES = 64
export const RELAY_RESUME_TOKEN_BYTES = 32
export const RELAY_STOP_ID_BYTES = 16
export const RELAY_SESSION_ID_BYTES = 8
export const RELAY_MAX_DESCRIPTOR_BYTES = 16 << 10
export const RELAY_MAX_OPAQUE_CIPHERTEXT_BYTES = 64 << 10
export const RELAY_OPAQUE_ROUTE_HEADER_BYTES = 20
export const RELAY_SENDER_WINDOW_FRAMES = 64
export const RELAY_SENDER_WINDOW_BYTES = 4 << 20

export const REGISTRATION_MODE = Object.freeze({ fresh: 0, resume: 1 } as const)
export type RegistrationMode =
  (typeof REGISTRATION_MODE)[keyof typeof REGISTRATION_MODE]

export const CHALLENGE_PURPOSE = Object.freeze({ register: 0, resume: 1, stop: 2 } as const)
export type ChallengePurpose =
  (typeof CHALLENGE_PURPOSE)[keyof typeof CHALLENGE_PURPOSE]

export const RELAY_ERROR = Object.freeze({
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
  resumeStale: 12,
  accessDenied: 13,
  accessUnavailable: 14,
} as const)
export type RelayErrorCode = (typeof RELAY_ERROR)[keyof typeof RELAY_ERROR]

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
  opaqueHeader: RELAY_OPAQUE_ROUTE_HEADER_BYTES,
  sessionRetired: 16,
  sessionAdmitted: 16,
  sessionCredit: 24,
})

export type RelayProtocolErrorKind = 'identity' | 'malformed' | 'mode' | 'purpose'

export class RelayProtocolError extends Error {
  readonly kind: RelayProtocolErrorKind

  constructor(kind: RelayProtocolErrorKind, message: string) {
    super(message)
    this.name = 'RelayProtocolError'
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
    throw new RelayProtocolError('identity', `${label} has an invalid value or width`)
  }
  return value.slice()
}

function requireMode(mode: number): RegistrationMode {
  if (mode !== REGISTRATION_MODE.fresh && mode !== REGISTRATION_MODE.resume) {
    throw new RelayProtocolError('mode', 'registration mode is unsupported')
  }
  return mode
}

function requirePurpose(purpose: number): ChallengePurpose {
  if (purpose < CHALLENGE_PURPOSE.register || purpose > CHALLENGE_PURPOSE.stop) {
    throw new RelayProtocolError('purpose', 'challenge purpose is invalid')
  }
  return purpose as ChallengePurpose
}

function encodeUint16(value: number): Uint8Array<ArrayBuffer> {
  const encoded = new Uint8Array(2)
  new DataView(encoded.buffer).setUint16(0, value, false)
  return encoded
}

function encodeUint64(value: bigint): Uint8Array<ArrayBuffer> {
  if (value < 0n || value > MAX_UINT64) {
    throw new RelayProtocolError('malformed', 'value is outside the unsigned 64-bit domain')
  }
  const encoded = new Uint8Array(8)
  new DataView(encoded.buffer).setBigUint64(0, value, false)
  return encoded
}

function prefix(magic: string, discriminator: number): Uint8Array<ArrayBuffer> {
  return concatBytes([
    TEXT_ENCODER.encode(magic),
    Uint8Array.of(RELAY_WIRE_VERSION, discriminator, 0, 0),
  ])
}

function reservedPrefix(magic: string): Uint8Array<ArrayBuffer> {
  return prefix(magic, 0)
}

function validPrefix(encoded: Uint8Array, magic: string, discriminator?: number): boolean {
  return (
    equalBytes(encoded.subarray(0, 4), TEXT_ENCODER.encode(magic)) &&
    encoded[4] === RELAY_WIRE_VERSION &&
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
  requireBytes(shareId, RELAY_SHARE_ID_BYTES, 'share ID')
  requireBytes(pkHash, RELAY_PK_HASH_BYTES, 'pkHash')
  const expected = (await sha256(concatBytes([SHARE_ID_DOMAIN, pkHash]), runtime)).subarray(
    0,
    RELAY_SHARE_ID_BYTES,
  )
  if (!equalBytes(expected, shareId)) {
    throw new RelayProtocolError('identity', 'share ID is not the image of pkHash')
  }
}

export async function encodeRegisterInit(
  frame: RegisterInit,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  const mode = requireMode(frame.mode)
  await validateRouteIdentity(frame.shareId, frame.pkHash, runtime)
  return concatBytes([
    prefix('WS2R', mode),
    frame.shareId,
    requireBytes(frame.shareInstance, RELAY_SHARE_INSTANCE_BYTES, 'share instance', true),
    frame.pkHash,
    requireBytes(frame.descriptorDigest, RELAY_DIGEST_BYTES, 'descriptor digest'),
    requireBytes(frame.resumeTokenHash, RELAY_DIGEST_BYTES, 'resume token hash'),
  ])
}

export async function decodeRegisterInit(
  encoded: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<RegisterInit> {
  if (encoded.byteLength !== FRAME_BYTES.registerInit || !validPrefix(encoded, 'WS2R', encoded[5])) {
    throw new RelayProtocolError('malformed', 'REGISTER_INIT has an invalid header or length')
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

export function encodeChallenge(frame: Challenge): Uint8Array<ArrayBuffer> {
  const purpose = requirePurpose(frame.purpose)
  if (frame.expiresAtUnixSeconds === 0n) {
    throw new RelayProtocolError('identity', 'challenge expiry must be nonzero')
  }
  return concatBytes([
    prefix('WS2Q', purpose),
    requireBytes(frame.id, RELAY_CHALLENGE_ID_BYTES, 'challenge ID', true),
    requireBytes(frame.nonce, RELAY_CHALLENGE_NONCE_BYTES, 'challenge nonce', true),
    encodeUint64(frame.expiresAtUnixSeconds),
  ])
}

export function decodeChallenge(encoded: Uint8Array): Challenge {
  if (encoded.byteLength !== FRAME_BYTES.challenge || !validPrefix(encoded, 'WS2Q', encoded[5])) {
    throw new RelayProtocolError('malformed', 'challenge has an invalid header or length')
  }
  const view = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength)
  const frame = Object.freeze({
    purpose: requirePurpose(encoded[5] ?? -1),
    id: requireBytes(encoded.subarray(8, 24), 16, 'challenge ID', true),
    nonce: requireBytes(encoded.subarray(24, 56), 32, 'challenge nonce', true),
    expiresAtUnixSeconds: view.getBigUint64(56, false),
  })
  if (frame.expiresAtUnixSeconds === 0n) {
    throw new RelayProtocolError('identity', 'challenge expiry must be nonzero')
  }
  return frame
}

function purposeForMode(mode: RegistrationMode): ChallengePurpose {
  return mode === REGISTRATION_MODE.fresh
    ? CHALLENGE_PURPOSE.register
    : CHALLENGE_PURPOSE.resume
}

export async function registrationProofPreimage(
  init: RegisterInit,
  challenge: Challenge,
  relayIdentity: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  await encodeRegisterInit(init, runtime)
  if (challenge.purpose !== purposeForMode(init.mode)) {
    throw new RelayProtocolError('purpose', 'challenge purpose does not match registration mode')
  }
  encodeChallenge(challenge)
  return concatBytes([
    init.mode === REGISTRATION_MODE.fresh ? REGISTER_DOMAIN : RESUME_DOMAIN,
    init.shareId,
    init.shareInstance,
    init.pkHash,
    init.descriptorDigest,
    init.resumeTokenHash,
    requireBytes(relayIdentity, RELAY_IDENTITY_BYTES, 'relay identity', true),
    challenge.id,
    challenge.nonce,
    encodeUint64(challenge.expiresAtUnixSeconds),
  ])
}

export function encodeRegisterProof(frame: RegisterProof): Uint8Array<ArrayBuffer> {
  return concatBytes([
    prefix('WS2P', requireMode(frame.mode)),
    requireBytes(frame.senderPublicKey, RELAY_SENDER_PUBLIC_KEY_BYTES, 'sender public key', true),
    requireBytes(frame.signature, RELAY_SIGNATURE_BYTES, 'registration signature', true),
  ])
}

export function decodeRegisterProof(encoded: Uint8Array): RegisterProof {
  if (encoded.byteLength !== FRAME_BYTES.registerProof || !validPrefix(encoded, 'WS2P', encoded[5])) {
    throw new RelayProtocolError('malformed', 'REGISTER_PROOF has an invalid header or length')
  }
  return Object.freeze({
    mode: requireMode(encoded[5] ?? -1),
    senderPublicKey: requireBytes(encoded.subarray(8, 40), 32, 'sender public key', true),
    signature: requireBytes(encoded.subarray(40), 64, 'registration signature', true),
  })
}

export function encodeResumeCredential(token: Uint8Array): Uint8Array<ArrayBuffer> {
  return concatBytes([
    reservedPrefix('WS2T'),
    requireBytes(token, RELAY_RESUME_TOKEN_BYTES, 'resume token', true),
  ])
}

export function decodeResumeCredential(encoded: Uint8Array): Uint8Array<ArrayBuffer> {
  if (encoded.byteLength !== FRAME_BYTES.resumeCredential || !validPrefix(encoded, 'WS2T')) {
    throw new RelayProtocolError('malformed', 'RESUME_CREDENTIAL has an invalid header or length')
  }
  return requireBytes(encoded.subarray(8), 32, 'resume token', true)
}

export async function encodeStopInit(
  frame: StopInit,
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

export async function decodeStopInit(
  encoded: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<StopInit> {
  if (encoded.byteLength !== FRAME_BYTES.stopInit || !validPrefix(encoded, 'WS2X')) {
    throw new RelayProtocolError('malformed', 'STOP_INIT has an invalid header or length')
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

export async function stopProofPreimage(
  init: StopInit,
  challenge: Challenge,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  await encodeStopInit(init, runtime)
  if (challenge.purpose !== CHALLENGE_PURPOSE.stop) {
    throw new RelayProtocolError('purpose', 'STOP requires a purpose-2 challenge')
  }
  encodeChallenge(challenge)
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

export function encodeStopProof(frame: StopProof): Uint8Array<ArrayBuffer> {
  return concatBytes([
    reservedPrefix('WS2V'),
    requireBytes(frame.senderPublicKey, 32, 'sender public key', true),
    requireBytes(frame.signature, 64, 'stop signature', true),
  ])
}

export function decodeStopProof(encoded: Uint8Array): StopProof {
  if (encoded.byteLength !== FRAME_BYTES.stopProof || !validPrefix(encoded, 'WS2V')) {
    throw new RelayProtocolError('malformed', 'STOP_PROOF has an invalid header or length')
  }
  return Object.freeze({
    senderPublicKey: requireBytes(encoded.subarray(8, 40), 32, 'sender public key', true),
    signature: requireBytes(encoded.subarray(40), 64, 'stop signature', true),
  })
}

export function encodeRegistered(frame: Registered): Uint8Array<ArrayBuffer> {
  return concatBytes([
    reservedPrefix('WS2K'),
    requireBytes(frame.shareId, 12, 'share ID', true),
    requireBytes(frame.shareInstance, 16, 'share instance', true),
    requireBytes(frame.descriptorDigest, 32, 'descriptor digest'),
  ])
}

export function decodeRegistered(encoded: Uint8Array): Registered {
  if (encoded.byteLength !== FRAME_BYTES.registered || !validPrefix(encoded, 'WS2K')) {
    throw new RelayProtocolError('malformed', 'REGISTERED has an invalid header or length')
  }
  return Object.freeze({
    shareId: requireBytes(encoded.subarray(8, 20), 12, 'share ID', true),
    shareInstance: requireBytes(encoded.subarray(20, 36), 16, 'share instance', true),
    descriptorDigest: encoded.slice(36),
  })
}

/** Confirms durable revocation and unsubmitted-queue discard, independently of delivery settlement. */
export function encodeStopped(stopId: Uint8Array): Uint8Array<ArrayBuffer> {
  return concatBytes([reservedPrefix('WS2Y'), requireBytes(stopId, 16, 'stop ID', true)])
}

/** Returns the STOP transaction ID; previously submitted bytes may still arrive on old connections. */
export function decodeStopped(encoded: Uint8Array): Uint8Array<ArrayBuffer> {
  if (encoded.byteLength !== FRAME_BYTES.stopped || !validPrefix(encoded, 'WS2Y')) {
    throw new RelayProtocolError('malformed', 'STOPPED has an invalid header or length')
  }
  return requireBytes(encoded.subarray(8), 16, 'stop ID', true)
}

export function encodeJoin(shareId: Uint8Array): Uint8Array<ArrayBuffer> {
  return concatBytes([reservedPrefix('WS2J'), requireBytes(shareId, 12, 'share ID', true)])
}

export function decodeJoin(encoded: Uint8Array): Uint8Array<ArrayBuffer> {
  if (encoded.byteLength !== FRAME_BYTES.join || !validPrefix(encoded, 'WS2J')) {
    throw new RelayProtocolError('malformed', 'JOIN has an invalid header or length')
  }
  return requireBytes(encoded.subarray(8), 12, 'share ID', true)
}

function encodeVariable(
  magic: 'WS2U' | 'WS2D',
  relaySessionId: Uint8Array | undefined,
  object: Uint8Array,
): Uint8Array<ArrayBuffer> {
  if (object.byteLength === 0 || object.byteLength > RELAY_MAX_DESCRIPTOR_BYTES) {
    throw new RelayProtocolError('malformed', 'descriptor object has an invalid length')
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
    throw new RelayProtocolError('malformed', `${magic} has an invalid header or length`)
  }
  const lengthOffset = headerBytes - 4
  const length = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength).getUint32(
    lengthOffset,
    false,
  )
  if (
    length === 0 ||
    length > RELAY_MAX_DESCRIPTOR_BYTES ||
    headerBytes + length !== encoded.byteLength
  ) {
    throw new RelayProtocolError('malformed', `${magic} descriptor length is inconsistent`)
  }
  return encoded.slice(headerBytes)
}

export function encodeDescriptorUpload(object: Uint8Array): Uint8Array<ArrayBuffer> {
  return encodeVariable('WS2U', undefined, object)
}

export function decodeDescriptorUpload(encoded: Uint8Array): Uint8Array<ArrayBuffer> {
  return decodeVariable(encoded, 'WS2U', FRAME_BYTES.descriptorUploadHeader)
}

export function encodeDescriptorDelivery(
  frame: DescriptorDelivery,
): Uint8Array<ArrayBuffer> {
  return encodeVariable('WS2D', frame.relaySessionId, frame.object)
}

export function decodeDescriptorDelivery(encoded: Uint8Array): DescriptorDelivery {
  if (encoded.byteLength < FRAME_BYTES.descriptorDeliveryHeader || !validPrefix(encoded, 'WS2D')) {
    throw new RelayProtocolError('malformed', 'WS2D has an invalid header or length')
  }
  return Object.freeze({
    relaySessionId: requireBytes(encoded.subarray(8, 16), 8, 'relay session ID', true),
    object: decodeVariable(encoded, 'WS2D', FRAME_BYTES.descriptorDeliveryHeader),
  })
}

export function encodeOpaqueRoute(frame: OpaqueRoute): Uint8Array<ArrayBuffer> {
  if (
    frame.ciphertext.byteLength === 0 ||
    frame.ciphertext.byteLength > RELAY_MAX_OPAQUE_CIPHERTEXT_BYTES
  ) {
    throw new RelayProtocolError('malformed', 'opaque ciphertext has an invalid length')
  }
  return concatBytes([
    reservedPrefix('WS2O'),
    requireBytes(frame.relaySessionId, 8, 'relay session ID', true),
    encodeUint32(frame.ciphertext.byteLength),
    frame.ciphertext,
  ])
}

export function decodeOpaqueRoute(encoded: Uint8Array): OpaqueRoute {
  if (encoded.byteLength < FRAME_BYTES.opaqueHeader || !validPrefix(encoded, 'WS2O')) {
    throw new RelayProtocolError('malformed', 'WS2O has an invalid header or length')
  }
  const length = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength).getUint32(
    16,
    false,
  )
  if (
    length === 0 ||
    length > RELAY_MAX_OPAQUE_CIPHERTEXT_BYTES ||
    FRAME_BYTES.opaqueHeader + length !== encoded.byteLength
  ) {
    throw new RelayProtocolError('malformed', 'WS2O ciphertext length is inconsistent')
  }
  return Object.freeze({
    relaySessionId: requireBytes(encoded.subarray(8, 16), 8, 'relay session ID', true),
    ciphertext: encoded.slice(FRAME_BYTES.opaqueHeader),
  })
}

export function encodeSessionRetired(frame: SessionRetired): Uint8Array<ArrayBuffer> {
  return concatBytes([
    reservedPrefix('WS2F'),
    requireBytes(frame.relaySessionId, RELAY_SESSION_ID_BYTES, 'relay session ID', true),
  ])
}

export function decodeSessionRetired(encoded: Uint8Array): SessionRetired {
  if (encoded.byteLength !== FRAME_BYTES.sessionRetired || !validPrefix(encoded, 'WS2F')) {
    throw new RelayProtocolError('malformed', 'WS2F has an invalid header or length')
  }
  return Object.freeze({
    relaySessionId: requireBytes(
      encoded.subarray(8),
      RELAY_SESSION_ID_BYTES,
      'relay session ID',
      true,
    ),
  })
}

export function encodeSessionCredit(frame: SessionCredit): Uint8Array<ArrayBuffer> {
  if (
    !Number.isInteger(frame.frames) || frame.frames < 0 || frame.frames > RELAY_SENDER_WINDOW_FRAMES ||
    !Number.isInteger(frame.bytes) || frame.bytes < 0 || frame.bytes > RELAY_SENDER_WINDOW_BYTES ||
    (frame.frames === 0 && frame.bytes === 0)
  ) {
    throw new RelayProtocolError('malformed', 'session credit exceeds the sender window')
  }
  return concatBytes([
    reservedPrefix('WS2W'),
    requireBytes(frame.relaySessionId, RELAY_SESSION_ID_BYTES, 'relay session ID', true),
    encodeUint32(frame.frames), encodeUint32(frame.bytes),
  ])
}

export function decodeSessionCredit(encoded: Uint8Array): SessionCredit {
  if (encoded.byteLength !== FRAME_BYTES.sessionCredit || !validPrefix(encoded, 'WS2W')) {
    throw new RelayProtocolError('malformed', 'WS2W has an invalid header or length')
  }
  const view = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength)
  const frame = {
    relaySessionId: requireBytes(encoded.subarray(8, 16), RELAY_SESSION_ID_BYTES, 'relay session ID', true),
    frames: view.getUint32(16, false), bytes: view.getUint32(20, false),
  }
  encodeSessionCredit(frame)
  return Object.freeze(frame)
}

export function encodeSessionAdmitted(frame: SessionAdmitted): Uint8Array<ArrayBuffer> {
  return concatBytes([
    reservedPrefix('WS2M'),
    requireBytes(frame.relaySessionId, RELAY_SESSION_ID_BYTES, 'relay session ID', true),
  ])
}

export function decodeSessionAdmitted(encoded: Uint8Array): SessionAdmitted {
  if (encoded.byteLength !== FRAME_BYTES.sessionAdmitted || !validPrefix(encoded, 'WS2M')) {
    throw new RelayProtocolError('malformed', 'WS2M has an invalid header or length')
  }
  return Object.freeze({
    relaySessionId: requireBytes(encoded.subarray(8), RELAY_SESSION_ID_BYTES, 'relay session ID', true),
  })
}

export const CONNECTION_PROBE_MAGIC = 'WS2H'
export const CONNECTION_PROBE_ACK_MAGIC = 'WS2A'
export const CONNECTION_PROBE_BYTES = 16

export function encodeConnectionProbe(nonce: bigint): Uint8Array<ArrayBuffer> {
  return encodeProbeFrame(CONNECTION_PROBE_MAGIC, nonce)
}

export function encodeConnectionProbeAck(nonce: bigint): Uint8Array<ArrayBuffer> {
  return encodeProbeFrame(CONNECTION_PROBE_ACK_MAGIC, nonce)
}

function encodeProbeFrame(magic: string, nonce: bigint): Uint8Array<ArrayBuffer> {
  if (nonce === 0n) throw new RelayProtocolError('identity', 'connection probe nonce must be nonzero')
  return concatBytes([reservedPrefix(magic), encodeUint64(nonce)])
}

export function decodeConnectionProbe(encoded: Uint8Array): bigint {
  return decodeProbeFrame(encoded, CONNECTION_PROBE_MAGIC)
}

export function decodeConnectionProbeAck(encoded: Uint8Array): bigint {
  return decodeProbeFrame(encoded, CONNECTION_PROBE_ACK_MAGIC)
}

function decodeProbeFrame(encoded: Uint8Array, magic: string): bigint {
  if (encoded.byteLength !== CONNECTION_PROBE_BYTES || !validPrefix(encoded, magic)) {
    throw new RelayProtocolError('malformed', 'connection probe has an invalid header or length')
  }
  const nonce = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength).getBigUint64(8, false)
  if (nonce === 0n) throw new RelayProtocolError('identity', 'connection probe nonce must be nonzero')
  return nonce
}

function validErrorCode(code: number): code is RelayErrorCode {
  return code >= RELAY_ERROR.malformed && code <= RELAY_ERROR.accessUnavailable
}

export function encodeRelayError(frame: RelayErrorFrame): Uint8Array<ArrayBuffer> {
  const retryAllowed =
    frame.code === RELAY_ERROR.starting || frame.code === RELAY_ERROR.admission || frame.code === RELAY_ERROR.accessUnavailable
  if (
    !validErrorCode(frame.code) ||
    !Number.isInteger(frame.retryAfterMilliseconds) ||
    frame.retryAfterMilliseconds < 0 ||
    frame.retryAfterMilliseconds > 30_000 ||
    (!retryAllowed && frame.retryAfterMilliseconds !== 0) ||
    frame.retryAfterMilliseconds > MAX_UINT32
  ) {
    throw new RelayProtocolError('malformed', 'relay error code or retry value is invalid')
  }
  return concatBytes([
    TEXT_ENCODER.encode('WS2E'),
    Uint8Array.of(RELAY_WIRE_VERSION, 0),
    encodeUint16(frame.code),
    encodeUint32(frame.retryAfterMilliseconds),
  ])
}

export function decodeRelayError(encoded: Uint8Array): RelayErrorFrame {
  if (
    encoded.byteLength !== FRAME_BYTES.error ||
    !equalBytes(encoded.subarray(0, 4), TEXT_ENCODER.encode('WS2E')) ||
    encoded[4] !== RELAY_WIRE_VERSION ||
    encoded[5] !== 0
  ) {
    throw new RelayProtocolError('malformed', 'WS2E has an invalid header or length')
  }
  const view = new DataView(encoded.buffer, encoded.byteOffset, encoded.byteLength)
  const code = view.getUint16(6, false)
  const retryAfterMilliseconds = view.getUint32(8, false)
  if (!validErrorCode(code)) {
    throw new RelayProtocolError('malformed', 'relay error code is unknown')
  }
  const frame = { code, retryAfterMilliseconds }
  encodeRelayError(frame)
  return Object.freeze(frame)
}
