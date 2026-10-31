import type { FailureEvidence } from '../diagnostics/failure/evidence'
import type { Ed25519Observation } from '../crypto/ed25519'
import type { LaneTermination } from './runtime-types'
import type { CancellationTraceReason, OperationRequestTrace } from './operation-diagnostics'
import type { RequestSchedulingObservation } from '../content/scheduling/requests'
import type { FailureCorrelation } from '../diagnostics/incident/failure-correlation'
import type { ProtocolErrorContent, ProtocolMessageKindV1 } from '../diagnostics/incident/protocol-error'
import { MESSAGE_KIND, type MessageKind } from './message'

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
      requestKind: ProtocolMessageKindV1
      cancellationReason: CancellationTraceReason
      blockWait?: BlockWaitTrace
      request?: OperationRequestTrace
      correlation: FailureCorrelation
    }>
  | Readonly<{
      eventName: 'protocol_operation'
      transition: 'late_response_discarded'
      requestKind: ProtocolMessageKindV1
      responseKind: ProtocolMessageKindV1
      settlement: ProtocolOperationSettlement
      cancellationReason?: CancellationTraceReason
      request?: OperationRequestTrace
      protocolError?: ProtocolErrorContent
      correlation: FailureCorrelation
    }>
  | Readonly<{
      eventName: 'protocol_operation'
      evidence?: FailureEvidence
      transition: 'request_sent' | 'request_send_failed' | 'admission_ready' | 'admission_abandoned'
        | 'send_queued' | 'send_sealing' | 'send_sending' | 'send_completed'
        | 'send_withdrawn' | 'send_abandoned' | 'send_failed'
      requestKind: ProtocolMessageKindV1
      correlation: FailureCorrelation
    }>
  | Readonly<{
      eventName: 'protocol_operation'
      transition: 'admission_waiting'
      requestKind: ProtocolMessageKindV1
      capacity: 'active' | 'retained'
      activeOperations: number
      trackedOperations: number
      correlation: FailureCorrelation
    }>
  | Readonly<{
      eventName: 'protocol_operation'
      transition: 'response_received' | 'retired_response_discarded'
      requestKind: ProtocolMessageKindV1
      responseKind: ProtocolMessageKindV1
      correlation: FailureCorrelation
    }>
  | Readonly<{
      eventName: 'protocol_operation'
      transition: 'authenticated_failure'
      requestKind: ProtocolMessageKindV1
      protocolError: ProtocolErrorContent
      correlation: FailureCorrelation
    }>
  | Readonly<{
      eventName: 'protocol_operation'
      transition: 'settled'
      requestKind: ProtocolMessageKindV1
      settlement: ProtocolOperationSettlement
      correlation: FailureCorrelation
    }>

export type LaneTransitionTraceEvent =
  | Readonly<{
      eventName: 'lane_transition'
      transition: 'attached' | 'grant_requested' | 'grant_received' | 'hello_sent' |
        'admission_response_received' | 'admission_accepted' | 'installed'
      correlation: FailureCorrelation
    }>
  | Readonly<{
      eventName: 'lane_transition'
      transition: 'admission_rejected'
      rejectionCode: number
      retryAfterMilliseconds: number
      correlation: FailureCorrelation
    }>
  | Readonly<{
      eventName: 'lane_transition'
      transition: 'detached'
      termination: LaneTermination
      correlation: FailureCorrelation
    }>

export type OperationRecoveryTraceEvent = Readonly<{
  eventName: 'operation_recovery'
  correlation: FailureCorrelation
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
  correlation: FailureCorrelation
  generationId: number
  shareId?: string
  shareInstanceId?: string
  relayBase?: string
  attempt: number
  phase: 'initial' | 'fast' | 'waiting'
  transition: 'attempt_started' | 'attempt_failed' | 'waiting' | 'connected' | 'terminal' | 'retry_requested'
  delayMilliseconds?: number
  waitReason?: 'backoff' | 'capacity' | 'server'
  failure?: unknown
}>

export type RelayHeartbeatTraceEvent = Readonly<{
  eventName: 'relay_heartbeat'
  correlation: FailureCorrelation
  connectionId: bigint
  generationId?: number
  shareId?: string
  shareInstanceId?: string
  relayBase: string
  round: bigint
  stage: 'probe' | 'acknowledged' | 'failed'
  bufferedBytes: number
  elapsedMilliseconds: number
  timeoutMilliseconds: number
}>

export type RelayDeliveryTraceEvent = import('../contracts/relay-delivery-observation').ReceiveLedgerEvent & Readonly<{
  eventName: 'relay_delivery'
  correlation: FailureCorrelation
  relayBase: string
  shareId?: string
  shareInstanceId?: string
}>

export type ContentSchedulingTraceEvent = Readonly<{
  admission?: import('../content/scheduling/receive-window').BlockReceiveAdmission
  eventName: 'content_scheduling'
  correlation: FailureCorrelation
  dispatchSequence: number
  fileId: string
  localBlockIndex: bigint
  route: 'application-relay' | 'direct' | 'turn'
  purpose: 'content' | 'probe' | 'sample' | 'rescue'
  deliveryEvidence?: 'unknown' | 'recent' | 'stale'
  rescueReason?: import('../content/scheduling/exploration').RescueReason
  expectedMilliseconds: number
  pendingBytes: number
  bytesPerSecond: number
}>

export type ContentRetryTraceEvent = import('../content/lane-set').BlockRetryObservation & Readonly<{
  eventName: 'content_retry'
  correlation: FailureCorrelation
}>

export type RequestSchedulingTraceEvent = RequestSchedulingObservation & Readonly<{
  eventName: 'request_scheduling'
  correlation: FailureCorrelation
}>

export type LeaseRetirementTraceEvent = import('../content/scheduling/lease-retirement').LeaseRetirementObservation & Readonly<{
  eventName: 'lease_retirement'
  correlation: FailureCorrelation
}>

export type SenderVerificationTraceEvent = Ed25519Observation & Readonly<{
  eventName: 'sender_verification'
  shareId: string
  correlation: FailureCorrelation
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
  readonly failure?: (error: unknown, operation: string, correlation: FailureCorrelation, relation?: 'consequence') => void
  readonly current: ProtocolTraceObserver | undefined
}

export function protocolMessageKindV1(kind: MessageKind): ProtocolMessageKindV1 {
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
    case MESSAGE_KIND.peerPathControl: return 'peer_path_control'
    case MESSAGE_KIND.turnControl: return 'turn_control'
  }
}
