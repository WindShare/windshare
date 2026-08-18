import { V2_PEER_OPERATION_CODE } from '../session/v2-message'
import type { V2PeerAttemptFailure, V2PeerFailureDecision } from './v2-peer-failure'

export const V2_CONNECTIVITY_DIAGNOSTIC_SCHEMA_VERSION = 2 as const

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

export type V2StableBrowserConnectivityAttemptStage =
  (typeof V2_BROWSER_CONNECTIVITY_ATTEMPT_STAGES)[number]
// The diagnostic error classifier is retained only as a local error-reporting helper.
// Product lifecycle events below accept exclusively the stable v2 vocabulary.
export type V2BrowserConnectivityAttemptStage =
  | V2StableBrowserConnectivityAttemptStage
  | 'lane-granted'
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

// Test fakes outside the lifecycle boundary still name their opaque input. The
// v2 event union deliberately has no field capable of retaining this value.
export type V2BrowserSelectedPairDiagnostic = Readonly<Record<string, unknown>>

export interface V2AuthenticatedPeerOperationFailureDiagnostic {
  readonly scope: 'peer'
  readonly code: number
  readonly message: string
}

export interface V2AttemptOrdinalCorrelation {
  readonly waveOrdinal: number
  readonly waveAttemptOrdinal: number
  readonly sessionAttemptOrdinal: number
}

interface V2BrowserAttemptEnvelope extends V2AttemptOrdinalCorrelation {
  readonly schemaVersion: typeof V2_CONNECTIVITY_DIAGNOSTIC_SCHEMA_VERSION
  readonly stream: 'attempt'
  readonly sessionId: string
  readonly peerPathId: string
  readonly attemptId: string
  readonly side: 'browser'
  readonly sideSequence: number
  readonly attemptElapsedMs: number
}

interface V2BrowserOfferMilestone {
  readonly candidateCounts: V2CandidateCounts
  readonly offerOperationId?: string
}

interface V2BrowserGrantMilestone {
  readonly phase: 'admission'
  readonly grantOperationId: string
  readonly lane: V2LaneIdentity
}

export type V2BrowserConnectivityAttemptDiagnostic = V2BrowserAttemptEnvelope & (
  | { readonly stage: 'started' }
  | {
      readonly stage: 'negotiation-deadline-armed' | 'negotiation-deadline-expired'
      readonly phase: 'negotiation'
      readonly deadlineBudgetMs: number
    }
  | (V2BrowserOfferMilestone & {
      readonly stage: 'offer-created' | 'offer-sent' | 'answer-received' | 'datachannel-open'
    })
  | {
      readonly stage: 'admission-deadline-armed' | 'admission-deadline-expired'
      readonly phase: 'admission'
      readonly deadlineBudgetMs: number
      readonly offerOperationId: string
    }
  | {
      readonly stage: 'grant-requested'
      readonly phase: 'admission'
      readonly offerOperationId: string
      readonly grantOperationId: string
      readonly requestedLaneId: number
    }
  | (V2BrowserGrantMilestone & {
      readonly stage: 'grant-received' | 'lane-hello-sent' | 'admission-response-received' |
        'lane-attached' | 'admitted'
      readonly offerOperationId: string
    })
  | (V2BrowserGrantMilestone & {
      readonly stage: 'admission-response-settled'
      readonly offerOperationId: string
      readonly settlement:
        | { readonly disposition: 'accepted' }
        | {
            readonly disposition: 'rejected'
            readonly rejection: {
              readonly code: number
              readonly retryAfterMilliseconds: number
            }
          }
    })
  | {
      readonly stage: 'failed'
      readonly failedAtStage: Exclude<V2StableBrowserConnectivityAttemptStage, 'started' | 'failed'>
      readonly failure: V2PeerAttemptFailure
      readonly failureScope: 'attempt' | 'session'
      readonly typedErrorCode: V2TypedPeerErrorCode
      readonly offerOperationId?: string
      readonly grantOperationId?: string
      readonly lane?: V2LaneIdentity
    }
)

interface V2BrowserRecoveryEnvelope {
  readonly schemaVersion: typeof V2_CONNECTIVITY_DIAGNOSTIC_SCHEMA_VERSION
  readonly stream: 'recovery'
  readonly sessionId: string
  readonly peerPathId: string
  readonly side: 'browser'
  readonly sideSequence: number
  readonly sessionRecoveryElapsedMs: number
}

export type V2PeerRecoveryWaveTrigger = 'activation' | 'network-change' | 'detachment'

export type V2BrowserConnectivityRecoveryDiagnostic = V2BrowserRecoveryEnvelope & (
  | {
      readonly stage: 'wave-started'
      readonly waveOrdinal: number
      readonly trigger: V2PeerRecoveryWaveTrigger
    }
  | {
      readonly stage: 'wave-rearmed'
      readonly waveOrdinal: number
      readonly trigger: 'activation' | 'network-change'
    }
  | {
      readonly stage: 'retry-decided'
      readonly waveOrdinal: number
      readonly attemptId: string
      readonly decision: V2PeerFailureDecision['type']
      readonly reason: V2PeerFailureDecision['reason']
      readonly authenticatedRetryAfterMilliseconds: number
    }
  | {
      readonly stage: 'backoff-scheduled'
      readonly waveOrdinal: number
      readonly attemptId: string
      readonly retryOrdinal: number
      readonly localDelayMilliseconds: number
      readonly authenticatedRetryAfterMilliseconds: number
      readonly effectiveDelayMilliseconds: number
    }
  | {
      readonly stage: 'attempt-replaced'
      readonly waveOrdinal: number
      readonly previousAttemptId: string
      readonly attemptId: string
    }
  | {
      readonly stage: 'wave-quiesced'
      readonly waveOrdinal: number
      readonly reason: 'wave-attempt-budget' | 'wave-elapsed-budget'
    }
  | { readonly stage: 'peer-detached'; readonly lane: V2LaneIdentity }
  | {
      readonly stage: 'session-budget-exhausted'
      readonly reason: 'session-attempt-budget' | 'session-elapsed-budget'
    }
  | {
      readonly stage: 'path-stopped'
      readonly reason: Extract<V2PeerFailureDecision, { readonly type: 'stop-path' }>['reason']
    }
  | {
      readonly stage: 'session-stopped'
      readonly reason: Extract<
        V2PeerFailureDecision,
        { readonly type: 'stop-session' }
      >['terminal']['code']
    }
)

export type V2ConnectivityDiagnostic =
  | V2BrowserConnectivityAttemptDiagnostic
  | V2BrowserConnectivityRecoveryDiagnostic

export type V2ConnectivityObserver = (event: V2BrowserConnectivityAttemptDiagnostic) => void
