import { V2_PEER_OPERATION_CODE } from '../session/v2-message'

export const V2_CONNECTIVITY_DIAGNOSTIC_SCHEMA_VERSION = 1 as const

export const V2_BROWSER_CONNECTIVITY_ATTEMPT_STAGES = Object.freeze([
  'started',
  'offer-created',
  'offer-sent',
  'answer-received',
  'datachannel-open',
  'lane-granted',
  'lane-attached',
  'admitted',
  'failed',
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

export const V2_ICE_CANDIDATE_TYPES = Object.freeze(['host', 'prflx', 'srflx', 'relay'] as const)
export const V2_ICE_PROTOCOLS = Object.freeze(['udp', 'tcp'] as const)

export type V2BrowserConnectivityAttemptStage =
  (typeof V2_BROWSER_CONNECTIVITY_ATTEMPT_STAGES)[number]
export type V2ConnectivityFailureScope = (typeof V2_CONNECTIVITY_FAILURE_SCOPES)[number]
export type V2TypedPeerErrorCode = (typeof V2_TYPED_PEER_ERROR_CODES)[number]
export type V2IceCandidateType = (typeof V2_ICE_CANDIDATE_TYPES)[number]
export type V2IceProtocol = (typeof V2_ICE_PROTOCOLS)[number]

export const V2_PEER_OPERATION_TYPED_ERRORS = Object.freeze({
  [V2_PEER_OPERATION_CODE.negotiation]: 'peer-negotiation',
  [V2_PEER_OPERATION_CODE.timeout]: 'peer-timeout',
  [V2_PEER_OPERATION_CODE.candidates]: 'peer-candidates',
  [V2_PEER_OPERATION_CODE.admission]: 'peer-admission',
} as const satisfies Readonly<Record<number, V2TypedPeerErrorCode>>)

export const V2_PEER_OPERATION_ERROR_REGISTRY = Object.freeze([
  Object.freeze({
    code: V2_PEER_OPERATION_CODE.negotiation,
    typedErrorCode: 'peer-negotiation',
  }),
  Object.freeze({
    code: V2_PEER_OPERATION_CODE.timeout,
    typedErrorCode: 'peer-timeout',
  }),
  Object.freeze({
    code: V2_PEER_OPERATION_CODE.candidates,
    typedErrorCode: 'peer-candidates',
  }),
  Object.freeze({
    code: V2_PEER_OPERATION_CODE.admission,
    typedErrorCode: 'peer-admission',
  }),
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

export interface V2BrowserIceCandidateDiagnostic {
  readonly candidateId: string
  readonly candidateType: V2IceCandidateType
  readonly protocol: V2IceProtocol
  readonly address?: string
  readonly port?: number
}

export interface V2BrowserSelectedPairDiagnostic {
  readonly candidatePairId: string
  readonly local: V2BrowserIceCandidateDiagnostic
  readonly remote: V2BrowserIceCandidateDiagnostic
}

export interface V2AuthenticatedPeerOperationFailureDiagnostic {
  readonly scope: 'peer'
  readonly code: number
  readonly message: string
}

interface V2BrowserAttemptEnvelope {
  readonly schemaVersion: typeof V2_CONNECTIVITY_DIAGNOSTIC_SCHEMA_VERSION
  readonly sessionId: string
  readonly peerPathId: string
  readonly attemptId: string
  readonly side: 'browser'
  readonly sideSequence: number
  readonly attemptElapsedMs: number
}

interface V2CandidateMilestone {
  readonly candidateCounts: V2CandidateCounts
}

interface V2LaneMilestone extends V2CandidateMilestone {
  readonly lane: V2LaneIdentity
}

interface V2FailureMilestone {
  readonly stage: 'failed'
  readonly failedAtStage: Exclude<V2BrowserConnectivityAttemptStage, 'started' | 'failed'>
  readonly failureScope: V2ConnectivityFailureScope
  readonly typedErrorCode: V2TypedPeerErrorCode
  readonly failureMessage: string
  readonly candidateCounts?: V2CandidateCounts
  readonly lane?: V2LaneIdentity
  readonly selectedPair?: V2BrowserSelectedPairDiagnostic | null
  readonly authenticatedSenderOperationFailure?: V2AuthenticatedPeerOperationFailureDiagnostic
}

type V2BrowserCandidateStage = Extract<
  V2BrowserConnectivityAttemptStage,
  'offer-created' | 'offer-sent' | 'answer-received' | 'datachannel-open'
>

/**
 * A read-only product trace. Observers may persist or display it, but cannot
 * participate in lane admission or authenticated signaling settlement.
 */
export type V2BrowserConnectivityAttemptDiagnostic =
  | (V2BrowserAttemptEnvelope & { readonly stage: 'started' })
  | (V2BrowserAttemptEnvelope & V2CandidateMilestone & {
      readonly stage: V2BrowserCandidateStage
    })
  | (V2BrowserAttemptEnvelope & V2LaneMilestone & {
      readonly stage: 'lane-granted' | 'lane-attached'
    })
  | (V2BrowserAttemptEnvelope & V2LaneMilestone & {
      readonly stage: 'admitted'
      readonly selectedPair: V2BrowserSelectedPairDiagnostic | null
    })
  | (V2BrowserAttemptEnvelope & V2FailureMilestone)

export type V2ConnectivityObserver = (event: V2BrowserConnectivityAttemptDiagnostic) => void
