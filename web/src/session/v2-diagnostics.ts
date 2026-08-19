import type {
  FailureCorrelation,
  ProtocolFailure,
  ProtocolMessageKindV1,
} from '../diagnostics/incident/fact'
import { V2_MESSAGE_KIND, type V2MessageKind } from './v2-message'

export type V2ProtocolOperationTransition =
  | 'request_sent'
  | 'request_send_failed'
  | 'response_received'
  | 'authenticated_failure'
  | 'cancelled'
  | 'settled'

export type V2ProtocolOperationSettlement =
  | 'remote_final'
  | 'local_cancel'
  | 'session_terminal'

export type V2LaneTransition =
  | 'attached'
  | 'detached'
  | 'grant_requested'
  | 'grant_received'
  | 'hello_sent'
  | 'admission_accepted'
  | 'admission_rejected'
  | 'installed'

export type V2LaneDetachmentClass =
  | 'closed'
  | 'physical_failure'
  | 'authenticated_failure'

export type V2ProtocolOperationTraceEvent =
  | Readonly<{
      eventName: 'protocol_operation'
      transition: 'request_sent' | 'request_send_failed' | 'cancelled'
      requestKind: ProtocolMessageKindV1
      correlation: FailureCorrelation
    }>
  | Readonly<{
      eventName: 'protocol_operation'
      transition: 'response_received'
      requestKind: ProtocolMessageKindV1
      responseKind: ProtocolMessageKindV1
      correlation: FailureCorrelation
    }>
  | Readonly<{
      eventName: 'protocol_operation'
      transition: 'authenticated_failure'
      requestKind: ProtocolMessageKindV1
      protocolFailure: ProtocolFailure
      correlation: FailureCorrelation
    }>
  | Readonly<{
      eventName: 'protocol_operation'
      transition: 'settled'
      requestKind: ProtocolMessageKindV1
      settlement: V2ProtocolOperationSettlement
      correlation: FailureCorrelation
    }>

export type V2LaneTransitionTraceEvent =
  | Readonly<{
      eventName: 'lane_transition'
      transition: 'attached' | 'grant_requested' | 'grant_received' | 'hello_sent' |
        'admission_accepted' | 'installed'
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
      detachmentClass: V2LaneDetachmentClass
      correlation: FailureCorrelation
    }>

export type V2ProtocolTraceEvent =
  | V2ProtocolOperationTraceEvent
  | V2LaneTransitionTraceEvent

export type V2ProtocolTraceObserver = (event: V2ProtocolTraceEvent) => void

/**
 * Session generations retain the source so enable/disable is observed at the
 * emission point. An absent observer means no trace-only object is constructed.
 */
export interface V2ProtocolTraceSource {
  readonly current: V2ProtocolTraceObserver | undefined
}

export function protocolMessageKindV1(kind: V2MessageKind): ProtocolMessageKindV1 {
  switch (kind) {
    case V2_MESSAGE_KIND.listChildren: return 'list_children'
    case V2_MESSAGE_KIND.catalogResult: return 'catalog_result'
    case V2_MESSAGE_KIND.openRevisions: return 'open_revisions'
    case V2_MESSAGE_KIND.openResults: return 'open_results'
    case V2_MESSAGE_KIND.renewLease: return 'renew_lease'
    case V2_MESSAGE_KIND.releaseLease: return 'release_lease'
    case V2_MESSAGE_KIND.requestBlocks: return 'request_blocks'
    case V2_MESSAGE_KIND.blockFragment: return 'block_fragment'
    case V2_MESSAGE_KIND.cancel: return 'cancel'
    case V2_MESSAGE_KIND.operationError: return 'operation_error'
    case V2_MESSAGE_KIND.sessionTerminal: return 'session_terminal'
    case V2_MESSAGE_KIND.laneAttach: return 'lane_attach'
    case V2_MESSAGE_KIND.scanProgress: return 'scan_progress'
    case V2_MESSAGE_KIND.operationComplete: return 'operation_complete'
    case V2_MESSAGE_KIND.leaseResult: return 'lease_result'
    case V2_MESSAGE_KIND.peerOffer: return 'peer_offer'
    case V2_MESSAGE_KIND.peerAnswer: return 'peer_answer'
    case V2_MESSAGE_KIND.peerCandidate: return 'peer_candidate'
  }
}
