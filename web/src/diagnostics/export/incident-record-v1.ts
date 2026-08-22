import type {
  FailureFactKind,
  FailureStage,
  NativeFailureClass,
  ProtocolFailureScope,
  ProtocolMessageKindV1,
  RecoveryDisposition,
} from '../incident/fact'
import type {
  PresentationBoundary,
  PresentationOutcome,
} from '../incident/presentation'
import type { CorrelationV1 } from './correlation-v1'
import type { DiagnosticContextV1 } from './context'

export const INCIDENT_RECORD_SCHEMA_VERSION = 1 as const
export const INCIDENT_RECORD_EVENT = 'failure_incident' as const

export interface DiagnosticEventEnvelopeV1<Payload extends object> {
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

export interface ProtocolFailureV1 {
  readonly request_kind: ProtocolMessageKindV1
  readonly wire_scope: ProtocolFailureScope
  readonly wire_code: number
  readonly retryable: boolean
  readonly retry_after_ms?: number
  readonly settlement:
    | Readonly<{ kind: 'received_authenticated' }>
    | Readonly<{
        kind: 'response_send'
        admitted: boolean
        settled: boolean
        outcome: 'unknown' | 'delivered' | 'dropped'
      }>
  readonly correlation: CorrelationV1
}

export type FaultDomainV1 =
  | 'source'
  | 'catalog'
  | 'session'
  | 'output'
  | 'checkpoint'

export type FaultScopeV1 =
  | 'file_local'
  | 'directory_local'
  | 'output_pause'
  | 'session_terminal'

export type FaultCodeV1 =
  | 'unavailable'
  | 'revision_changed'
  | 'revision_invalidated'
  | 'permanent'
  | 'directory_stale'
  | 'invalid_generation'
  | 'transport'
  | 'protocol'
  | 'resource_budget'
  | 'dependency_contract'
  | 'state_io'
  | 'ownership'
  | 'namespace_unsafe'
  | 'unsupported_filesystem'
  | 'directory_binding'
  | 'directory_metadata'
  | 'file_already_active'
  | 'mutation_ambiguous'
  | 'contract'
  | 'busy'
  | 'corrupt_record'
  | 'unsafe_install'
  | 'ownership_mismatch'

export type PeerFailureCodeV1 =
  | 'peer_negotiation'
  | 'peer_timeout'
  | 'peer_candidates'
  | 'peer_admission'
  | 'signaling_contract'
  | 'attempt_cancelled'
  | 'runtime_stopped'
  | 'unexpected'

export type NativeOutputCodeV1 =
  | 'state_io'
  | 'ownership'
  | 'namespace_unsafe'
  | 'unsupported_filesystem'
  | 'directory_binding'
  | 'directory_metadata'
  | 'file_already_active'
  | 'resource_budget'
  | 'mutation_ambiguous'
  | 'contract'
  | 'busy'
  | 'corrupt_record'
  | 'unsafe_install'
  | 'ownership_mismatch'

export type LifecycleStateV1 =
  | 'intent_frozen'
  | 'preparing'
  | 'receiving'
  | 'resumable_receive'
  | 'finalizing_tree'
  | 'committing_atomic'
  | 'materialization_sealed'
  | 'packaging'
  | 'resumable_package'
  | 'artifact_sealed'
  | 'waiting_to_save'
  | 'publishing_managed'
  | 'handing_off'
  | 'published'
  | 'download_started'
  | 'partial_directory'
  | 'restart_required'
  | 'discarded'
  | 'expired'
  | 'needs_attention'
  | 'authorization_required'
  | 'target_verification_required'
  | 'destination_space_required'

export type LifecycleReasonV1 =
  | 'failures'
  | 'stopped'
  | 'direct_atomic_rolled_back'
  | 'portable_aborted'
  | 'source_revision_changed'
  | 'preparation_invalidated'
  | 'content_session_ended'
  | 'target_deleted'
  | 'target_ownership_unknown'
  | 'publication_unknown'
  | 'cleanup_unknown'

interface FailureFactEnvelopeV1<
  Kind extends FailureFactKind,
  Payload extends object,
> {
  readonly kind: Kind
  readonly stage: FailureStage
  readonly recovery_disposition: RecoveryDisposition
  readonly correlation?: CorrelationV1
  readonly payload: Payload
}

export type FailureFactV1 =
  | Readonly<FailureFactEnvelopeV1<'fault', Readonly<{
      fault: Readonly<{
        domain: FaultDomainV1
        scope: FaultScopeV1
        code: FaultCodeV1
      }>
    }>>>
  | Readonly<FailureFactEnvelopeV1<'protocol_failure', Readonly<{
      protocol_failure: ProtocolFailureV1
    }>>>
  | Readonly<FailureFactEnvelopeV1<'peer_failure', Readonly<{
      peer_failure: Readonly<{
        scope: 'attempt' | 'session'
        code: PeerFailureCodeV1
        retryable: boolean
      }>
    }>>>
  | Readonly<FailureFactEnvelopeV1<'native_output_failure', Readonly<{
      native_output_failure: Readonly<{
        native_class: NativeFailureClass
        code?: NativeOutputCodeV1
      }>
    }>>>
  | Readonly<FailureFactEnvelopeV1<'lifecycle_failure', Readonly<{
      lifecycle_failure: Readonly<{
        state: LifecycleStateV1
        reason?: LifecycleReasonV1
      }>
    }>>>
  | Readonly<FailureFactEnvelopeV1<'unclassified', Readonly<{
      unclassified: Readonly<Record<never, never>>
    }>>>

export interface FailureFactBucketV1 {
  readonly fingerprint: string
  readonly count: string
  readonly representative: FailureFactV1
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

export interface FailureIncidentPayloadV1 {
  readonly root_incident_sequence?: string
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
  readonly trigger: FailureFactV1
  readonly contributors: readonly FailureFactBucketV1[]
  readonly consequences: readonly FailureFactBucketV1[]
  readonly fact_count: string
  readonly overflow_fact_count: string
  readonly context: DiagnosticContextV1
  readonly diagnostics_health_at_seal: DiagnosticsHealthV1
}

export type IncidentRecordV1 = Readonly<
  DiagnosticEventEnvelopeV1<FailureIncidentPayloadV1> & {
    readonly level: 'error'
    readonly event: typeof INCIDENT_RECORD_EVENT
  }
>
