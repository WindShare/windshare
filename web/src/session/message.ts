import { decodePeerPathControl } from '../protocol/peer-path-control'
import { requirePeerSlot } from './peer-slots/slots'
import { equalBytes } from '../crypto/bytes'
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
  CborError,
} from '../protocol/cbor'

export const MESSAGE_KIND = Object.freeze({
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
  peerPathControl: 19,
} as const)

export const PEER_OPERATION_CODE = Object.freeze({
  negotiation: 0x5001,
  timeout: 0x5002,
  candidates: 0x5003,
  admission: 0x5004,
  ice: 0x5005,
  stun: 0x5006,
  transport: 0x5007,
  dtls: 0x5008,
  policy: 0x5009,
  authentication: 0x500a,
  sessionInvariant: 0x500b,
  capacity: 0x500c,
} as const)

export type MessageKind = (typeof MESSAGE_KIND)[keyof typeof MESSAGE_KIND]

// Encoding and decoding must recognize the same registry when protocol controls grow.
const MESSAGE_KINDS: ReadonlySet<number> = new Set(Object.values(MESSAGE_KIND))

const MAXIMUM_PLAINTEXT_BYTES = 65_536 - 44
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
export interface SessionMessage {
  readonly kind: MessageKind
  readonly operationId?: Uint8Array<ArrayBuffer>
  readonly body: Uint8Array<ArrayBuffer>
  readonly plaintext: Uint8Array<ArrayBuffer>
  readonly data: boolean
}

export class MessageError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'MessageError'
  }
}

export function encodeMessage(
  kind: MessageKind,
  operationId: Uint8Array | undefined,
  canonicalBody: Uint8Array,
): SessionMessage {
  requireMessageIdentity(kind, operationId)
  const bodyValue = decodeCanonicalCbor(canonicalBody, MAXIMUM_PLAINTEXT_BYTES, 'message body')
  const plaintext = encodeCanonicalCbor(new Map<number, unknown>([
    [0, kind],
    [1, operationId === undefined ? null : operationId.slice()],
    [2, bodyValue],
  ]))
  if (plaintext.byteLength > MAXIMUM_PLAINTEXT_BYTES) {
    throw new MessageError('Session message exceeds its envelope plaintext limit')
  }
  return Object.freeze({
    kind,
    ...(operationId === undefined ? {} : { operationId: operationId.slice() }),
    body: canonicalBody.slice(),
    plaintext,
    data: false,
  })
}

