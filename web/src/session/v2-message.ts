import { concatBytes, equalBytes } from '../crypto/bytes'
import { verifyEd25519Signature } from '../crypto/curve25519'
import { sha256 } from '../crypto/digest'
import { type CryptoRuntime, defaultCryptoRuntime } from '../crypto/webcrypto'
import { isWellFormedUnicode } from '../protocol/text'
import {
  decodeCanonicalCbor,
  encodeCanonicalCbor,
  requireArray,
  requireBoolean,
  requireBytes,
  requireNumericMap,
  requireText,
  requireUnsigned,
  V2CborError,
} from '../protocol/cbor'

export const V2_MESSAGE_KIND = Object.freeze({
  listChildren: 1,
  catalogResult: 2,
  openRevisions: 3,
  openResults: 4,
  renewLease: 5,
  releaseLease: 6,
  requestBlocks: 7,
  blockFragment: 8,
  cancel: 9,
  operationError: 10,
  sessionTerminal: 11,
  laneAttach: 12,
  scanProgress: 13,
  operationComplete: 14,
  leaseResult: 15,
  peerOffer: 16,
  peerAnswer: 17,
  peerCandidate: 18,
} as const)

export const V2_PEER_OPERATION_CODE = Object.freeze({
  negotiation: 0x5001,
  timeout: 0x5002,
  candidates: 0x5003,
  admission: 0x5004,
} as const)

export type V2MessageKind = (typeof V2_MESSAGE_KIND)[keyof typeof V2_MESSAGE_KIND]

const MAXIMUM_PLAINTEXT_BYTES = 65_536 - 44
export const V2_SENDER_CONTROL_SCHEMA_VERSION = 1
const CONTROL_SCHEMA_KEY = 0
const CONTROL_BODY_KEY = 1
const CONTROL_SIGNATURE_KEY = 255
const FRAGMENT_ROUTING_HEADER_BYTES = 20
const MAXIMUM_REMOTE_ERROR_MESSAGE_BYTES = 512
const MINIMUM_RETRY_DELAY_MILLISECONDS = 1n
const MAXIMUM_RETRY_DELAY_MILLISECONDS = 30_000n
const MAXIMUM_OPEN_BATCH = 64
const MAXIMUM_REVISION_OBJECT_BYTES = 16 << 10
const LEASE_TTL_MILLISECONDS = 120_000n
const LEASE_RENEW_AFTER_MILLISECONDS = 60_000n
const MAXIMUM_SIGNALING_SDP_BYTES = 60 * 1024
const MAXIMUM_SIGNALING_CANDIDATE_BYTES = 4 * 1024
const MAXIMUM_SIGNALING_TEXT_BYTES = 256
const TEXT_ENCODER = new TextEncoder()
const CONTROL_DOMAIN = Object.freeze({
  operation: 'windshare/v2 control/operation',
  terminal: 'windshare/v2 control/session-terminal',
  lane: 'windshare/v2 control/lane-attach',
})

export interface V2SessionMessage {
  readonly kind: V2MessageKind
  readonly operationId?: Uint8Array<ArrayBuffer>
  readonly body: Uint8Array<ArrayBuffer>
  readonly plaintext: Uint8Array<ArrayBuffer>
  readonly data: boolean
}

export interface V2ControlBinding {
  readonly shareInstance: Uint8Array
  readonly protocolSessionId: Uint8Array
  readonly laneId: number
  readonly laneEpoch: number
  readonly direction: 1
  readonly sequence: bigint
}

export class V2MessageError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2MessageError'
  }
}

export function encodeV2Message(
  kind: V2MessageKind,
  operationId: Uint8Array | undefined,
  canonicalBody: Uint8Array,
): V2SessionMessage {
  requireMessageIdentity(kind, operationId)
  const bodyValue = decodeCanonicalCbor(canonicalBody, MAXIMUM_PLAINTEXT_BYTES, 'message body')
  const plaintext = encodeCanonicalCbor(new Map<number, unknown>([
    [0, kind],
    [1, operationId === undefined ? null : operationId.slice()],
    [2, bodyValue],
  ]))
  if (plaintext.byteLength > MAXIMUM_PLAINTEXT_BYTES) {
    throw new V2MessageError('Session message exceeds its envelope plaintext limit')
  }
  return Object.freeze({
    kind,
    ...(operationId === undefined ? {} : { operationId: operationId.slice() }),
    body: canonicalBody.slice(),
    plaintext,
    data: false,
  })
}

