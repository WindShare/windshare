import {
  PROTOCOL_FAILURE_SCOPES,
  PROTOCOL_MESSAGE_KINDS_V1,
  PROTOCOL_REQUEST_KINDS_V1,
} from '../incident/fact'
import {
  booleanValue,
  decimalFields,
  decimalUint64,
  exactKeys,
  integerBetween,
  member,
  recordValue,
  uint16,
  uint32,
  validateCorrelationV1,
  type UnknownRecord,
} from './trace-payload-validation'

const MAX_AUTHENTICATED_RETRY_AFTER_MS = 30_000
const PEER_FAILURE_CODES = [
  'peer_negotiation',
  'peer_timeout',
  'peer_candidates',
  'peer_admission',
  'signaling_contract',
  'attempt_cancelled',
  'runtime_stopped',
  'unexpected',
] as const

export function validateProtocolOperation(payload: UnknownRecord): void {
  switch (payload.transition) {
    case 'request_sent':
    case 'request_send_failed':
    case 'cancelled':
      exactKeys(payload, ['transition', 'request_kind'], [], 'protocol request payload')
      member(payload.request_kind, PROTOCOL_MESSAGE_KINDS_V1, 'protocol request kind')
      return
    case 'response_received':
      exactKeys(payload, ['transition', 'request_kind', 'response_kind'], [],
        'protocol response payload')
      member(payload.request_kind, PROTOCOL_MESSAGE_KINDS_V1, 'protocol request kind')
      member(payload.response_kind, PROTOCOL_MESSAGE_KINDS_V1, 'protocol response kind')
      return
    case 'authenticated_failure': {
      exactKeys(payload, ['transition', 'request_kind', 'protocol_failure'], [],
        'protocol authenticated_failure payload')
      member(payload.request_kind, PROTOCOL_MESSAGE_KINDS_V1, 'protocol request kind')
      const failure = validateProtocolFailure(payload.protocol_failure)
      if (failure.request_kind !== payload.request_kind) {
        throw new TypeError('protocol failure request kind contradicts its trace payload')
      }
      return
    }
    case 'settled':
      exactKeys(payload, ['transition', 'request_kind', 'settlement'], [],
        'protocol settled payload')
      member(payload.request_kind, PROTOCOL_MESSAGE_KINDS_V1, 'protocol request kind')
      member(payload.settlement, ['remote_final', 'local_cancel', 'session_terminal'],
        'protocol settlement')
      return
    default:
      throw new TypeError('protocol_operation discriminant is invalid')
  }
}

export function validatePeerAttempt(payload: UnknownRecord): void {
  switch (payload.stage) {
    case 'provider_fact':
      exactKeys(payload, ['stage', 'fact'], [], 'peer provider fact payload')
      validateProviderFact(payload.fact)
      return
    case 'started':
      exactKeys(payload, [
        'stage', 'wave_ordinal', 'wave_attempt_ordinal', 'session_attempt_ordinal',
      ], [], 'peer attempt started payload')
      decimalFields(payload, ['wave_ordinal', 'wave_attempt_ordinal', 'session_attempt_ordinal'],
        'peer attempt started')
      return
    case 'negotiation_deadline_armed':
    case 'negotiation_deadline_expired':
    case 'admission_deadline_armed':
    case 'admission_deadline_expired':
      exactKeys(payload, ['stage', 'deadline_budget_ms'], [], 'peer deadline payload')
      uint32(payload.deadline_budget_ms, 'peer deadline budget')
      return
    case 'offer_created':
    case 'offer_sent':
    case 'answer_received':
    case 'datachannel_open':
      exactKeys(payload, [
        'stage', 'local_candidates_emitted', 'remote_candidates_accepted',
      ], [], 'peer negotiation payload')
      decimalFields(payload, ['local_candidates_emitted', 'remote_candidates_accepted'],
        'peer negotiation')
      return
    case 'grant_requested':
      exactKeys(payload, ['stage', 'requested_lane_id'], [], 'peer grant request payload')
      uint32(payload.requested_lane_id, 'peer requested lane id')
      return
    case 'grant_received':
    case 'lane_hello_sent':
    case 'admission_response_received':
    case 'lane_attached':
    case 'admitted':
      exactKeys(payload, ['stage'], [], 'peer stage payload')
      return
    case 'admission_response_settled':
      exactKeys(payload, ['stage', 'settlement'], [], 'peer admission settlement payload')
      validatePeerSettlement(payload.settlement)
      return
    case 'failed':
      exactKeys(payload, [
        'stage', 'failed_at_stage', 'failure_scope', 'code', 'retryable',
      ], [], 'peer failed payload')
      member(payload.failed_at_stage, [
        'negotiation_deadline_armed', 'negotiation_deadline_expired', 'offer_created',
        'offer_sent', 'answer_received', 'datachannel_open', 'admission_deadline_armed',
        'admission_deadline_expired', 'grant_requested', 'grant_received', 'lane_hello_sent',
        'admission_response_received', 'admission_response_settled', 'lane_attached',
        'admitted',
      ], 'peer failed-at stage')
      member(payload.failure_scope, ['attempt-transient', 'path-terminal', 'session-terminal'], 'peer failure scope')
      member(payload.code, PEER_FAILURE_CODES, 'peer failure code')
      booleanValue(payload.retryable, 'peer retryable')
      return
    default:
      throw new TypeError('peer_attempt discriminant is invalid')
  }
}

