import type { ProtocolMessageKindV1 } from '../incident/fact'
import type { ProtocolFailureV1 } from '../export/incident-record-v1'

export interface ProtocolRequestTraceV1 {
  readonly lease_id: string
  readonly blocks?: Readonly<{ first_index: string; count: number }>
}

export type ProtocolOperationPayloadV1 =
  | Readonly<{
      transition: 'cancelled'
      request_kind: ProtocolMessageKindV1
      cancellation_reason: 'user' | 'superseded' | 'output_abort' | 'timeout' | 'lane_race'
      request?: ProtocolRequestTraceV1
    }>
  | Readonly<{
      transition: 'late_response_discarded'
      request_kind: ProtocolMessageKindV1
      response_kind: ProtocolMessageKindV1
      settlement: 'remote_final' | 'local_cancel' | 'session_terminal'
      cancellation_reason?: 'user' | 'superseded' | 'output_abort' | 'timeout' | 'lane_race'
      request?: ProtocolRequestTraceV1
      protocol_failure?: ProtocolFailureV1
    }>
  | Readonly<{
      transition: 'request_sent' | 'request_send_failed' | 'admission_ready' | 'admission_abandoned'
        | 'send_queued' | 'send_sealing' | 'send_sending' | 'send_completed'
        | 'send_withdrawn' | 'send_abandoned' | 'send_failed'
      request_kind: ProtocolMessageKindV1
    }>
  | Readonly<{
      transition: 'admission_waiting'
      request_kind: ProtocolMessageKindV1
      capacity: 'active' | 'retained'
      active_operations: string
      tracked_operations: string
    }>
  | Readonly<{
      transition: 'response_received'
      request_kind: ProtocolMessageKindV1
      response_kind: ProtocolMessageKindV1
    }>
  | Readonly<{
      transition: 'authenticated_failure'
      request_kind: ProtocolMessageKindV1
      protocol_failure: ProtocolFailureV1
    }>
  | Readonly<{
      transition: 'settled'
      request_kind: ProtocolMessageKindV1
      settlement: 'remote_final' | 'local_cancel' | 'session_terminal'
    }>