export function decodeV2Message(plaintext: Uint8Array): V2SessionMessage {
  if (plaintext.byteLength === 0 || plaintext.byteLength > MAXIMUM_PLAINTEXT_BYTES) {
    throw new V2MessageError('Session message plaintext has an invalid length')
  }
  if (plaintext[0] === 1 && plaintext[1] === V2_MESSAGE_KIND.blockFragment) {
    return decodeFragmentMessage(plaintext)
  }
  const fields = requireNumericMap(
    decodeCanonicalCbor(plaintext, MAXIMUM_PLAINTEXT_BYTES, 'session message'),
    [0, 1, 2],
    'session message',
  )
  const kindValue = requireUnsigned(fields.get(0), 'session message kind')
  if (kindValue < 1n || kindValue > 18n || kindValue === 8n) {
    throw new V2MessageError('Session message kind is unknown or uses the wrong codec')
  }
  const kind = Number(kindValue) as V2MessageKind
  const rawOperation = fields.get(1)
  const operationId = rawOperation === null
    ? undefined
    : requireBytes(rawOperation, 16, 'operation ID', true)
  requireMessageIdentity(kind, operationId)
  const body = encodeCanonicalCbor(fields.get(2))
  return Object.freeze({
    kind,
    ...(operationId === undefined ? {} : { operationId }),
    body,
    plaintext: plaintext.slice(),
    data: false,
  })
}

export async function verifyV2SenderControl(
  message: V2SessionMessage,
  binding: V2ControlBinding,
  senderPublicKey: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  if (message.data) return message.body.slice()
  const domain = controlDomain(message.kind)
  const signed = requireNumericMap(
    decodeCanonicalCbor(message.body, MAXIMUM_PLAINTEXT_BYTES, 'sender control wrapper'),
    [CONTROL_SCHEMA_KEY, CONTROL_BODY_KEY, CONTROL_SIGNATURE_KEY],
    'sender control wrapper',
  )
  if (
    requireUnsigned(signed.get(CONTROL_SCHEMA_KEY), 'sender control wrapper schema') !==
      BigInt(V2_SENDER_CONTROL_SCHEMA_VERSION)
  ) {
    throw new V2MessageError('Sender control wrapper schema is unsupported')
  }
  const signature = requireBytes(
    signed.get(CONTROL_SIGNATURE_KEY),
    64,
    'sender control signature',
  )
  const semanticValue = signed.get(CONTROL_BODY_KEY)
  const semanticBody = encodeCanonicalCbor(semanticValue)
  const unsignedWrapper = encodeCanonicalCbor(new Map<number, unknown>([
    [CONTROL_SCHEMA_KEY, V2_SENDER_CONTROL_SCHEMA_VERSION],
    [CONTROL_BODY_KEY, semanticValue],
  ]))
  const preimage = await controlPreimage(message, binding, domain, unsignedWrapper, runtime)
  if (!(await verifyEd25519(senderPublicKey, preimage, signature, runtime))) {
    throw new V2MessageError('Sender control signature is invalid')
  }
  validateV2SenderControlBody(message.kind, semanticBody)
  return semanticBody
}

export interface V2ScanProgress {
  readonly attemptId: Uint8Array<ArrayBuffer>
  readonly discoveredEntries: bigint
}

export interface V2OperationErrorControl {
  readonly scope: 'directory' | 'revision' | 'block' | 'peer'
  readonly code: number
  readonly retryable: boolean
  readonly retryAfterMilliseconds: number | undefined
  readonly message: string
}

