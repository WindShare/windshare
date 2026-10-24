import type { FailureCorrelation } from '../diagnostics/incident/failure-correlation'
import type { PeerProviderFact } from './peer-set/provider-facts'
import { PEER_OPERATION_CODE } from '../session/message'
import type {
  PeerAttemptIdentity,
  PeerPathIdentity,
  ProtocolSessionIdentity,
} from '../session/identities'
import type { PeerAttemptFailure, PeerFailureDecision } from './peer-failure'

export const BROWSER_CONNECTIVITY_ATTEMPT_STAGES = Object.freeze([
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

export const BROWSER_CONNECTIVITY_RECOVERY_STAGES = Object.freeze([
  'parallelism',
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

export const CONNECTIVITY_FAILURE_SCOPES = Object.freeze(['attempt-transient', 'resource-deferred', 'path-terminal', 'session-terminal'] as const)

export const TYPED_PEER_ERROR_CODES = Object.freeze([
  'peer-negotiation',
  'peer-timeout',
  'peer-candidates',
  'peer-admission',
  'peer-busy',
  'signaling-contract',
  'attempt-cancelled',
  'runtime-stopped',
  'unexpected',
] as const)

export type BrowserConnectivityAttemptStage =
  (typeof BROWSER_CONNECTIVITY_ATTEMPT_STAGES)[number]
export type BrowserConnectivityRecoveryStage =
  (typeof BROWSER_CONNECTIVITY_RECOVERY_STAGES)[number]
export type ConnectivityFailureScope = (typeof CONNECTIVITY_FAILURE_SCOPES)[number]
export type TypedPeerErrorCode = (typeof TYPED_PEER_ERROR_CODES)[number]

export const PEER_OPERATION_TYPED_ERRORS = Object.freeze({
  [PEER_OPERATION_CODE.negotiation]: 'peer-negotiation',
  [PEER_OPERATION_CODE.timeout]: 'peer-timeout',
  [PEER_OPERATION_CODE.candidates]: 'peer-candidates',
  [PEER_OPERATION_CODE.admission]: 'peer-admission',
  [PEER_OPERATION_CODE.capacity]: 'peer-busy',
} as const satisfies Readonly<Record<number, TypedPeerErrorCode>>)

export const PEER_OPERATION_ERROR_REGISTRY = Object.freeze([
  Object.freeze({ code: PEER_OPERATION_CODE.negotiation, typedErrorCode: 'peer-negotiation' }),
  Object.freeze({ code: PEER_OPERATION_CODE.timeout, typedErrorCode: 'peer-timeout' }),
  Object.freeze({ code: PEER_OPERATION_CODE.candidates, typedErrorCode: 'peer-candidates' }),
  Object.freeze({ code: PEER_OPERATION_CODE.admission, typedErrorCode: 'peer-admission' }),
  Object.freeze({ code: PEER_OPERATION_CODE.capacity, typedErrorCode: 'peer-busy' }),
] as const)

export function typedErrorForPeerOperationCode(
  code: number,
): TypedPeerErrorCode | undefined {
  if (!Object.hasOwn(PEER_OPERATION_TYPED_ERRORS, code)) return undefined
  return PEER_OPERATION_TYPED_ERRORS[
    code as keyof typeof PEER_OPERATION_TYPED_ERRORS
  ]
}

export interface CandidateCounts {
  readonly localEmitted: number
  readonly remoteAccepted: number
}

export interface LaneIdentity {
  readonly laneId: number
  readonly laneEpoch: number
}

export type PeerRecoveryWaveTrigger = 'activation' | 'network-change' | 'detachment'

export interface AttemptOrdinalCorrelation {
  readonly waveOrdinal: number
  readonly waveAttemptOrdinal: number
  readonly sessionAttemptOrdinal: number
}

export interface PeerAttemptCorrelation extends AttemptOrdinalCorrelation {
  readonly protocolSessionId: ProtocolSessionIdentity
  readonly peerPathId: PeerPathIdentity
  readonly attemptId: PeerAttemptIdentity
}

export interface PeerAttemptSummary {
  readonly lastCompletedStage: BrowserConnectivityAttemptStage
  readonly attemptElapsedMilliseconds: number
  readonly stageElapsedMilliseconds: number
  readonly deadlineExpired: boolean
}

type PeerAttemptBase = Readonly<{
  summary?: PeerAttemptSummary
  eventName: 'peer_attempt'
  correlation: FailureCorrelation & Readonly<{
    protocolSessionId: ProtocolSessionIdentity
    peerPathId: PeerPathIdentity
    peerAttemptId: PeerAttemptIdentity
  }>
  waveOrdinal: number
  waveAttemptOrdinal: number
  sessionAttemptOrdinal: number
}>

export type PeerAttemptTraceEvent = PeerAttemptBase & (
  | Readonly<{ stage: 'provider-fact'; fact: PeerProviderFact }>
  | Readonly<{ stage: 'started' }>
  | Readonly<{
      stage: 'negotiation-deadline-armed' | 'negotiation-deadline-expired'
      phase: 'negotiation'
      deadlineBudgetMilliseconds: number
    }>
  | Readonly<{
      stage: 'offer-created' | 'offer-sent' | 'answer-received' | 'datachannel-open'
      candidateCounts: CandidateCounts
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
      failedAtStage: Exclude<BrowserConnectivityAttemptStage, 'started' | 'failed'>
      failure: PeerAttemptFailure
      failureScope: ConnectivityFailureScope
      typedErrorCode: TypedPeerErrorCode
      retryable: boolean
    }>
)

type PeerRecoveryBase = Readonly<{
  eventName: 'peer_recovery'
  correlation: FailureCorrelation & Readonly<{
    protocolSessionId: ProtocolSessionIdentity
    peerPathId: PeerPathIdentity
  }>
}>

export type PeerRecoveryTraceEvent = PeerRecoveryBase & (
  | Readonly<{ stage: 'parallelism'; action: string; reason: string;
      networkWaitingBytes: number; inFlightBytes: number; remainingBytes: number; remainingKnown: boolean;
      paths: number; queuedBytes: number; unfinishedBytes: number; capacityWaitingBytes: number;
      usefulRate: number; outputRate: number; baselineUsefulRate: number; baselineOutputRate: number;
      trialPath?: Readonly<{ pathId: string; laneId: number; laneEpoch: number }>;
      trialWork?: import('../content/scheduling/sampling').SamplingEvidence }>
  | Readonly<{
      stage: 'wave-started'
      waveOrdinal: number
      trigger: PeerRecoveryWaveTrigger
    }>
  | Readonly<{
      stage: 'wave-rearmed'
      waveOrdinal: number
      trigger: 'activation' | 'network-change'
    }>
  | Readonly<{
      stage: 'retry-decided'
      waveOrdinal: number
      decision: PeerFailureDecision['type']
      reason: PeerFailureDecision['reason']
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
      previousAttemptId: PeerAttemptIdentity
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
      reason: Extract<PeerFailureDecision, { readonly type: 'stop-path' }>['reason']
    }>
  | Readonly<{
      stage: 'session-stopped'
      reason: Extract<
        PeerFailureDecision,
        { readonly type: 'stop-session' }
      >['terminal']['code']
    }>
)

export type ConnectivityTraceEvent =
  | PeerAttemptTraceEvent
  | PeerRecoveryTraceEvent

export type ConnectivityTraceObserver = (event: ConnectivityTraceEvent) => void

/**
 * Connectivity lifetimes retain this source across attempts and reconnect
 * generations; producers read current before constructing a trace event.
 */
export interface ConnectivityTraceSource {
  readonly current: ConnectivityTraceObserver | undefined
}
