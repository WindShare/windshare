import { MAX_FRAME_BYTES, MIN_FRAME_BYTES } from '../../contracts/channel'

export const RELAY_PROTOCOL_VERSION = 'v1'
export const MAX_SIGNALING_MESSAGE_BYTES = 64 * 1024
export const MAX_SIGNALING_JSON_DEPTH = 64
export const MAX_SHARE_ID_CHARACTERS = 64
export const SESSION_ID_BYTES = 8
export const SESSION_ID_CHARACTERS = 11
export const ROUTED_ENVELOPE_BYTES = 1 + SESSION_ID_BYTES

export const ENVELOPE_MANIFEST = 0x01
export const ENVELOPE_FORWARD = 0x02
export const ENVELOPE_TERMINAL_FORWARD = 0x03

export const SIGNAL_KEEPALIVE = 'keepalive'
export const SIGNAL_JOIN = 'join'
export const SIGNAL_MANIFEST = 'manifest'
export const SIGNAL_NOT_FOUND = 'not_found'
export const SIGNAL_SIGNAL = 'signal'
export const SIGNAL_BYE = 'bye'
export const SIGNAL_ERROR = 'error'

export const RELAY_ERROR_RATE_LIMITED = 'rate_limited'

const UTF8_ENCODER = new TextEncoder()
const UTF8_DECODER = new TextDecoder()
const BASE64URL_PATTERN = /^[A-Za-z0-9_-]+$/u

declare const sessionIdBrand: unique symbol
export type SessionId = Uint8Array & { readonly [sessionIdBrand]: 'SessionId' }

export type RelayProtocolErrorKind =
  | 'malformed-envelope'
  | 'unknown-envelope'
  | 'invalid-session-id'
  | 'invalid-share-id'
  | 'invalid-signaling'
  | 'oversize-signaling'
  | 'unknown-signaling'

export class RelayProtocolError extends Error {
  readonly kind: RelayProtocolErrorKind

  constructor(kind: RelayProtocolErrorKind, message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'RelayProtocolError'
    this.kind = kind
  }
}

export interface ManifestEnvelope {
  readonly type: 'manifest'
  readonly sealedManifest: Uint8Array
}

export interface ForwardEnvelope {
  readonly type: 'forward' | 'terminal-forward'
  readonly sessionId: SessionId
  readonly frame: Uint8Array
}

export type RelayEnvelope = ManifestEnvelope | ForwardEnvelope

export interface KeepaliveMessage {
  readonly type: 'keepalive'
}

export interface JoinMessage {
  readonly type: 'join'
  readonly shareId: string
}

export interface ManifestMessage {
  readonly type: 'manifest'
  readonly sessionId: string
}

export interface NotFoundMessage {
  readonly type: 'not_found'
}

export interface SignalMessage {
  readonly type: 'signal'
  readonly sessionId: string
  readonly kind: string
  readonly payload: unknown
}

export interface ByeMessage {
  readonly type: 'bye'
  readonly sessionId: string
}

export interface RelayErrorMessage {
  readonly type: 'error'
  readonly code: string
  readonly message?: string
  readonly sessionId?: string
}

export type RelaySignalingMessage =
  | KeepaliveMessage
  | JoinMessage
  | ManifestMessage
  | NotFoundMessage
  | SignalMessage
  | ByeMessage
  | RelayErrorMessage

export function validateShareId(shareId: string): void {
  if (
    shareId.length === 0 ||
    shareId.length > MAX_SHARE_ID_CHARACTERS ||
    !BASE64URL_PATTERN.test(shareId)
  ) {
    throw new RelayProtocolError(
      'invalid-share-id',
      `shareId must be 1..${MAX_SHARE_ID_CHARACTERS} base64url characters`,
    )
  }
}

function bytesToBinary(bytes: Uint8Array): string {
  let output = ''
  for (const byte of bytes) {
    output += String.fromCharCode(byte)
  }
  return output
}

function decodeBase64Url(encoded: string): Uint8Array {
  if (
    encoded.length !== SESSION_ID_CHARACTERS ||
    encoded.includes('=') ||
    !BASE64URL_PATTERN.test(encoded)
  ) {
    throw new RelayProtocolError(
      'invalid-session-id',
      'sessionId must use canonical unpadded base64url',
    )
  }
  const standard = encoded.replaceAll('-', '+').replaceAll('_', '/')
  const padded = standard.padEnd(Math.ceil(standard.length / 4) * 4, '=')
  try {
    return Uint8Array.from(atob(padded), (character) => character.charCodeAt(0))
  } catch (cause) {
    throw new RelayProtocolError('invalid-session-id', 'sessionId is not base64url', {
      cause,
    })
  }
}

export function createSessionId(bytes: Uint8Array): SessionId {
  if (bytes.byteLength !== SESSION_ID_BYTES) {
    throw new RelayProtocolError(
      'invalid-session-id',
      `sessionId is ${bytes.byteLength} bytes, expected ${SESSION_ID_BYTES}`,
    )
  }
  return bytes.slice() as SessionId
}

