import type { Ed25519Observation } from '../crypto/ed25519'
import type { RequestSchedulingObservation } from '../content/scheduling/requests'
import type { ProtocolCorrelation, ProtocolOperationIdentity, ProtocolSessionIdentity } from './identities'
import { MESSAGE_KIND, type MessageKind } from './message'
import type { CancellationTraceReason, OperationRequestTrace } from './operation-diagnostics'
import type { LaneTermination } from './runtime-types'

export const PROTOCOL_ERROR_SCOPES = Object.freeze([
  'directory',
  'revision',
  'block',
  'peer',
] as const)

export type ProtocolErrorScope = (typeof PROTOCOL_ERROR_SCOPES)[number]

export const PROTOCOL_REQUEST_KINDS = Object.freeze([
  'list_children',
  'open_revisions',
  'renew_lease',
  'release_lease',
  'request_blocks',
  'lane_attach',
  'peer_offer',
] as const)

export type ProtocolRequestKind = (typeof PROTOCOL_REQUEST_KINDS)[number]

export const PROTOCOL_MESSAGE_KINDS = Object.freeze([
  ...PROTOCOL_REQUEST_KINDS,
  'catalog_result',
  'open_results',
  'block_fragment',
  'cancel',
  'operation_error',
  'session_terminal',
  'scan_progress',
  'operation_complete',
  'lease_result',
  'peer_answer',
  'peer_candidate',
  'peer_path_control', 'turn_control', 'operation_retry',
] as const)

export type ProtocolMessageKind = (typeof PROTOCOL_MESSAGE_KINDS)[number]

export interface ProtocolErrorContent {
  readonly scope: ProtocolErrorScope
  readonly code: number
  readonly retryable: boolean
  readonly retryAfterMilliseconds?: number
}

export interface ReceivedProtocolError {
  readonly requestKind: ProtocolRequestKind
  readonly correlation: ProtocolCorrelation & Readonly<{
    protocolSessionId: ProtocolSessionIdentity
    protocolOperationId: ProtocolOperationIdentity
  }>
  readonly content: ProtocolErrorContent
}

export function createProtocolErrorContent(input: ProtocolErrorContent): ProtocolErrorContent {
  if (!PROTOCOL_ERROR_SCOPES.includes(input.scope)) {
    throw new RangeError('Unknown protocol error scope')
  }
  if (!Number.isInteger(input.code) || input.code < 0 || input.code > 0xffff) {
    throw new RangeError('Protocol error code must be a uint16')
  }
  if (input.retryAfterMilliseconds !== undefined &&
      (!Number.isInteger(input.retryAfterMilliseconds) || input.retryAfterMilliseconds < 0)) {
    throw new RangeError('Protocol retry delay must be a non-negative integer')
  }
  return Object.freeze({
    scope: input.scope,
    code: input.code,
    retryable: Boolean(input.retryable),
    ...(input.retryAfterMilliseconds === undefined ? {} : { retryAfterMilliseconds: input.retryAfterMilliseconds }),
  })
}

export function createReceivedProtocolError(input: {
  readonly requestKind: ProtocolMessageKind
  readonly correlation: ReceivedProtocolError['correlation']
  readonly content: ProtocolErrorContent
}): ReceivedProtocolError {
  if (!PROTOCOL_REQUEST_KINDS.includes(input.requestKind as ProtocolRequestKind)) {
    throw new RangeError('Received protocol error requires a request message kind')
  }
  return Object.freeze({
    requestKind: input.requestKind as ProtocolRequestKind,
    correlation: Object.freeze({
      ...input.correlation,
      ...(input.correlation.lane === undefined ? {} : { lane: Object.freeze({ ...input.correlation.lane }) }),
    }),
    content: createProtocolErrorContent(input.content),
  })
}

export type ProtocolOperationTransition =
  | 'send_queued' | 'send_sealing' | 'send_sending' | 'send_completed'
  | 'send_withdrawn' | 'send_abandoned' | 'send_failed'
  | 'request_sent'
  | 'request_send_failed'
  | 'response_received'
  | 'authenticated_failure'
  | 'cancelled'
  | 'late_response_discarded'
  | 'retired_response_discarded'
  | 'settled'
  | 'admission_waiting'
  | 'admission_ready'
  | 'admission_abandoned'

export type ProtocolOperationSettlement =
  | 'remote_final'
  | 'local_cancel'
  | 'session_terminal'

export type LaneTransition =
  | 'attached'
  | 'detached'
  | 'grant_requested'
  | 'grant_received'
  | 'hello_sent'
  | 'admission_response_received'
  | 'admission_accepted'
  | 'admission_rejected'
  | 'installed'