export function decodeMessage(plaintext: Uint8Array): SessionMessage {
  if (plaintext.byteLength === 0 || plaintext.byteLength > MAXIMUM_PLAINTEXT_BYTES) {
    throw new MessageError('Session message plaintext has an invalid length')
  }
  if (plaintext[0] === 1 && plaintext[1] === MESSAGE_KIND.blockFragment) {
    return decodeFragmentMessage(plaintext)
  }
  const fields = requireNumericMap(
    decodeCanonicalCbor(plaintext, MAXIMUM_PLAINTEXT_BYTES, 'session message'),
    [0, 1, 2],
    'session message',
  )
  const kindValue = Number(requireUnsigned(fields.get(0), 'session message kind'))
  if (!MESSAGE_KINDS.has(kindValue) || kindValue === MESSAGE_KIND.blockFragment) {
    throw new MessageError('Session message kind is unknown or uses the wrong codec')
  }
  const kind = kindValue as MessageKind
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

export interface ScanProgress {
  readonly attemptId: Uint8Array<ArrayBuffer>
  readonly discoveredEntries: bigint
}

export interface PeerAttemptBinding {
  readonly peerSlot: number
  readonly peerPathId: Uint8Array<ArrayBuffer>
  readonly attemptId: Uint8Array<ArrayBuffer>
  readonly attemptSequence: bigint
}

export interface OperationErrorControl {
  readonly peerAttempt: PeerAttemptBinding | undefined
  readonly scope: 'directory' | 'revision' | 'block' | 'peer'
  readonly code: number
  readonly retryable: boolean
  readonly retryAfterMilliseconds: number | undefined
  readonly message: string
}

export function decodeOperationErrorControl(body: Uint8Array): OperationErrorControl {
  const decoded = decodeCanonicalCbor(body, MAXIMUM_PLAINTEXT_BYTES, 'operation error')
  const peer = decoded instanceof Map && requireUnsigned(decoded.get(1), 'operation error scope') === 5n
  const fields = requireNumericMap(decoded, peer ? [0, 1, 2, 3, 4, 5, 6] : [0, 1, 2, 3, 4, 5], 'operation error')
  if (peer) {
    if (requireUnsigned(fields.get(0), 'peer operation error schema') !== 3n) throw new MessageError('Invalid peer operation error schema')
  } else {
    requireSchema(fields.get(0), 'operation error')
  }
  const scope = operationErrorScope(requireUnsigned(fields.get(1), 'operation error scope'))
  const codeValue = requireUnsigned(fields.get(2), 'operation error code')
  const [lower, upper] = operationErrorCodeRange(scope)
  if (codeValue < lower || codeValue > upper) {
    throw new MessageError('Operation error scope and code are inconsistent')
  }
  const retryable = requireBoolean(fields.get(3), 'operation error retryable')
  if (scope === 'peer' && retryable) {
    throw new MessageError('Peer operation errors are permanent')
  }
  const retryAfterMilliseconds = operationRetryDelay(retryable, fields.get(4))
  const message = requireBoundedMessage(fields.get(5), 'operation error message')
  const peerAttempt = peer ? decodeErrorPeerAttempt(fields.get(6)) : undefined
  return Object.freeze({ scope, code: Number(codeValue), retryable, retryAfterMilliseconds, message, peerAttempt })
}

function decodeErrorPeerAttempt(value: unknown): PeerAttemptBinding {
  const fields = requireArray(value, 4, 'peer operation error binding')
  if (fields.length !== 4) throw new MessageError('Invalid peer operation error binding')
  const attemptSequence = requireUnsigned(fields[2], 'peer attempt sequence')
  if (attemptSequence === 0n || attemptSequence > 0xffffffffffffffffn) throw new MessageError('Invalid peer attempt sequence')
  return Object.freeze({
    peerPathId: requireBytes(fields[0], 16, 'peer path ID', true),
    attemptId: requireBytes(fields[1], 16, 'peer attempt ID', true),
    attemptSequence,
    peerSlot: requirePeerSlot(requireUnsigned(fields[3], 'peer slot')),
  })
}

function operationErrorScope(value: bigint): OperationErrorControl['scope'] {
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
      throw new MessageError('Operation error scope is outside its registry')
  }
}

function operationErrorCodeRange(
  scope: OperationErrorControl['scope'],
): readonly [bigint, bigint] {
  switch (scope) {
    case 'directory':
      return [0x2001n, 0x2008n]
    case 'revision':
      return [0x3001n, 0x3008n]
    case 'block':
      return [0x4001n, 0x4006n]
    case 'peer':
      return [0x5000n, 0x5fffn]
  }
}

function operationRetryDelay(retryable: boolean, value: unknown): number | undefined {
  if (!retryable) {
    if (value !== null) throw new MessageError('Permanent operation error carries a retry delay')
    return undefined
  }
  const delay = requireUnsigned(value, 'operation error retry delay')
  if (
    delay < MINIMUM_RETRY_DELAY_MILLISECONDS ||
    delay > MAXIMUM_RETRY_DELAY_MILLISECONDS
  ) {
    throw new MessageError('Operation error retry delay is outside its limits')
  }
  return Number(delay)
}

export function decodeScanProgress(body: Uint8Array): ScanProgress {
  const fields = requireNumericMap(
    decodeCanonicalCbor(body, MAXIMUM_PLAINTEXT_BYTES, 'scan progress'),
    [0, 1, 2],
    'scan progress',
  )
  requireSchema(fields.get(0), 'scan progress')
  const discoveredEntries = requireUnsigned(fields.get(2), 'scan progress discovered entries')
  if (discoveredEntries > 0xffff_ffff_ffff_ffffn) {
    throw new MessageError('Scan progress exceeds its unsigned 64-bit domain')
  }
  return Object.freeze({
    attemptId: requireBytes(fields.get(1), 16, 'scan progress attempt ID', true),
    discoveredEntries,
  })
}