export function decodeV2OperationErrorControl(body: Uint8Array): V2OperationErrorControl {
  const fields = requireNumericMap(
    decodeCanonicalCbor(body, MAXIMUM_PLAINTEXT_BYTES, 'operation error'),
    [0, 1, 2, 3, 4, 5],
    'operation error',
  )
  requireSchema(fields.get(0), 'operation error')
  const scope = operationErrorScope(requireUnsigned(fields.get(1), 'operation error scope'))
  const codeValue = requireUnsigned(fields.get(2), 'operation error code')
  const [lower, upper] = operationErrorCodeRange(scope)
  if (codeValue < lower || codeValue > upper) {
    throw new V2MessageError('Operation error scope and code are inconsistent')
  }
  const retryable = requireBoolean(fields.get(3), 'operation error retryable')
  if (scope === 'peer' && retryable) {
    throw new V2MessageError('Peer operation errors are permanent')
  }
  const retryAfterMilliseconds = operationRetryDelay(retryable, fields.get(4))
  const message = requireBoundedMessage(fields.get(5), 'operation error message')
  return Object.freeze({ scope, code: Number(codeValue), retryable, retryAfterMilliseconds, message })
}

function operationErrorScope(value: bigint): V2OperationErrorControl['scope'] {
  switch (value) {
    case 2n:
      return 'directory'
    case 3n:
      return 'revision'
    case 4n:
      return 'block'
    case 5n:
      return 'peer'
    default:
      throw new V2MessageError('Operation error scope is outside its registry')
  }
}

function operationErrorCodeRange(
  scope: V2OperationErrorControl['scope'],
): readonly [bigint, bigint] {
  switch (scope) {
    case 'directory':
      return [0x2001n, 0x2008n]
    case 'revision':
      return [0x3001n, 0x3008n]
    case 'block':
      return [0x4001n, 0x4006n]
    case 'peer':
      return [BigInt(V2_PEER_OPERATION_CODE.negotiation), BigInt(V2_PEER_OPERATION_CODE.admission)]
  }
}

function operationRetryDelay(retryable: boolean, value: unknown): number | undefined {
  if (!retryable) {
    if (value !== null) throw new V2MessageError('Permanent operation error carries a retry delay')
    return undefined
  }
  const delay = requireUnsigned(value, 'operation error retry delay')
  if (
    delay < MINIMUM_RETRY_DELAY_MILLISECONDS ||
    delay > MAXIMUM_RETRY_DELAY_MILLISECONDS
  ) {
    throw new V2MessageError('Operation error retry delay is outside its limits')
  }
  return Number(delay)
}

export function decodeV2ScanProgress(body: Uint8Array): V2ScanProgress {
  const fields = requireNumericMap(
    decodeCanonicalCbor(body, MAXIMUM_PLAINTEXT_BYTES, 'scan progress'),
    [0, 1, 2],
    'scan progress',
  )
  requireSchema(fields.get(0), 'scan progress')
  const discoveredEntries = requireUnsigned(fields.get(2), 'scan progress discovered entries')
  if (discoveredEntries > 0xffff_ffff_ffff_ffffn) {
    throw new V2MessageError('Scan progress exceeds its unsigned 64-bit domain')
  }
  return Object.freeze({
    attemptId: requireBytes(fields.get(1), 16, 'scan progress attempt ID', true),
    discoveredEntries,
  })
}

function decodeFragmentMessage(plaintext: Uint8Array): V2SessionMessage {
  if (
    plaintext.byteLength < FRAGMENT_ROUTING_HEADER_BYTES ||
    (plaintext[2] ?? 0) > 1 ||
    plaintext[3] !== 0
  ) {
    throw new V2MessageError('Block fragment routing header is invalid')
  }
  const operationId = requireBytes(
    plaintext.subarray(4, FRAGMENT_ROUTING_HEADER_BYTES),
    16,
    'fragment operation ID',
    true,
  )
  return Object.freeze({
    kind: V2_MESSAGE_KIND.blockFragment,
    operationId,
    body: plaintext.slice(),
    plaintext: plaintext.slice(),
    data: true,
  })
}