export interface BlockWaitTrace {
  readonly phase: 'awaiting_first_fragment' | 'receiving_fragments'
  readonly waitedMilliseconds: number
  readonly queueProgress: number
}

export type ProtocolOperationTraceEvent =
  | Readonly<{
      eventName: 'protocol_operation'
      transition: 'cancelled'
      requestKind: ProtocolMessageKind
      cancellationReason: CancellationTraceReason
      blockWait?: BlockWaitTrace
      request?: OperationRequestTrace
      correlation: ProtocolCorrelation
    }>
  | Readonly<{
      eventName: 'protocol_operation'
      transition: 'late_response_discarded'
      requestKind: ProtocolMessageKind
      responseKind: ProtocolMessageKind
      settlement: ProtocolOperationSettlement
      cancellationReason?: CancellationTraceReason
      request?: OperationRequestTrace
      protocolError?: ProtocolErrorContent
      correlation: ProtocolCorrelation
    }>
  | Readonly<{
      eventName: 'protocol_operation'
      failure?: unknown
      transition: 'request_sent' | 'request_send_failed' | 'admission_ready' | 'admission_abandoned'
        | 'send_queued' | 'send_sealing' | 'send_sending' | 'send_completed'
        | 'send_withdrawn' | 'send_abandoned' | 'send_failed'
      requestKind: ProtocolMessageKind
      correlation: ProtocolCorrelation
    }>
  | Readonly<{
      eventName: 'protocol_operation'
      transition: 'admission_waiting'
      requestKind: ProtocolMessageKind
      capacity: 'active'
      activeOperations: number
      trackedOperations: number
      correlation: ProtocolCorrelation
    }>
  | Readonly<{
      eventName: 'protocol_operation'
      transition: 'response_received' | 'retired_response_discarded'
      requestKind: ProtocolMessageKind
      responseKind: ProtocolMessageKind
      correlation: ProtocolCorrelation
    }>
  | Readonly<{
      eventName: 'protocol_operation'
      transition: 'authenticated_failure'
      requestKind: ProtocolMessageKind
      protocolError: ProtocolErrorContent
      correlation: ProtocolCorrelation
    }>
  | Readonly<{
      eventName: 'protocol_operation'
      transition: 'settled'
      requestKind: ProtocolMessageKind
      settlement: ProtocolOperationSettlement
      correlation: ProtocolCorrelation
    }>

export type LaneTransitionTraceEvent =
  | Readonly<{
      eventName: 'lane_transition'
      transition: 'attached' | 'unavailable' | 'grant_requested' | 'grant_received' | 'hello_sent' |
        'admission_response_received' | 'admission_accepted' | 'installed'
      correlation: ProtocolCorrelation
    }>
  | Readonly<{
      eventName: 'lane_transition'
      transition: 'admission_rejected'
      rejectionCode: number
      retryAfterMilliseconds: number
      correlation: ProtocolCorrelation
    }>
  | Readonly<{
      eventName: 'lane_transition'
      transition: 'detached'
      termination: LaneTermination
      correlation: ProtocolCorrelation
    }>

export type OperationRecoveryTraceEvent = Readonly<{
  eventName: 'operation_recovery'
  correlation: ProtocolCorrelation
  operationSequence: number
  generationId: number
  availabilityRevision: number
  laneCount: number
  unchangedAvailabilityRetries: number
}> & (
  | Readonly<{ transition: 'retry_available_lanes' | 'wait_for_generation' | 'exhausted' }>
  | Readonly<{ transition: 'wait_for_availability'; delayMilliseconds: number }>
)

export type ConnectionRecoveryTraceEvent = Readonly<{
  eventName: 'connection_recovery'
  correlation: ProtocolCorrelation
  generationId: number
  shareId?: string
  shareInstanceId?: string
  relayIdentity?: string
  attempt: number
  phase: 'initial' | 'fast' | 'waiting'
  transition: 'attempt_started' | 'attempt_failed' | 'waiting' | 'connected' | 'terminal' | 'retry_requested'
  delayMilliseconds?: number
  waitReason?: 'backoff' | 'capacity' | 'server' | 'bandwidth'
  failure?: unknown
}>

export type RelayHeartbeatTraceEvent = Readonly<{
  eventName: 'relay_heartbeat'
  correlation: ProtocolCorrelation
  connectionId: bigint
  generationId?: number
  shareId?: string
  shareInstanceId?: string
  relayIdentity: string
  round: bigint
  stage: 'probe' | 'acknowledged' | 'failed'
  bufferedBytes: number
  elapsedMilliseconds: number
  timeoutMilliseconds: number
}>