export function parseSessionId(encoded: string): SessionId {
  const id = createSessionId(decodeBase64Url(encoded))
  if (formatSessionId(id) !== encoded) {
    throw new RelayProtocolError(
      'invalid-session-id',
      'sessionId is not canonically encoded',
    )
  }
  return id
}

export function formatSessionId(id: SessionId): string {
  const encoded = btoa(bytesToBinary(createSessionId(id)))
    .replaceAll('+', '-')
    .replaceAll('/', '_')
  return encoded.endsWith('=') ? encoded.slice(0, -1) : encoded
}

export function sessionIdsEqual(left: SessionId, right: SessionId): boolean {
  if (left.byteLength !== right.byteLength) {
    return false
  }
  let difference = 0
  for (let index = 0; index < left.byteLength; index += 1) {
    difference |= (left[index] ?? 0) ^ (right[index] ?? 0)
  }
  return difference === 0
}

export function encodeManifestEnvelope(sealedManifest: Uint8Array): Uint8Array {
  const envelope = new Uint8Array(1 + sealedManifest.byteLength)
  envelope[0] = ENVELOPE_MANIFEST
  envelope.set(sealedManifest, 1)
  return envelope
}

function checkedInnerFrame(frame: Uint8Array): void {
  if (frame.byteLength < MIN_FRAME_BYTES || frame.byteLength > MAX_FRAME_BYTES) {
    throw new RelayProtocolError(
      'malformed-envelope',
      `inner frame must be ${MIN_FRAME_BYTES}..${MAX_FRAME_BYTES} bytes`,
    )
  }
}

function encodeRoutedEnvelope(
  kind: typeof ENVELOPE_FORWARD | typeof ENVELOPE_TERMINAL_FORWARD,
  sessionId: SessionId,
  frame: Uint8Array,
): Uint8Array {
  checkedInnerFrame(frame)
  const id = createSessionId(sessionId)
  const envelope = new Uint8Array(ROUTED_ENVELOPE_BYTES + frame.byteLength)
  envelope[0] = kind
  envelope.set(id, 1)
  envelope.set(frame, ROUTED_ENVELOPE_BYTES)
  return envelope
}

export function encodeForwardEnvelope(
  sessionId: SessionId,
  frame: Uint8Array,
): Uint8Array {
  return encodeRoutedEnvelope(ENVELOPE_FORWARD, sessionId, frame)
}

export function encodeTerminalForwardEnvelope(
  sessionId: SessionId,
  frame: Uint8Array,
): Uint8Array {
  return encodeRoutedEnvelope(ENVELOPE_TERMINAL_FORWARD, sessionId, frame)
}

export function decodeRelayEnvelope(wire: Uint8Array): RelayEnvelope {
  if (wire.byteLength === 0) {
    throw new RelayProtocolError('malformed-envelope', 'relay envelope is empty')
  }
  if (wire[0] === ENVELOPE_MANIFEST) {
    return { type: 'manifest', sealedManifest: wire.slice(1) }
  }
  if (wire[0] !== ENVELOPE_FORWARD && wire[0] !== ENVELOPE_TERMINAL_FORWARD) {
    throw new RelayProtocolError(
      'unknown-envelope',
      `unknown relay envelope type 0x${wire[0]?.toString(16).padStart(2, '0')}`,
    )
  }
  if (wire.byteLength < ROUTED_ENVELOPE_BYTES + MIN_FRAME_BYTES) {
    throw new RelayProtocolError('malformed-envelope', 'routed relay envelope is truncated')
  }
  const frame = wire.slice(ROUTED_ENVELOPE_BYTES)
  checkedInnerFrame(frame)
  return {
    type: wire[0] === ENVELOPE_FORWARD ? 'forward' : 'terminal-forward',
    sessionId: createSessionId(wire.subarray(1, ROUTED_ENVELOPE_BYTES)),
    frame,
  }
}

function signalingObject(value: unknown): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new RelayProtocolError('invalid-signaling', 'signaling JSON must be an object')
  }
  return value as Record<string, unknown>
}

function signalingDepthError(): RelayProtocolError {
  return new RelayProtocolError(
    'invalid-signaling',
    `signaling JSON exceeds nesting depth ${MAX_SIGNALING_JSON_DEPTH}`,
  )
}

// A non-recursive preflight keeps public encode calls below the same structural
// limit as wire decoding before JSON.stringify can inherit an engine stack cap.
function assertJSONValueDepth(value: unknown, limit: number): void {
  const pending: { readonly value: unknown; readonly parentDepth: number }[] = [
    { value, parentDepth: 0 },
  ]
  while (pending.length > 0) {
    const current = pending.pop()!
    if (typeof current.value !== 'object' || current.value === null) {
      continue
    }
    const depth = current.parentDepth + 1
    if (depth > limit) {
      throw signalingDepthError()
    }
    for (const child of Object.values(current.value)) {
      pending.push({ value: child, parentDepth: depth })
    }
  }
}