function validateProviderFact(input: unknown): void {
  const fact = recordValue(input, 'peer provider fact')
  const fields: Readonly<Record<string, readonly string[]>> = {
    state: ['kind', 'phase', 'state', 'elapsedMs'],
    candidate: ['kind', 'candidateType', 'protocol', 'family', 'interfaceClass', 'endpoint', 'disposition'],
    'selected-pair': ['kind', 'route', 'localType', 'remoteType', 'protocol', 'family', 'rttMs', 'ageMs', 'switchReason'],
    'ice-error': ['kind', 'code', 'endpoint'],
    profile: ['kind', 'networkGenerationID', 'profileID', 'side', 'endpointIDs'],
    'observer-loss': ['kind', 'count'],
  }
  if (typeof fact.kind !== 'string' || !Object.hasOwn(fields, fact.kind)) throw new TypeError('invalid provider fact kind')
  exactKeys(fact, fields[fact.kind]!, [], 'peer provider fact')
  for (const [key, value] of Object.entries(fact)) {
    if (key === 'rttMs' && value === null) continue
    if (['elapsedMs', 'rttMs', 'ageMs', 'code', 'count'].includes(key)) {
      if (typeof value !== 'number' || !Number.isFinite(value) || value < 0) throw new TypeError('invalid provider measurement')
    } else if (typeof value !== 'string' || value.length > 512) throw new TypeError('invalid provider label')
  }
}

export function validatePeerRecovery(payload: UnknownRecord): void {
  switch (payload.stage) {
    case 'wave_started':
    case 'wave_rearmed':
      exactKeys(payload, ['stage', 'wave_ordinal', 'trigger'], [], 'peer wave payload')
      decimalUint64(payload.wave_ordinal, 'peer wave ordinal')
      member(payload.trigger, ['activation', 'network_change', 'detachment'],
        'peer wave trigger')
      return
    case 'retry_decided':
      exactKeys(payload, [
        'stage', 'wave_ordinal', 'decision', 'reason', 'authenticated_retry_after_ms',
      ], [], 'peer retry decision payload')
      decimalUint64(payload.wave_ordinal, 'peer wave ordinal')
      member(payload.decision, ['retry_attempt', 'stop_path', 'stop_session'],
        'peer retry decision')
      member(payload.reason, [
        'local_transient', 'grant_expired', 'admission_limited', 'local_policy',
        'local_contract', 'peer_operation_final', 'lane_rejection_final',
        'untyped_failure', 'session_terminal',
      ], 'peer retry reason')
      integerBetween(payload.authenticated_retry_after_ms, 0,
        MAX_AUTHENTICATED_RETRY_AFTER_MS, 'peer authenticated retry-after')
      return
    case 'backoff_scheduled':
      exactKeys(payload, [
        'stage', 'wave_ordinal', 'retry_ordinal', 'local_delay_ms',
        'authenticated_retry_after_ms', 'effective_delay_ms',
      ], [], 'peer backoff payload')
      decimalFields(payload, ['wave_ordinal', 'retry_ordinal'], 'peer backoff')
      uint32(payload.local_delay_ms, 'peer local delay')
      integerBetween(payload.authenticated_retry_after_ms, 0,
        MAX_AUTHENTICATED_RETRY_AFTER_MS, 'peer authenticated retry-after')
      uint32(payload.effective_delay_ms, 'peer effective delay')
      return
    case 'attempt_replaced':
      exactKeys(payload, ['stage', 'wave_ordinal'], [], 'peer replacement payload')
      decimalUint64(payload.wave_ordinal, 'peer wave ordinal')
      return
    case 'wave_quiesced':
      exactKeys(payload, ['stage', 'wave_ordinal', 'reason'], [], 'peer quiesced payload')
      decimalUint64(payload.wave_ordinal, 'peer wave ordinal')
      member(payload.reason, ['wave_attempt_budget', 'wave_elapsed_budget'],
        'peer quiesce reason')
      return
    case 'peer_detached':
      exactKeys(payload, ['stage'], [], 'peer detached payload')
      return
    case 'session_budget_exhausted':
      exactKeys(payload, ['stage', 'reason'], [], 'peer session budget payload')
      member(payload.reason, ['session_attempt_budget', 'session_elapsed_budget'],
        'peer session budget reason')
      return
    case 'path_stopped':
      exactKeys(payload, ['stage', 'reason'], [], 'peer path stopped payload')
      member(payload.reason, [
        'local_policy', 'local_contract', 'peer_operation_final',
        'lane_rejection_final', 'untyped_failure',
      ], 'peer path stop reason')
      return
    case 'session_stopped':
      exactKeys(payload, ['stage', 'reason'], [], 'peer session stopped payload')
      member(payload.reason, [
        'runtime_closed', 'generation_retired', 'binding_conflict',
        'continuation_conflict', 'protocol_failure',
      ], 'peer session stop reason')
      return
    default:
      throw new TypeError('peer_recovery discriminant is invalid')
  }
}

