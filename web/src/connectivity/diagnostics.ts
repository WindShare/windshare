import type { FailureCorrelation } from '../diagnostics/incident/fact'
import { V2_PEER_OPERATION_CODE } from '../session/v2-message'
import type {
  V2PeerAttemptIdentity,
  V2PeerPathIdentity,
  V2ProtocolSessionIdentity,
} from '../session/v2-identities'
import type { V2PeerAttemptFailure, V2PeerFailureDecision } from './v2-peer-failure'

export const V2_BROWSER_CONNECTIVITY_ATTEMPT_STAGES = Object.freeze([
  'started',
  'negotiation-deadline-armed',
  'negotiation-deadline-expired',
  'offer-created',
  'offer-sent',
  'answer-received',
  'datachannel-open',
  'admission-deadline-armed',
  'admission-deadline-expired',
  'grant-requested',
  'grant-received',
  'lane-hello-sent',
  'admission-response-received',
  'admission-response-settled',
  'lane-attached',
  'admitted',
  'failed',
] as const)

export const V2_BROWSER_CONNECTIVITY_RECOVERY_STAGES = Object.freeze([
  'wave-started',
  'retry-decided',
  'backoff-scheduled',
  'attempt-replaced',
  'wave-quiesced',
  'wave-rearmed',
  'peer-detached',
  'session-budget-exhausted',
  'path-stopped',
  'session-stopped',
] as const)

export const V2_CONNECTIVITY_FAILURE_SCOPES = Object.freeze(['attempt', 'session'] as const)

export const V2_TYPED_PEER_ERROR_CODES = Object.freeze([
  'peer-negotiation',
  'peer-timeout',
  'peer-candidates',
  'peer-admission',
  'signaling-contract',
  'attempt-cancelled',
  'runtime-stopped',
  'unexpected',
] as const)

export type V2BrowserConnectivityAttemptStage =
  (typeof V2_BROWSER_CONNECTIVITY_ATTEMPT_STAGES)[number]
export type V2BrowserConnectivityRecoveryStage =
  (typeof V2_BROWSER_CONNECTIVITY_RECOVERY_STAGES)[number]
export type V2ConnectivityFailureScope = (typeof V2_CONNECTIVITY_FAILURE_SCOPES)[number]
export type V2TypedPeerErrorCode = (typeof V2_TYPED_PEER_ERROR_CODES)[number]

export const V2_PEER_OPERATION_TYPED_ERRORS = Object.freeze({
  [V2_PEER_OPERATION_CODE.negotiation]: 'peer-negotiation',
  [V2_PEER_OPERATION_CODE.timeout]: 'peer-timeout',
  [V2_PEER_OPERATION_CODE.candidates]: 'peer-candidates',
  [V2_PEER_OPERATION_CODE.admission]: 'peer-admission',
} as const satisfies Readonly<Record<number, V2TypedPeerErrorCode>>)

export const V2_PEER_OPERATION_ERROR_REGISTRY = Object.freeze([
  Object.freeze({ code: V2_PEER_OPERATION_CODE.negotiation, typedErrorCode: 'peer-negotiation' }),
  Object.freeze({ code: V2_PEER_OPERATION_CODE.timeout, typedErrorCode: 'peer-timeout' }),
  Object.freeze({ code: V2_PEER_OPERATION_CODE.candidates, typedErrorCode: 'peer-candidates' }),
  Object.freeze({ code: V2_PEER_OPERATION_CODE.admission, typedErrorCode: 'peer-admission' }),
] as const)

export function v2TypedErrorForPeerOperationCode(
  code: number,
): V2TypedPeerErrorCode | undefined {
  if (!Object.hasOwn(V2_PEER_OPERATION_TYPED_ERRORS, code)) return undefined
  return V2_PEER_OPERATION_TYPED_ERRORS[
    code as keyof typeof V2_PEER_OPERATION_TYPED_ERRORS
  ]
}

export interface V2CandidateCounts {
  readonly localEmitted: number
  readonly remoteAccepted: number
}

export interface V2LaneIdentity {
  readonly laneId: number
  readonly laneEpoch: number
}

export type V2PeerRecoveryWaveTrigger = 'activation' | 'network-change' | 'detachment'

export interface V2AttemptOrdinalCorrelation {
  readonly waveOrdinal: number
  readonly waveAttemptOrdinal: number
  readonly sessionAttemptOrdinal: number
}

export interface V2PeerAttemptCorrelation extends V2AttemptOrdinalCorrelation {
  readonly protocolSessionId: V2ProtocolSessionIdentity
  readonly peerPathId: V2PeerPathIdentity
  readonly attemptId: V2PeerAttemptIdentity
}

