import {
  createFailureCorrelation,
  isFailureCorrelation,
  type FailureCorrelation,
  type FailureIdentity,
} from './failure-correlation'

export const PROTOCOL_MESSAGE_KINDS_V1 = Object.freeze([
  'list_children',
  'catalog_result',
  'open_revisions',
  'open_results',
  'renew_lease',
  'release_lease',
  'request_blocks',
  'block_fragment',
  'cancel',
  'operation_error',
  'session_terminal',
  'lane_attach',
  'scan_progress',
  'operation_complete',
  'lease_result',
  'peer_offer',
  'peer_answer',
  'peer_candidate',
  'peer_path_control',
] as const)

export type ProtocolMessageKindV1 = (typeof PROTOCOL_MESSAGE_KINDS_V1)[number]

// Failure attribution names the operation's initiating receiver request. Keeping
// this subset closed prevents a response or continuation kind from being
// misreported as the request that owned an authenticated failure.
export const PROTOCOL_REQUEST_KINDS_V1 = Object.freeze([
  'list_children',
  'open_revisions',
  'renew_lease',
  'release_lease',
  'request_blocks',
  'lane_attach',
  'peer_offer',
] as const satisfies readonly ProtocolMessageKindV1[])

export type ProtocolRequestKindV1 = (typeof PROTOCOL_REQUEST_KINDS_V1)[number]

export const PROTOCOL_ERROR_SCOPES = Object.freeze([
  'directory',
  'revision',
  'block',
  'peer',
] as const)

export type ProtocolErrorScope = (typeof PROTOCOL_ERROR_SCOPES)[number]

export type ProtocolErrorContent =
  | Readonly<{
      scope: ProtocolErrorScope
      code: number
      retryable: true
      retryAfterMilliseconds: number
    }>
  | Readonly<{
      scope: ProtocolErrorScope
      code: number
      retryable: false
      retryAfterMilliseconds?: never
    }>

export interface ProtocolErrorContentInput {
  readonly scope: ProtocolErrorScope
  readonly code: number
  readonly retryable: boolean
  readonly retryAfterMilliseconds?: number
}

export interface ReceivedProtocolError {
  readonly requestKind: ProtocolRequestKindV1
  readonly content: ProtocolErrorContent
  readonly correlation: FailureCorrelation & Readonly<{
    protocolSessionId: FailureIdentity<'protocol_session'>
    protocolOperationId: FailureIdentity<'protocol_operation'>
  }>
}

// Decoded routing supplies the wire-wide kind. This seam narrows attribution to
// the initiating request and snapshots the authenticated receive context.
export interface ReceivedProtocolErrorInput {
  readonly requestKind: ProtocolMessageKindV1
  readonly content: ProtocolErrorContentInput
  readonly correlation: ReceivedProtocolError['correlation']
}

const UINT16_MAX = 0xffff
export const MAX_PROTOCOL_RETRY_AFTER_MILLISECONDS = 30_000

export function createProtocolErrorContent(input: ProtocolErrorContentInput): ProtocolErrorContent {
  if (!isProtocolErrorContent(input)) throw new TypeError('Protocol error content is invalid')
  return Object.freeze({ ...input })
}

export function isProtocolErrorContent(value: unknown): value is ProtocolErrorContent {
  if (!isRecord(value) || !hasExactOptionalKeys(
    value, ['scope', 'code', 'retryable'], ['retryAfterMilliseconds'],
  )) return false
  if (!isMember(PROTOCOL_ERROR_SCOPES, value.scope) ||
      !isIntegerBetween(value.code, 0, UINT16_MAX) ||
      typeof value.retryable !== 'boolean') return false
  if (Object.hasOwn(value, 'retryAfterMilliseconds') !== value.retryable) return false
  return !value.retryable || isIntegerBetween(
    value.retryAfterMilliseconds, 1, MAX_PROTOCOL_RETRY_AFTER_MILLISECONDS,
  )
}

export function createReceivedProtocolError(input: ReceivedProtocolErrorInput): ReceivedProtocolError {
  if (!isReceivedProtocolError(input)) throw new TypeError('Received protocol error is invalid')
  return Object.freeze({
    requestKind: input.requestKind,
    content: createProtocolErrorContent(input.content),
    correlation: createFailureCorrelation(input.correlation) as ReceivedProtocolError['correlation'],
  })
}

export function isReceivedProtocolError(value: unknown): value is ReceivedProtocolError {
  return isRecord(value) &&
    hasExactOptionalKeys(value, ['requestKind', 'content', 'correlation'], []) &&
    isMember(PROTOCOL_REQUEST_KINDS_V1, value.requestKind) &&
    isProtocolErrorContent(value.content) &&
    isFailureCorrelation(value.correlation) &&
    value.correlation.protocolSessionId !== undefined &&
    value.correlation.protocolOperationId !== undefined
}

function isIntegerBetween(value: unknown, minimum: number, maximum: number): boolean {
  return (
    typeof value === 'number' &&
    Number.isInteger(value) &&
    value >= minimum &&
    value <= maximum
  )
}

function isMember<const Value extends string>(
  values: readonly Value[],
  value: unknown,
): value is Value {
  return typeof value === 'string' && values.includes(value as Value)
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function hasExactOptionalKeys(
  value: Record<string, unknown>,
  required: readonly string[],
  optional: readonly string[],
): boolean {
  const keys = Object.keys(value)
  return (
    required.every((key) => keys.includes(key)) &&
    keys.every((key) => required.includes(key) || optional.includes(key))
  )
}
