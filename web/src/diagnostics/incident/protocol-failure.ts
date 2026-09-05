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

export const PROTOCOL_FAILURE_SCOPES = Object.freeze([
  'directory',
  'revision',
  'block',
  'peer',
] as const)

export type ProtocolFailureScope = (typeof PROTOCOL_FAILURE_SCOPES)[number]

export type ProtocolSettlement =
  | Readonly<{ kind: 'received_authenticated' }>
  | Readonly<{
      kind: 'response_send'
      admitted: boolean
      settled: boolean
      outcome: 'unknown' | 'delivered' | 'dropped'
    }>

interface ProtocolFailureCommon {
  readonly requestKind: ProtocolRequestKindV1
  readonly wireScope: ProtocolFailureScope
  readonly wireCode: number
  readonly correlation: FailureCorrelation & Readonly<{
    protocolSessionId: FailureIdentity<'protocol_session'>
    protocolOperationId: FailureIdentity<'protocol_operation'>
  }>
}

export type ProtocolFailure =
  | Readonly<ProtocolFailureCommon & {
      retryable: true
      retryAfterMilliseconds: number
      settlement: Readonly<{ kind: 'received_authenticated' }>
    }>
  | Readonly<ProtocolFailureCommon & {
      retryable: false
      retryAfterMilliseconds?: never
      settlement: Readonly<{ kind: 'received_authenticated' }>
    }>
  | Readonly<ProtocolFailureCommon & {
      retryable: boolean
      retryAfterMilliseconds?: never
      settlement: Extract<ProtocolSettlement, Readonly<{ kind: 'response_send' }>>
    }>

// Decoded routing supplies a wire-wide kind; successful construction is the
// single seam that narrows it to an initiating request.
export interface ProtocolFailureInput {
  readonly requestKind: ProtocolMessageKindV1
  readonly wireScope: ProtocolFailureScope
  readonly wireCode: number
  readonly retryable: boolean
  readonly retryAfterMilliseconds?: number
  readonly settlement: ProtocolSettlement
  readonly correlation: ProtocolFailureCommon['correlation']
}

const UINT16_MAX = 0xffff
const MAX_RETRY_AFTER_MILLISECONDS = 30_000

export function createProtocolFailure(input: ProtocolFailureInput): ProtocolFailure {
  if (!isProtocolFailure(input)) throw new TypeError('Protocol failure is invalid')
  const correlation = createFailureCorrelation(input.correlation)
  if (
    correlation.protocolSessionId === undefined ||
    correlation.protocolOperationId === undefined
  ) {
    throw new TypeError('Protocol failures require session and operation identities')
  }
  const common = Object.freeze({
    requestKind: input.requestKind,
    wireScope: input.wireScope,
    wireCode: input.wireCode,
    correlation: correlation as ProtocolFailureCommon['correlation'],
  })
  if (input.settlement.kind === 'received_authenticated') {
    const settlement = Object.freeze({ kind: 'received_authenticated' } as const)
    if (input.retryable) {
      const retryAfterMilliseconds = input.retryAfterMilliseconds
      if (retryAfterMilliseconds === undefined) {
        throw new TypeError('Retryable authenticated failures require retry-after')
      }
      return Object.freeze({
        ...common,
        retryable: true,
        retryAfterMilliseconds,
        settlement,
      })
    }
    return Object.freeze({
      ...common,
      retryable: false,
      settlement,
    })
  }
  return Object.freeze({
    ...common,
    retryable: input.retryable,
    settlement: Object.freeze({ ...input.settlement }),
  })
}

export function isProtocolFailure(value: unknown): value is ProtocolFailure {
  if (!isRecord(value) || !hasExactOptionalKeys(
    value,
    ['requestKind', 'wireScope', 'wireCode', 'retryable', 'settlement', 'correlation'],
    ['retryAfterMilliseconds'],
  )) {
    return false
  }
  if (
    !isMember(PROTOCOL_REQUEST_KINDS_V1, value.requestKind) ||
    !isMember(PROTOCOL_FAILURE_SCOPES, value.wireScope) ||
    !isIntegerBetween(value.wireCode, 0, UINT16_MAX) ||
    typeof value.retryable !== 'boolean' ||
    !isProtocolSettlement(value.settlement) ||
    !isFailureCorrelation(value.correlation)
  ) {
    return false
  }
  if (
    value.correlation.protocolSessionId === undefined ||
    value.correlation.protocolOperationId === undefined
  ) {
    return false
  }
  const retryAfterRequired =
    value.settlement.kind === 'received_authenticated' && value.retryable
  if (Object.hasOwn(value, 'retryAfterMilliseconds') !== retryAfterRequired) {
    return false
  }
  if (!retryAfterRequired) return true
  return isIntegerBetween(
    value.retryAfterMilliseconds,
    1,
    MAX_RETRY_AFTER_MILLISECONDS,
  )
}

function isProtocolSettlement(value: unknown): value is ProtocolSettlement {
  if (!isRecord(value)) return false
  if (value.kind === 'received_authenticated') {
    return hasExactKeys(value, ['kind'])
  }
  return (
    value.kind === 'response_send' &&
    hasExactKeys(value, ['kind', 'admitted', 'settled', 'outcome']) &&
    typeof value.admitted === 'boolean' &&
    typeof value.settled === 'boolean' &&
    isMember(['unknown', 'delivered', 'dropped'] as const, value.outcome)
  )
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

function hasExactKeys(
  value: Record<string, unknown>,
  expected: readonly string[],
): boolean {
  const keys = Object.keys(value)
  return keys.length === expected.length && expected.every((key) => keys.includes(key))
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