// Syntax remains JSON.parse's responsibility; this scanner only rejects deep
// hostile structure while correctly ignoring delimiters inside JSON strings.
function hasExcessiveJSONNesting(text: string): boolean {
  let depth = 0
  let inString = false
  let escaped = false
  for (let index = 0; index < text.length; index += 1) {
    const character = text[index]
    if (inString) {
      if (escaped) {
        escaped = false
      } else if (character === '\\') {
        escaped = true
      } else if (character === '"') {
        inString = false
      }
      continue
    }
    if (character === '"') {
      inString = true
    } else if (character === '{' || character === '[') {
      depth += 1
      if (depth > MAX_SIGNALING_JSON_DEPTH) {
        return true
      }
    } else if (character === '}' || character === ']') {
      depth -= 1
    }
  }
  return false
}

function requiredString(
  object: Record<string, unknown>,
  field: string,
): string {
  const value = object[field]
  if (
    typeof value !== 'string' ||
    value.length === 0 ||
    UTF8_DECODER.decode(UTF8_ENCODER.encode(value)) !== value
  ) {
    throw new RelayProtocolError(
      'invalid-signaling',
      `signaling field ${field} must be a non-empty string`,
    )
  }
  return value
}

function sessionIdString(object: Record<string, unknown>): string {
  const encoded = requiredString(object, 'sessionId')
  parseSessionId(encoded)
  return encoded
}

function decodeKnownSignaling(object: Record<string, unknown>): RelaySignalingMessage {
  const type = requiredString(object, 'type')
  switch (type) {
    case SIGNAL_KEEPALIVE:
      return { type }
    case SIGNAL_JOIN: {
      const shareId = requiredString(object, 'shareId')
      validateShareId(shareId)
      return { type, shareId }
    }
    case SIGNAL_MANIFEST:
      return { type, sessionId: sessionIdString(object) }
    case SIGNAL_NOT_FOUND:
      return { type }
    case SIGNAL_SIGNAL:
      if (!Object.hasOwn(object, 'payload')) {
        throw new RelayProtocolError('invalid-signaling', 'signal is missing payload')
      }
      assertJSONValueDepth(object.payload, MAX_SIGNALING_JSON_DEPTH - 1)
      return {
        type,
        sessionId: sessionIdString(object),
        kind: requiredString(object, 'kind'),
        payload: object.payload,
      }
    case SIGNAL_BYE:
      return { type, sessionId: sessionIdString(object) }
    case SIGNAL_ERROR:
      return decodeErrorMessage(object)
    default:
      throw new RelayProtocolError(
        'unknown-signaling',
        `unknown signaling message type ${JSON.stringify(type)}`,
      )
  }
}

function decodeErrorMessage(object: Record<string, unknown>): RelayErrorMessage {
  const message: RelayErrorMessage = {
    type: 'error',
    code: requiredString(object, 'code'),
  }
  if (object.message !== undefined) {
    if (
      typeof object.message !== 'string' ||
      object.message.length === 0 ||
      UTF8_DECODER.decode(UTF8_ENCODER.encode(object.message)) !== object.message
    ) {
      throw new RelayProtocolError(
        'invalid-signaling',
        'error message must be non-empty Unicode scalar text when present',
      )
    }
    Object.assign(message, { message: object.message })
  }
  if (object.sessionId !== undefined) {
    Object.assign(message, { sessionId: sessionIdString(object) })
  }
  return message
}

export function decodeSignaling(text: string): RelaySignalingMessage {
  if (
    text.length > MAX_SIGNALING_MESSAGE_BYTES ||
    UTF8_ENCODER.encode(text).byteLength > MAX_SIGNALING_MESSAGE_BYTES
  ) {
    throw new RelayProtocolError(
      'oversize-signaling',
      `signaling message exceeds ${MAX_SIGNALING_MESSAGE_BYTES} bytes`,
    )
  }
  if (hasExcessiveJSONNesting(text)) {
    throw signalingDepthError()
  }
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch (cause) {
    throw new RelayProtocolError('invalid-signaling', 'signaling JSON is malformed', {
      cause,
    })
  }
  return decodeKnownSignaling(signalingObject(parsed))
}

export function encodeSignaling(message: RelaySignalingMessage): string {
  const normalized = decodeKnownSignaling(signalingObject(message))
  let text: string
  try {
    const encoded = JSON.stringify(normalized)
    if (encoded === undefined) {
      throw new TypeError('JSON.stringify returned undefined')
    }
    text = encoded
  } catch (cause) {
    throw new RelayProtocolError(
      'invalid-signaling',
      'signaling message is not JSON-serializable',
      { cause },
    )
  }
  if (hasExcessiveJSONNesting(text)) {
    throw signalingDepthError()
  }
  if (UTF8_ENCODER.encode(text).byteLength > MAX_SIGNALING_MESSAGE_BYTES) {
    throw new RelayProtocolError(
      'oversize-signaling',
      `signaling message exceeds ${MAX_SIGNALING_MESSAGE_BYTES} bytes`,
    )
  }
  decodeSignaling(text)
  return text
}
