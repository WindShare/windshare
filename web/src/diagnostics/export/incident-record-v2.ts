import type { FailureEvidence } from '../failure/evidence'
import type {
  FailureFactKind,
  FailureStage,
  NativeFailureClass,
  ProtocolErrorScope,
  ProtocolRequestKindV1,
  RecoveryDisposition,
} from '../incident/fact'
import type {
  PresentationBoundary,
  PresentationOutcome,
} from '../incident/presentation'
import type { CorrelationV1 } from './correlation-v1'
import type { DiagnosticContextV1 } from './context'

export const INCIDENT_RECORD_SCHEMA_VERSION = 2 as const
export const INCIDENT_RECORD_EVENT = 'failure_incident' as const

export interface DiagnosticEventEnvelopeV2<Payload extends object> {
  readonly schema_version: typeof INCIDENT_RECORD_SCHEMA_VERSION
  readonly sequence: string
  readonly time: string
  readonly elapsed_ms: string
  readonly level: 'debug' | 'info' | 'warn' | 'error'
  readonly event: string
  readonly runtime_run_id: string
  readonly correlation?: CorrelationV1
  readonly payload: Payload
}

export interface BuildIdentityV1 {
  readonly application: 'windshare_web'
  readonly version: string
  readonly revision?: string
  readonly mode: 'development' | 'production' | 'test'
}

export interface RuntimeIdentityV1 {
  readonly kind: 'browser'
  readonly secure_context: boolean
}

export interface ProtocolErrorContentV2 {
  readonly scope: ProtocolErrorScope
  readonly code: number
  readonly retryable: boolean
  readonly retry_after_ms?: number
}

export interface ReceivedProtocolErrorV2 {
  readonly request_kind: ProtocolRequestKindV1
  readonly content: ProtocolErrorContentV2
  readonly correlation: CorrelationV1
}

export type FaultDomainV1 =
  | 'protocol'
  | 'source'
  | 'output'
  | 'checkpoint'
  | 'cancellation'

export type FaultScopeV1 =
  | 'job'
  | 'directory_local'
  | 'file_local'

export type FaultCodeV1 =
  | NativeOutputCodeV1
  | 'revision_changed'
  | 'revision_invalidated'
  | 'unavailable'
  | 'permanent'
  | (string & Record<never, never>)

export type PeerFailureCodeV1 =
  | 'peer_negotiation'
  | 'peer_timeout'
  | 'peer_candidates'
  | 'peer_admission'
  | 'peer_busy'
  | 'signaling_contract'
  | 'attempt_cancelled'
  | 'runtime_stopped'
  | 'unexpected'

export type NativeOutputCodeV1 =
  | 'write_failed'
  | 'commit_failed'
  | 'quota_exceeded'
  | 'permission_denied'
  | 'directory_metadata'
  | 'storage_unavailable'
  | 'target_collision'
  | 'invalid'
  | 'storage_failed'
  | 'busy'

export type LifecycleStateV1 =
  | 'preparing'
  | 'downloading'
  | 'paused'
  | 'ready_to_save'
  | 'download_started'
  | 'saved'
  | 'partial'
  | 'failed'

export type LifecycleReasonV1 =
  | 'user'
  | 'interrupted'
  | 'storage_full'
  | 'permission_needed'
  | 'failures'
  | 'stopped'
  | 'source_changed'
  | 'cancelled'
  | 'session_ended'
  | 'error'

interface FailureFactEnvelopeV2<
  Kind extends FailureFactKind,
  Payload extends object,
> {
  readonly kind: Kind
  readonly stage: FailureStage
  readonly recovery_disposition: RecoveryDisposition
  readonly correlation?: CorrelationV1
  readonly evidence?: FailureEvidence
  readonly payload: Payload
}

export type FailureFactV2 =
  | Readonly<FailureFactEnvelopeV2<'fault', Readonly<{
      fault: Readonly<{
        domain: FaultDomainV1
        scope: FaultScopeV1
        code: FaultCodeV1
      }>
    }>>>
  | Readonly<FailureFactEnvelopeV2<'protocol_failure', Readonly<{
      protocol_failure: ReceivedProtocolErrorV2
    }>>>
  | Readonly<FailureFactEnvelopeV2<'peer_failure', Readonly<{
      peer_failure: Readonly<{
        scope: 'attempt-transient' | 'resource-deferred' | 'path-terminal' | 'session-terminal'
        code: PeerFailureCodeV1
        retryable: boolean
      }>
    }>>>
  | Readonly<FailureFactEnvelopeV2<'native_output_failure', Readonly<{
      native_output_failure: Readonly<{
        native_class: NativeFailureClass
        code?: NativeOutputCodeV1
      }>
    }>>>
  | Readonly<FailureFactEnvelopeV2<'lifecycle_failure', Readonly<{
      lifecycle_failure: Readonly<{
        state: LifecycleStateV1
        reason?: LifecycleReasonV1
      }>
    }>>>
  | Readonly<FailureFactEnvelopeV2<'unclassified', Readonly<{
      unclassified: Readonly<Record<string, never>>
    }>>>

export interface FailureFactBucketV2 {
  readonly cause_signature?: string
  readonly fingerprint: string
  readonly count: string
  readonly representative: FailureFactV2
}

export interface DiagnosticsHealthV1 {
  readonly fact_overflow_count: string
  readonly incident_history_eviction_count: string
  readonly console_suppression_count: string
  readonly late_link_eviction_count: string
  readonly trace_dropped_count: string
  readonly trace_overwritten_count: string
  readonly trace_sampled_count: string
  readonly trace_coalesced_count: string
}

export interface FailureIncidentPayloadV2 {
  readonly evidence_details_omitted?: number
  readonly scope: Readonly<{
    scope_kind:
      | 'join'
      | 'browse'
      | 'preview_open'
      | 'preview_seek'
      | 'preview_media'
      | 'projection'
      | 'authority_activation'
      | 'receive'
      | 'lifecycle_action'
      | 'retained_inventory'
      | 'retained_action'
    scope_sequence: string
  }>
  readonly presentation: Readonly<{
    boundary: PresentationBoundary
    outcome: PresentationOutcome
  }>
  readonly build: BuildIdentityV1
  readonly runtime: RuntimeIdentityV1
  readonly trigger: FailureFactV2
  readonly contributors: readonly FailureFactBucketV2[]
  readonly consequences: readonly FailureFactBucketV2[]
  readonly fact_count: string
  readonly overflow_fact_count: string
  readonly context: DiagnosticContextV1
  readonly diagnostics_health_at_seal: DiagnosticsHealthV1
}

export type IncidentRecordV2 = Readonly<
  DiagnosticEventEnvelopeV2<FailureIncidentPayloadV2> & {
    readonly level: 'error'
    readonly event: typeof INCIDENT_RECORD_EVENT
  }
>