async function controlPreimage(
  message: V2SessionMessage,
  binding: V2ControlBinding,
  domain: string,
  unsignedWrapper: Uint8Array,
  runtime: CryptoRuntime,
): Promise<Uint8Array<ArrayBuffer>> {
  const share = requireBytes(binding.shareInstance, 16, 'control share instance', true)
  const session = requireBytes(binding.protocolSessionId, 16, 'control protocol session', true)
  if (
    !Number.isInteger(binding.laneId) || binding.laneId <= 0 || binding.laneId > 0xffff_ffff ||
    !Number.isInteger(binding.laneEpoch) || binding.laneEpoch < 0 || binding.laneEpoch > 0xffff_ffff ||
    binding.direction !== 1 ||
    binding.sequence < 0n || binding.sequence > 0xffff_ffff_ffff_ffffn
  ) {
    throw new V2MessageError('Sender control delivery identity is invalid')
  }
  const lane = new Uint8Array(8)
  const laneView = new DataView(lane.buffer)
  laneView.setUint32(0, binding.laneId, false)
  laneView.setUint32(4, binding.laneEpoch, false)
  const sequence = new Uint8Array(8)
  new DataView(sequence.buffer).setBigUint64(0, binding.sequence, false)
  return concatBytes([
    TEXT_ENCODER.encode(domain),
    Uint8Array.of(0),
    share,
    session,
    lane,
    Uint8Array.of(binding.direction),
    sequence,
    Uint8Array.of(message.kind),
    message.operationId ?? new Uint8Array(16),
    await sha256(unsignedWrapper, runtime),
  ])
}

function controlDomain(kind: V2MessageKind): string {
  switch (kind) {
    case V2_MESSAGE_KIND.sessionTerminal:
      return CONTROL_DOMAIN.terminal
    case V2_MESSAGE_KIND.laneAttach:
      return CONTROL_DOMAIN.lane
    case V2_MESSAGE_KIND.catalogResult:
    case V2_MESSAGE_KIND.openResults:
    case V2_MESSAGE_KIND.operationError:
    case V2_MESSAGE_KIND.scanProgress:
    case V2_MESSAGE_KIND.operationComplete:
    case V2_MESSAGE_KIND.leaseResult:
    case V2_MESSAGE_KIND.peerAnswer:
    case V2_MESSAGE_KIND.peerCandidate:
      return CONTROL_DOMAIN.operation
    default:
      throw new V2MessageError('Inbound sender message is not a signed control or data record')
  }
}

export function validateV2SenderControlBody(kind: V2MessageKind, body: Uint8Array): void {
  switch (kind) {
    case V2_MESSAGE_KIND.catalogResult:
      validateCatalogResult(body)
      return
    case V2_MESSAGE_KIND.openResults:
      validateOpenResults(body)
      return
    case V2_MESSAGE_KIND.operationError:
      decodeV2OperationErrorControl(body)
      return
    case V2_MESSAGE_KIND.sessionTerminal:
      validateSessionTerminal(body)
      return
    case V2_MESSAGE_KIND.laneAttach:
      validateLaneGrant(body)
      return
    case V2_MESSAGE_KIND.scanProgress:
      decodeV2ScanProgress(body)
      return
    case V2_MESSAGE_KIND.operationComplete:
      validateOperationComplete(body)
      return
    case V2_MESSAGE_KIND.leaseResult:
      validateLeaseResult(body)
      return
    case V2_MESSAGE_KIND.peerAnswer:
      validatePeerAnswer(body)
      return
    case V2_MESSAGE_KIND.peerCandidate:
      validatePeerCandidate(body)
      return
    default:
      throw new V2MessageError('Inbound sender control kind has no closed validator')
  }
}

function validateCatalogResult(body: Uint8Array): void {
  const fields = requireNumericMap(
    decodeCanonicalCbor(body, MAXIMUM_PLAINTEXT_BYTES, 'catalog result'),
    [0, 1],
    'catalog result',
  )
  requireSchema(fields.get(0), 'catalog result')
  requireBytes(fields.get(1), undefined, 'catalog sender object', true)
}

function validateOpenResults(body: Uint8Array): void {
  const fields = requireNumericMap(
    decodeCanonicalCbor(body, MAXIMUM_PLAINTEXT_BYTES, 'open results'),
    [0, 1],
    'open results',
  )
  requireSchema(fields.get(0), 'open results')
  const results = requireArray(fields.get(1), MAXIMUM_OPEN_BATCH, 'open result items')
  if (results.length === 0) throw new V2MessageError('Open results must not be empty')
  for (const value of results) validateOpenResult(value)
}

