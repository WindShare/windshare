import {
  V2_LANE_MAX_RETRY_AFTER_MILLISECONDS,
  V2_LANE_REJECT,
  type V2LaneRejection,
} from '../session/v2-lane-codec'
import { peerFailureScope } from '../session/v2-message'

export type V2PeerAttemptPhase = 'negotiation' | 'admission'

export type V2PeerLocalTransientReason =
  | 'tab-capacity-timeout'
  | 'negotiation-timeout'
  | 'admission-timeout'
  | 'transport-loss'
  | 'signaling-delivery-loss'
  | 'lane-installation-failed'

export type V2PeerLocalPolicyCode =
  | 'unsupported-capability'
  | 'candidate-limit'
  | 'unexpected-data-channel'
  | 'configured-policy-refusal'

export type V2PeerLocalContractCode =
  | 'invalid-adapter-result'
  | 'invalid-lane-response'
  | 'invalid-proof'
  | 'invalid-signature'
  | 'identity-mismatch'
  | 'unknown-local-failure'

export type V2ProtocolSessionTerminalCode =
  | 'runtime-closed'
  | 'generation-retired'
  | 'binding-conflict'
  | 'continuation-conflict'
  | 'protocol-failure'

/**
 * The runtime creates this snapshot after it has already sealed terminal session
 * authority. Recovery can reflect that decision, but has no callback with which
 * to close or otherwise promote the ProtocolSession itself.
 */
export interface V2ProtocolSessionTerminalSnapshot {
  readonly authority: 'protocol-session-terminal'
  readonly code: V2ProtocolSessionTerminalCode
}

export type V2PeerAttemptFailure =
  | {
      readonly kind: 'local-transient'
      readonly phase: V2PeerAttemptPhase
      readonly reason: V2PeerLocalTransientReason
    }
  | {
      readonly kind: 'local-policy'
      readonly code: V2PeerLocalPolicyCode
    }
  | {
      readonly kind: 'local-contract'
      readonly code: V2PeerLocalContractCode
    }
  | {
      readonly kind: 'authenticated-peer-operation'
      readonly code: number
    }
  | {
      readonly kind: 'authenticated-lane-rejection'
      readonly rejection: V2LaneRejection
    }
  | {
      readonly kind: 'session-terminal'
      readonly terminal: V2ProtocolSessionTerminalSnapshot
    }

export interface V2PeerAdmittedLane {
  readonly laneId: number
  readonly laneEpoch: number
}

export type V2PeerAttemptCancellationOwner =
  | 'last-activation'
  | 'generation-replaced'
  | 'runtime-stop'
  | 'recovery-budget'

export type V2PeerAttemptResult =
  | {
      readonly type: 'admitted'
      readonly lane: V2PeerAdmittedLane
    }
  | {
      readonly type: 'failed'
      readonly failure: V2PeerAttemptFailure
    }
  | {
      readonly type: 'lifecycle-cancelled'
      readonly owner: V2PeerAttemptCancellationOwner
    }

export type V2PeerFailureDecision =
  | {
      readonly type: 'retry-attempt'
      readonly reason: 'local-transient' | 'grant-expired'
    }
  | {
      readonly type: 'retry-attempt'
      readonly reason: 'admission-limited'
      readonly authenticatedRetryAfterMilliseconds: number
    }
  | {
      readonly type: 'stop-path'
      readonly reason:
        | 'local-policy'
        | 'local-contract'
        | 'peer-operation-final'
        | 'lane-rejection-final'
        | 'untyped-failure'
    }
  | {
      readonly type: 'stop-session'
      readonly reason: 'session-terminal'
      readonly terminal: V2ProtocolSessionTerminalSnapshot
    }

/**
 * Classification is deliberately structural and closed. Unknown values fail at
 * path scope; error text and nested causes can therefore never mint retry or
 * session authority.
 */