function decodeFragmentMessage(plaintext: Uint8Array): SessionMessage {
  if (
    plaintext.byteLength < FRAGMENT_ROUTING_HEADER_BYTES ||
    (plaintext[2] ?? 0) > 1 ||
    plaintext[3] !== 0
  ) {
    throw new MessageError('Block fragment routing header is invalid')
  }
  const operationId = requireBytes(
    plaintext.subarray(4, FRAGMENT_ROUTING_HEADER_BYTES),
    16,
    'fragment operation ID',
    true,
  )
  return Object.freeze({
    kind: MESSAGE_KIND.blockFragment,
    operationId,
    body: plaintext.slice(),
    plaintext: plaintext.slice(),
    data: true,
  })
}

export function validateSenderControlBody(kind: MessageKind, body: Uint8Array): void {
  switch (kind) {
    case MESSAGE_KIND.catalogResult:
      validateCatalogResult(body)
      return
    case MESSAGE_KIND.openResults:
      validateOpenResults(body)
      return
    case MESSAGE_KIND.operationError:
      decodeOperationErrorControl(body)
      return
    case MESSAGE_KIND.peerPathControl:
      decodePeerPathControl(body)
      return
    case MESSAGE_KIND.sessionTerminal:
      validateSessionTerminal(body)
      return
    case MESSAGE_KIND.laneAttach:
      validateLaneGrant(body)
      return
    case MESSAGE_KIND.scanProgress:
      decodeScanProgress(body)
      return
    case MESSAGE_KIND.operationComplete:
      validateOperationComplete(body)
      return
    case MESSAGE_KIND.leaseResult:
      validateLeaseResult(body)
      return
    case MESSAGE_KIND.peerAnswer:
      validatePeerAnswer(body)
      return
    case MESSAGE_KIND.peerCandidate:
      validatePeerCandidate(body)
      return
    default:
      throw new MessageError('Inbound sender control kind has no closed validator')
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
  if (results.length === 0) throw new MessageError('Open results must not be empty')
  for (const value of results) validateOpenResult(value)
}

function validateOpenResult(value: unknown): void {
  const item = requireArray(value, 6, 'open result')
  requireBytes(item[0], 16, 'open result file ID', true)
  const status = requireUnsigned(item[1], 'open result status')
  if (status === 0n && item.length === 6) {
    const revisionObject = requireBytes(item[2], undefined, 'revision sender object', true)
    if (revisionObject.byteLength > MAXIMUM_REVISION_OBJECT_BYTES) {
      throw new MessageError('Revision sender object exceeds its size limit')
    }
    requireBytes(item[3], 16, 'revision lease ID', true)
    validateLeaseTiming(item[4], item[5], 'open result')
    return
  }
  if (status !== 1n || item.length !== 5) {
    throw new MessageError('Open result has an unknown outcome shape')
  }
  const code = requireUnsigned(item[2], 'revision failure code')
  if (code < 0x3001n || code > 0x3008n) {
    throw new MessageError('Revision failure code is outside its scope')
  }
  const retryable = requireBoolean(item[3], 'revision failure retryable')
  const retry = item[4]
  if (retryable) {
    const delay = requireUnsigned(retry, 'revision retry delay')
    if (delay < MINIMUM_RETRY_DELAY_MILLISECONDS || delay > MAXIMUM_RETRY_DELAY_MILLISECONDS) {
      throw new MessageError('Revision retry delay is invalid')
    }
  } else if (retry !== null) {
    throw new MessageError('Permanent revision failure carries a retry delay')
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
    throw new MessageError('Lane grant disposition is invalid')
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
    throw new MessageError(`${label} lease timing is invalid`)
  }
}

function validatePeerAnswer(body: Uint8Array): void {
  const fields = requireExactArray(body, 7, 'peer answer')
  validatePeerBinding(fields, 'peer answer')
  requireNormalizedText(fields[6], MAXIMUM_SIGNALING_SDP_BYTES, false, 'peer answer SDP')
}

function validatePeerCandidate(body: Uint8Array): void {
  const fields = requireExactArray(body, 10, 'peer candidate')
  validatePeerBinding(fields, 'peer candidate')
  requireNormalizedText(fields[6], MAXIMUM_SIGNALING_CANDIDATE_BYTES, false, 'ICE candidate')
  requireOptionalNormalizedText(fields[7], MAXIMUM_SIGNALING_TEXT_BYTES, 'SDP mid')
  if (fields[8] !== null) requireUint16(fields[8], 'SDP m-line index')
  requireOptionalNormalizedText(fields[9], MAXIMUM_SIGNALING_TEXT_BYTES, 'ICE username fragment')
}

function requireExactArray(body: Uint8Array, length: number, label: string): readonly unknown[] {
  const fields = requireArray(decodeCanonicalCbor(body, MAXIMUM_PLAINTEXT_BYTES, label), length, label)
  if (fields.length !== length) throw new MessageError(`${label} has the wrong field count`)
  return fields
}

function validatePeerBinding(fields: readonly unknown[], label: string): void {
  if (requireUnsigned(fields[0], label) !== 4n) {
    throw new MessageError('invalid peer schema')
  }
  const sequence = requireUnsigned(fields[3], 'attempt sequence')
  if (sequence === 0n || sequence > 0xffffffffffffffffn) {
    throw new MessageError('invalid attempt sequence')
  }
  requireBytes(fields[1], 16, `${label} peer path ID`, true)
  requireBytes(fields[2], 16, `${label} attempt ID`, true)
  requirePeerSlot(requireUnsigned(fields[4], 'peer slot'))
  if (requireUnsigned(fields[5], 'connection purpose') > 2n) throw new MessageError('invalid connection purpose')
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
    throw new MessageError(`${label} is empty, non-NFC, or exceeds its UTF-8 limit`)
  }
  return text
}

