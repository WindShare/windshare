import {
  MAX_PROTOCOL_RETRY_AFTER_MILLISECONDS,
  PROTOCOL_ERROR_SCOPES,
  type ProtocolErrorContent,
  type ReceivedProtocolError,
} from '../incident/protocol-error'
import type { ProtocolErrorContentV2, ReceivedProtocolErrorV2 } from './incident-record-v2'
import { projectCorrelationV1 } from './correlation-v1'
import { deepFreezeJson } from './json'
import { booleanValue, exactKeys, integerBetween, member, recordValue, uint16 } from './trace-payload-validation'

export function projectProtocolErrorContentV2(content: ProtocolErrorContent): ProtocolErrorContentV2 {
  return deepFreezeJson({
    scope: content.scope,
    code: content.code,
    retryable: content.retryable,
    ...(content.retryAfterMilliseconds === undefined ? {} : { retry_after_ms: content.retryAfterMilliseconds }),
  })
}

export function projectReceivedProtocolErrorV2(error: ReceivedProtocolError): ReceivedProtocolErrorV2 {
  const correlation = projectCorrelationV1(error.correlation)
  if (correlation === undefined) throw new TypeError('Received protocol error requires correlation')
  return deepFreezeJson({
    request_kind: error.requestKind,
    content: projectProtocolErrorContentV2(error.content),
    correlation,
  })
}

export function validateProtocolErrorContentV2(value: unknown): asserts value is ProtocolErrorContentV2 {
  const content = recordValue(value, 'protocol_error')
  exactKeys(content, ['scope', 'code', 'retryable'], ['retry_after_ms'], 'protocol_error')
  member(content.scope, PROTOCOL_ERROR_SCOPES, 'protocol error scope')
  uint16(content.code, 'protocol error code')
  booleanValue(content.retryable, 'protocol error retryable')
  if (content.retryable === true) {
    integerBetween(content.retry_after_ms, 1, MAX_PROTOCOL_RETRY_AFTER_MILLISECONDS, 'protocol error retry_after_ms')
  } else if (Object.hasOwn(content, 'retry_after_ms')) {
    throw new TypeError('non-retryable protocol error cannot contain retry_after_ms')
  }
}