export function classifyV2PeerAttemptFailure(failure: unknown): V2PeerFailureDecision {
  if (!isRecord(failure) || typeof failure.kind !== 'string') {
    return stopUntypedFailure()
  }

  switch (failure.kind) {
    case 'local-transient':
      return isLocalTransient(failure)
        ? Object.freeze({ type: 'retry-attempt', reason: 'local-transient' })
        : stopUntypedFailure()
    case 'local-policy':
      return isLocalPolicy(failure)
        ? Object.freeze({ type: 'stop-path', reason: 'local-policy' })
        : stopUntypedFailure()
    case 'local-contract':
      return isLocalContract(failure)
        ? Object.freeze({ type: 'stop-path', reason: 'local-contract' })
        : stopUntypedFailure()
    case 'authenticated-peer-operation':
      if (!isAuthenticatedPeerOperation(failure)) return stopUntypedFailure()
      return peerFailureScope(failure.code as number) === 'attempt-transient'
        ? Object.freeze({ type: 'retry-attempt', reason: 'local-transient' })
        : Object.freeze({ type: 'stop-path', reason: 'peer-operation-final' })
    case 'authenticated-lane-rejection':
      return classifyLaneRejection(failure)
    case 'session-terminal':
      return isSessionTerminal(failure)
        ? Object.freeze({
            type: 'stop-session',
            reason: 'session-terminal',
            terminal: failure.terminal,
          })
        : stopUntypedFailure()
    default:
      return stopUntypedFailure()
  }
}

function classifyLaneRejection(failure: Record<string, unknown>): V2PeerFailureDecision {
  if (!isV2LaneRejection(failure.rejection)) return stopUntypedFailure()
  if (failure.rejection.code === V2_LANE_REJECT.grantExpired) {
    return Object.freeze({ type: 'retry-attempt', reason: 'grant-expired' })
  }
  if (failure.rejection.code === V2_LANE_REJECT.admissionLimited) {
    return Object.freeze({
      type: 'retry-attempt',
      reason: 'admission-limited',
      authenticatedRetryAfterMilliseconds: failure.rejection.retryAfterMilliseconds,
    })
  }
  return Object.freeze({ type: 'stop-path', reason: 'lane-rejection-final' })
}

function isLocalTransient(value: Record<string, unknown>): boolean {
  if (!isOneOf(value.phase, ['negotiation', 'admission'])) return false
  if (!isOneOf(value.reason, [
    'tab-capacity-timeout',
    'negotiation-timeout',
    'admission-timeout',
    'transport-loss',
    'signaling-delivery-loss',
    'lane-installation-failed',
  ])) return false
  if (value.reason === 'negotiation-timeout' || value.reason === 'tab-capacity-timeout') return value.phase === 'negotiation'
  if (value.reason === 'admission-timeout' || value.reason === 'lane-installation-failed') {
    return value.phase === 'admission'
  }
  return true
}

function isLocalPolicy(value: Record<string, unknown>): boolean {
  return isOneOf(value.code, [
    'unsupported-capability',
    'candidate-limit',
    'unexpected-data-channel',
    'configured-policy-refusal',
  ])
}

function isLocalContract(value: Record<string, unknown>): boolean {
  return isOneOf(value.code, [
    'invalid-adapter-result',
    'invalid-lane-response',
    'invalid-proof',
    'invalid-signature',
    'identity-mismatch',
    'unknown-local-failure',
  ])
}

function isAuthenticatedPeerOperation(value: Record<string, unknown>): boolean {
  return isUint32(value.code)
}

function isSessionTerminal(value: Record<string, unknown>): value is {
  readonly terminal: V2ProtocolSessionTerminalSnapshot
} & Record<string, unknown> {
  if (!isRecord(value.terminal) || value.terminal.authority !== 'protocol-session-terminal') {
    return false
  }
  return isOneOf(value.terminal.code, [
    'runtime-closed',
    'generation-retired',
    'binding-conflict',
    'continuation-conflict',
    'protocol-failure',
  ])
}

export function isV2LaneRejection(value: unknown): value is V2LaneRejection {
  if (!isRecord(value) || !isOneOf(value.code, Object.values(V2_LANE_REJECT))) return false
  if (
    !Number.isSafeInteger(value.retryAfterMilliseconds) ||
    (value.retryAfterMilliseconds as number) < 0 ||
    (value.retryAfterMilliseconds as number) > V2_LANE_MAX_RETRY_AFTER_MILLISECONDS
  ) return false
  return value.code === V2_LANE_REJECT.admissionLimited ||
    value.retryAfterMilliseconds === 0
}

function isUint32(value: unknown): value is number {
  return Number.isSafeInteger(value) && (value as number) >= 0 && (value as number) <= 0xffff_ffff
}

function isOneOf<T>(value: unknown, candidates: readonly T[]): value is T {
  return candidates.some((candidate) => candidate === value)
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

function stopUntypedFailure(): V2PeerFailureDecision {
  return Object.freeze({ type: 'stop-path', reason: 'untyped-failure' })
}