function validateOpenResult(value: unknown): void {
  const item = requireArray(value, 6, 'open result')
  requireBytes(item[0], 16, 'open result file ID', true)
  const status = requireUnsigned(item[1], 'open result status')
  if (status === 0n && item.length === 6) {
    const revisionObject = requireBytes(item[2], undefined, 'revision sender object', true)
    if (revisionObject.byteLength > MAXIMUM_REVISION_OBJECT_BYTES) {
      throw new V2MessageError('Revision sender object exceeds its size limit')
    }
    requireBytes(item[3], 16, 'revision lease ID', true)
    validateLeaseTiming(item[4], item[5], 'open result')
    return
  }
  if (status !== 1n || item.length !== 5) {
    throw new V2MessageError('Open result has an unknown outcome shape')
  }
  const code = requireUnsigned(item[2], 'revision failure code')
  if (code < 0x3001n || code > 0x3008n) {
    throw new V2MessageError('Revision failure code is outside its scope')
  }
  const retryable = requireBoolean(item[3], 'revision failure retryable')
  const retry = item[4]
  if (retryable) {
    const delay = requireUnsigned(retry, 'revision retry delay')
    if (delay < MINIMUM_RETRY_DELAY_MILLISECONDS || delay > MAXIMUM_RETRY_DELAY_MILLISECONDS) {
      throw new V2MessageError('Revision retry delay is invalid')
    }
  } else if (retry !== null) {
    throw new V2MessageError('Permanent revision failure carries a retry delay')
  }
}

function validateLaneGrant(body: Uint8Array): void {
  const fields = requireNumericMap(
    decodeCanonicalCbor(body, 1_024, 'lane grant'),
    [0, 1, 2, 3, 4],
    'lane grant',
  )
  requireSchema(fields.get(0), 'lane grant')
  if (requireUnsigned(fields.get(1), 'lane grant disposition') !== 1n) {
    throw new V2MessageError('Lane grant disposition is invalid')
  }
  requireUint32(fields.get(2), 'lane grant ID', true)
  requireUint32(fields.get(3), 'lane grant epoch', true)
  requireBytes(fields.get(4), 16, 'lane grant attach nonce', true)
}

function validateOperationComplete(body: Uint8Array): void {
  const fields = requireNumericMap(
    decodeCanonicalCbor(body, MAXIMUM_PLAINTEXT_BYTES, 'operation complete'),
    [0, 1],
    'operation complete',
  )
  requireSchema(fields.get(0), 'operation complete')
  requireUint32(fields.get(1), 'operation result count')
}

function validateLeaseResult(body: Uint8Array): void {
  const fields = requireNumericMap(
    decodeCanonicalCbor(body, MAXIMUM_PLAINTEXT_BYTES, 'lease result'),
    [0, 1, 2, 3],
    'lease result',
  )
  requireSchema(fields.get(0), 'lease result')
  requireBytes(fields.get(1), 16, 'lease result ID', true)
  validateLeaseTiming(fields.get(2), fields.get(3), 'lease result')
}

function validateLeaseTiming(ttlValue: unknown, renewValue: unknown, label: string): void {
  if (
    requireUnsigned(ttlValue, `${label} TTL`) !== LEASE_TTL_MILLISECONDS ||
    requireUnsigned(renewValue, `${label} renew delay`) !== LEASE_RENEW_AFTER_MILLISECONDS
  ) {
    throw new V2MessageError(`${label} lease timing is invalid`)
  }
}

function validatePeerAnswer(body: Uint8Array): void {
  const fields = requireExactArray(body, 4, 'peer answer')
  validatePeerBinding(fields, 'peer answer')
  requireNormalizedText(fields[3], MAXIMUM_SIGNALING_SDP_BYTES, false, 'peer answer SDP')
}

function validatePeerCandidate(body: Uint8Array): void {
  const fields = requireExactArray(body, 7, 'peer candidate')
  validatePeerBinding(fields, 'peer candidate')
  requireNormalizedText(fields[3], MAXIMUM_SIGNALING_CANDIDATE_BYTES, false, 'ICE candidate')
  requireOptionalNormalizedText(fields[4], MAXIMUM_SIGNALING_TEXT_BYTES, 'SDP mid')
  if (fields[5] !== null) requireUint16(fields[5], 'SDP m-line index')
  requireOptionalNormalizedText(fields[6], MAXIMUM_SIGNALING_TEXT_BYTES, 'ICE username fragment')
}

function requireExactArray(body: Uint8Array, length: number, label: string): readonly unknown[] {
  const fields = requireArray(decodeCanonicalCbor(body, MAXIMUM_PLAINTEXT_BYTES, label), length, label)
  if (fields.length !== length) throw new V2MessageError(`${label} has the wrong field count`)
  return fields
}