function requireUint32(value: unknown, label: string, nonzero = false): number {
  const decoded = requireUnsigned(value, label)
  if (decoded > 0xffff_ffffn || (nonzero && decoded === 0n)) {
    throw new MessageError(`${label} is outside its unsigned 32-bit domain`)
  }
  return Number(decoded)
}

function requireUint16(value: unknown, label: string): number {
  const decoded = requireUnsigned(value, label)
  if (decoded > 0xffffn) throw new MessageError(`${label} is outside its unsigned 16-bit domain`)
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
    throw new MessageError('Session terminal code is outside its registry')
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
    throw new MessageError(`${label} is empty, non-NFC, malformed, or exceeds its UTF-8 limit`)
  }
  return message
}

function requireSchema(value: unknown, label: string): void {
  if (requireUnsigned(value, `${label} schema`) !== 1n) {
    throw new MessageError(`${label} schema is unsupported`)
  }
}

function requireMessageIdentity(kind: MessageKind, operationId: Uint8Array | undefined): void {
  if (!MESSAGE_KINDS.has(kind)) throw new MessageError('Message kind is outside the wire registry')
  if (kind === MESSAGE_KIND.sessionTerminal || kind === MESSAGE_KIND.peerPathControl) {
    if (operationId !== undefined) throw new MessageError('Session control has an operation ID')
    return
  }
  if (operationId === undefined) throw new MessageError('Operation message has no operation ID')
  requireBytes(operationId, 16, 'operation ID', true)
}

export function sameOperationId(left: Uint8Array | undefined, right: Uint8Array): boolean {
  return left !== undefined && equalBytes(left, right)
}

export function encodeBody(value: unknown): Uint8Array<ArrayBuffer> {
  try {
    return encodeCanonicalCbor(value)
  } catch (cause) {
    if (cause instanceof CborError) throw cause
    throw new MessageError('Unable to encode operation body', { cause })
  }
}

export type PeerFailureRecoveryScope = 'attempt-transient' | 'resource-deferred' | 'path-terminal' | 'session-terminal'

// Only closed typed reasons grant recovery or session authority.
export function peerFailureScope(code: number): PeerFailureRecoveryScope {
  switch (code) {
    case PEER_OPERATION_CODE.capacity:
      return 'resource-deferred'
    case PEER_OPERATION_CODE.negotiation:
    case PEER_OPERATION_CODE.timeout:
    case PEER_OPERATION_CODE.ice:
    case PEER_OPERATION_CODE.stun:
    case PEER_OPERATION_CODE.transport:
    case PEER_OPERATION_CODE.dtls:
      return 'attempt-transient'
    case PEER_OPERATION_CODE.authentication:
    case PEER_OPERATION_CODE.sessionInvariant:
      return 'session-terminal'
    default:
      return 'path-terminal'
  }
}