export function validateLane(payload: UnknownRecord): void {
  switch (payload.transition) {
    case 'attached':
    case 'grant_requested':
    case 'grant_received':
    case 'hello_sent':
    case 'admission_accepted':
    case 'installed':
      exactKeys(payload, ['transition'], [], 'lane transition payload')
      return
    case 'admission_rejected':
      exactKeys(payload, ['transition', 'rejection_code', 'retry_after_ms'], [],
        'lane rejection payload')
      uint32(payload.rejection_code, 'lane rejection code')
      integerBetween(payload.retry_after_ms, 0, MAX_AUTHENTICATED_RETRY_AFTER_MS,
        'lane retry_after_ms')
      return
    case 'detached':
      exactKeys(payload, ['transition', 'detachment_class'], [], 'lane detached payload')
      member(payload.detachment_class,
        ['closed', 'physical_failure', 'authenticated_failure'],
        'lane detachment class')
      return
    default:
      throw new TypeError('lane_transition discriminant is invalid')
  }
}

function validateProtocolFailure(value: unknown): UnknownRecord {
  const failure = recordValue(value, 'protocol_failure')
  exactKeys(failure, [
    'request_kind', 'wire_scope', 'wire_code', 'retryable', 'settlement', 'correlation',
  ], ['retry_after_ms'], 'protocol_failure')
  member(failure.request_kind, PROTOCOL_REQUEST_KINDS_V1, 'protocol failure request kind')
  member(failure.wire_scope, PROTOCOL_FAILURE_SCOPES, 'protocol failure wire scope')
  uint16(failure.wire_code, 'protocol failure wire code')
  booleanValue(failure.retryable, 'protocol failure retryable')
  const settlement = recordValue(failure.settlement, 'protocol failure settlement')
  if (settlement.kind === 'received_authenticated') {
    exactKeys(settlement, ['kind'], [], 'received protocol failure settlement')
    if (failure.retryable === true) {
      if (failure.retry_after_ms === undefined) {
        throw new TypeError('retryable authenticated protocol failure requires retry_after_ms')
      }
      integerBetween(failure.retry_after_ms, 1, MAX_AUTHENTICATED_RETRY_AFTER_MS,
        'protocol failure retry_after_ms')
    } else if (failure.retry_after_ms !== undefined) {
      throw new TypeError('non-retryable protocol failure cannot contain retry_after_ms')
    }
  } else if (settlement.kind === 'response_send') {
    exactKeys(settlement, ['kind', 'admitted', 'settled', 'outcome'], [],
      'response-send protocol failure settlement')
    booleanValue(settlement.admitted, 'protocol response admission')
    booleanValue(settlement.settled, 'protocol response settlement')
    member(settlement.outcome, ['unknown', 'delivered', 'dropped'],
      'protocol response outcome')
    if (failure.retry_after_ms !== undefined) {
      throw new TypeError('response-send protocol failure cannot contain retry_after_ms')
    }
  } else {
    throw new TypeError('protocol failure settlement discriminant is invalid')
  }
  const correlation = validateCorrelationV1(failure.correlation, 'protocol failure correlation')
  if (correlation.protocol_session_id === undefined ||
      correlation.protocol_operation_id === undefined) {
    throw new TypeError('protocol failure requires session and operation correlation')
  }
  return failure
}

function validatePeerSettlement(value: unknown): void {
  const settlement = recordValue(value, 'peer settlement')
  if (settlement.disposition === 'accepted') {
    exactKeys(settlement, ['disposition'], [], 'accepted peer settlement')
    return
  }
  if (settlement.disposition === 'rejected') {
    exactKeys(settlement, ['disposition', 'rejection_code', 'retry_after_ms'], [],
      'rejected peer settlement')
    uint32(settlement.rejection_code, 'peer rejection code')
    integerBetween(settlement.retry_after_ms, 0, MAX_AUTHENTICATED_RETRY_AFTER_MS,
      'peer retry_after_ms')
    return
  }
  throw new TypeError('peer settlement discriminant is invalid')
}