function validatePeerBinding(fields: readonly unknown[], label: string): void {
  requireSchema(fields[0], label)
  requireBytes(fields[1], 16, `${label} peer path ID`, true)
  requireBytes(fields[2], 16, `${label} attempt ID`, true)
}

function requireOptionalNormalizedText(value: unknown, maximumBytes: number, label: string): void {
  if (value !== null) requireNormalizedText(value, maximumBytes, true, label)
}

function requireNormalizedText(
  value: unknown,
  maximumBytes: number,
  allowEmpty: boolean,
  label: string,
): string {
  const text = requireText(value, label)
  if (
    !isWellFormedUnicode(text) ||
    text.normalize('NFC') !== text ||
    (!allowEmpty && text.length === 0) ||
    TEXT_ENCODER.encode(text).byteLength > maximumBytes
  ) {
    throw new V2MessageError(`${label} is empty, non-NFC, or exceeds its UTF-8 limit`)
  }
  return text
}

function requireUint32(value: unknown, label: string, nonzero = false): number {
  const decoded = requireUnsigned(value, label)
  if (decoded > 0xffff_ffffn || (nonzero && decoded === 0n)) {
    throw new V2MessageError(`${label} is outside its unsigned 32-bit domain`)
  }
  return Number(decoded)
}

function requireUint16(value: unknown, label: string): number {
  const decoded = requireUnsigned(value, label)
  if (decoded > 0xffffn) throw new V2MessageError(`${label} is outside its unsigned 16-bit domain`)
  return Number(decoded)
}

function validateSessionTerminal(body: Uint8Array): void {
  const fields = requireNumericMap(
    decodeCanonicalCbor(body, MAXIMUM_PLAINTEXT_BYTES, 'session terminal'),
    [0, 1, 2],
    'session terminal',
  )
  requireSchema(fields.get(0), 'session terminal')
  const code = requireUnsigned(fields.get(1), 'session terminal code')
  if (code < 0x1001n || code > 0x1008n) {
    throw new V2MessageError('Session terminal code is outside its registry')
  }
  requireBoundedMessage(fields.get(2), 'session terminal message')
}

function requireBoundedMessage(value: unknown, label: string): string {
  const message = requireText(value, label)
  if (
    message.length === 0 ||
    !isWellFormedUnicode(message) ||
    message.normalize('NFC') !== message ||
    TEXT_ENCODER.encode(message).byteLength > MAXIMUM_REMOTE_ERROR_MESSAGE_BYTES
  ) {
    throw new V2MessageError(`${label} is empty, non-NFC, malformed, or exceeds its UTF-8 limit`)
  }
  return message
}

function requireSchema(value: unknown, label: string): void {
  if (requireUnsigned(value, `${label} schema`) !== 1n) {
    throw new V2MessageError(`${label} schema is unsupported`)
  }
}

function requireMessageIdentity(kind: V2MessageKind, operationId: Uint8Array | undefined): void {
  if (kind < 1 || kind > 18) throw new V2MessageError('Message kind is outside the wire registry')
  if (kind === V2_MESSAGE_KIND.sessionTerminal) {
    if (operationId !== undefined) throw new V2MessageError('Session terminal has an operation ID')
    return
  }
  if (operationId === undefined) throw new V2MessageError('Operation message has no operation ID')
  requireBytes(operationId, 16, 'operation ID', true)
}

async function verifyEd25519(
  publicKey: Uint8Array,
  preimage: Uint8Array,
  signature: Uint8Array,
  runtime: CryptoRuntime,
): Promise<boolean> {
  try {
    return await verifyEd25519Signature(publicKey, preimage, signature, runtime)
  } catch (cause) {
    throw new V2MessageError('Unable to verify sender control signature', { cause })
  }
}

export function sameOperationId(left: Uint8Array | undefined, right: Uint8Array): boolean {
  return left !== undefined && equalBytes(left, right)
}

export function encodeV2Body(value: unknown): Uint8Array<ArrayBuffer> {
  try {
    return encodeCanonicalCbor(value)
  } catch (cause) {
    if (cause instanceof V2CborError) throw cause
    throw new V2MessageError('Unable to encode operation body', { cause })
  }
}