export type RelayDeliveryTraceEvent = import('../contracts/relay-delivery-observation').ReceiveLedgerEvent & Readonly<{
  eventName: 'relay_delivery'
  correlation: ProtocolCorrelation
  relayIdentity: string
  shareId?: string
  shareInstanceId?: string
}>

export type ContentSchedulingTraceEvent = Readonly<{
  admission?: import('../content/scheduling/receive-window').BlockReceiveAdmission
  eventName: 'content_scheduling'
  correlation: ProtocolCorrelation
  dispatchSequence: number
  fileId: string
  localBlockIndex: bigint
  route: 'application-relay' | 'direct' | 'turn'
  purpose: import('../content/scheduling/exploration').DispatchPurpose
  deliveryEvidence?: 'unknown' | 'recent' | 'stale'
  rescueReason?: import('../content/scheduling/exploration').RescueReason
  expectedMilliseconds: number
  pendingBytes: number
  bytesPerSecond: number
}>

export type ContentRetryTraceEvent = import('../content/lane-set').BlockRetryObservation & Readonly<{
  eventName: 'content_retry'
  correlation: ProtocolCorrelation
}>

export type RequestSchedulingTraceEvent = RequestSchedulingObservation & Readonly<{
  eventName: 'request_scheduling'
  correlation: ProtocolCorrelation
}>

export type LeaseRetirementTraceEvent = import('../content/scheduling/lease-retirement').LeaseRetirementObservation & Readonly<{
  eventName: 'lease_retirement'
  correlation: ProtocolCorrelation
}>

export type SenderVerificationTraceEvent = Ed25519Observation & Readonly<{
  eventName: 'sender_verification'
  shareId: string
  correlation: ProtocolCorrelation
}>

export type ProtocolTraceEvent =
  | SenderVerificationTraceEvent
  | LeaseRetirementTraceEvent
  | RequestSchedulingTraceEvent
  | ContentSchedulingTraceEvent
  | ContentRetryTraceEvent
  | ProtocolOperationTraceEvent
  | LaneTransitionTraceEvent
  | OperationRecoveryTraceEvent
  | ConnectionRecoveryTraceEvent
  | RelayHeartbeatTraceEvent
  | RelayDeliveryTraceEvent

export type ProtocolTraceObserver = (event: ProtocolTraceEvent) => void

/**
 * Session generations retain the source so enable/disable is observed at the
 * emission point. An absent observer means no trace-only object is constructed.
 */
export interface ProtocolTraceSource {
  readonly failure?: (
    error: unknown,
    operation: string,
    correlation: ProtocolCorrelation,
    context?: Readonly<Record<string, unknown>>,
  ) => void
  readonly current: ProtocolTraceObserver | undefined
}

export function protocolMessageKindV1(kind: MessageKind): ProtocolMessageKind {
  switch (kind) {
    case MESSAGE_KIND.listChildren: return 'list_children'
    case MESSAGE_KIND.catalogResult: return 'catalog_result'
    case MESSAGE_KIND.openRevisions: return 'open_revisions'
    case MESSAGE_KIND.openResults: return 'open_results'
    case MESSAGE_KIND.renewLease: return 'renew_lease'
    case MESSAGE_KIND.releaseLease: return 'release_lease'
    case MESSAGE_KIND.requestBlocks: return 'request_blocks'
    case MESSAGE_KIND.blockFragment: return 'block_fragment'
    case MESSAGE_KIND.cancel: return 'cancel'
    case MESSAGE_KIND.operationError: return 'operation_error'
    case MESSAGE_KIND.sessionTerminal: return 'session_terminal'
    case MESSAGE_KIND.laneAttach: return 'lane_attach'
    case MESSAGE_KIND.scanProgress: return 'scan_progress'
    case MESSAGE_KIND.operationComplete: return 'operation_complete'
    case MESSAGE_KIND.leaseResult: return 'lease_result'
    case MESSAGE_KIND.peerOffer: return 'peer_offer'
    case MESSAGE_KIND.peerAnswer: return 'peer_answer'
    case MESSAGE_KIND.peerCandidate: return 'peer_candidate'
    case MESSAGE_KIND.turnControl: return 'turn_control'
    case MESSAGE_KIND.peerPathControl: return 'peer_path_control'
    case MESSAGE_KIND.operationRetry: return 'operation_retry'
  }
}