type V2PeerAttemptBase = Readonly<{
  eventName: 'peer_attempt'
  correlation: FailureCorrelation & Readonly<{
    protocolSessionId: V2ProtocolSessionIdentity
    peerPathId: V2PeerPathIdentity
    peerAttemptId: V2PeerAttemptIdentity
  }>
  waveOrdinal: number
  waveAttemptOrdinal: number
  sessionAttemptOrdinal: number
}>

export type V2PeerAttemptTraceEvent = V2PeerAttemptBase & (
  | Readonly<{ stage: 'started' }>
  | Readonly<{
      stage: 'negotiation-deadline-armed' | 'negotiation-deadline-expired'
      phase: 'negotiation'
      deadlineBudgetMilliseconds: number
    }>
  | Readonly<{
      stage: 'offer-created' | 'offer-sent' | 'answer-received' | 'datachannel-open'
      candidateCounts: V2CandidateCounts
    }>
  | Readonly<{
      stage: 'admission-deadline-armed' | 'admission-deadline-expired'
      phase: 'admission'
      deadlineBudgetMilliseconds: number
    }>
  | Readonly<{
      stage: 'grant-requested'
      phase: 'admission'
      requestedLaneId: number
    }>
  | Readonly<{
      stage: 'grant-received' | 'lane-hello-sent' | 'admission-response-received' |
        'lane-attached' | 'admitted'
      phase: 'admission'
    }>
  | Readonly<{
      stage: 'admission-response-settled'
      phase: 'admission'
      settlement:
        | Readonly<{ disposition: 'accepted' }>
        | Readonly<{
            disposition: 'rejected'
            rejectionCode: number
            retryAfterMilliseconds: number
          }>
    }>
  | Readonly<{
      stage: 'failed'
      failedAtStage: Exclude<V2BrowserConnectivityAttemptStage, 'started' | 'failed'>
      failure: V2PeerAttemptFailure
      failureScope: V2ConnectivityFailureScope
      typedErrorCode: V2TypedPeerErrorCode
      retryable: boolean
    }>
)

type V2PeerRecoveryBase = Readonly<{
  eventName: 'peer_recovery'
  correlation: FailureCorrelation & Readonly<{
    protocolSessionId: V2ProtocolSessionIdentity
    peerPathId: V2PeerPathIdentity
  }>
}>

export type V2PeerRecoveryTraceEvent = V2PeerRecoveryBase & (
  | Readonly<{
      stage: 'wave-started'
      waveOrdinal: number
      trigger: V2PeerRecoveryWaveTrigger
    }>
  | Readonly<{
      stage: 'wave-rearmed'
      waveOrdinal: number
      trigger: 'activation' | 'network-change'
    }>
  | Readonly<{
      stage: 'retry-decided'
      waveOrdinal: number
      decision: V2PeerFailureDecision['type']
      reason: V2PeerFailureDecision['reason']
      authenticatedRetryAfterMilliseconds: number
    }>
  | Readonly<{
      stage: 'backoff-scheduled'
      waveOrdinal: number
      retryOrdinal: number
      localDelayMilliseconds: number
      authenticatedRetryAfterMilliseconds: number
      effectiveDelayMilliseconds: number
    }>
  | Readonly<{
      stage: 'attempt-replaced'
      waveOrdinal: number
      previousAttemptId: V2PeerAttemptIdentity
    }>
  | Readonly<{
      stage: 'wave-quiesced'
      waveOrdinal: number
      reason: 'wave-attempt-budget' | 'wave-elapsed-budget'
    }>
  | Readonly<{ stage: 'peer-detached' }>
  | Readonly<{
      stage: 'session-budget-exhausted'
      reason: 'session-attempt-budget' | 'session-elapsed-budget'
    }>
  | Readonly<{
      stage: 'path-stopped'
      reason: Extract<V2PeerFailureDecision, { readonly type: 'stop-path' }>['reason']
    }>
  | Readonly<{
      stage: 'session-stopped'
      reason: Extract<
        V2PeerFailureDecision,
        { readonly type: 'stop-session' }
      >['terminal']['code']
    }>
)

export type V2ConnectivityTraceEvent =
  | V2PeerAttemptTraceEvent
  | V2PeerRecoveryTraceEvent

export type V2ConnectivityTraceObserver = (event: V2ConnectivityTraceEvent) => void

/**
 * Connectivity lifetimes retain this source across attempts and reconnect
 * generations; producers read current before constructing a trace event.
 */
export interface V2ConnectivityTraceSource {
  readonly current: V2ConnectivityTraceObserver | undefined
}
