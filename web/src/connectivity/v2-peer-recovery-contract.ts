import { browserConnectivityClock } from './clock'
import type {
  V2BrowserConnectivityRecoveryDiagnostic,
  V2PeerRecoveryWaveTrigger,
} from './diagnostics'
import type { V2PeerAttemptObserver } from './v2-peer-attempt'
import type {
  V2PeerAdmittedLane,
  V2PeerAttemptCancellationOwner,
  V2PeerAttemptPhase,
  V2PeerAttemptResult,
  V2PeerFailureDecision,
  V2ProtocolSessionTerminalSnapshot,
} from './v2-peer-failure'

export const V2_LANE_GRANT_TTL_MILLISECONDS = 30_000
export const V2_MINIMUM_LANE_GRANT_COMPLETION_MARGIN_MILLISECONDS = 5_000
export const V2_MAXIMUM_PEER_ADMISSION_BUDGET_MILLISECONDS =
  V2_LANE_GRANT_TTL_MILLISECONDS - V2_MINIMUM_LANE_GRANT_COMPLETION_MARGIN_MILLISECONDS
export const V2_DEFAULT_PEER_NEGOTIATION_BUDGET_MILLISECONDS = 15_000
export const V2_DEFAULT_PEER_ADMISSION_BUDGET_MILLISECONDS = 20_000
export const V2_PEER_RECOVERY_WAVE_MAX_ATTEMPTS = 3
export const V2_PEER_RECOVERY_WAVE_ELAPSED_BUDGET_MILLISECONDS = 120_000
export const V2_PEER_RECOVERY_SESSION_MAX_ATTEMPTS = 8
export const V2_PEER_RECOVERY_SESSION_ACTIVE_ELAPSED_BUDGET_MILLISECONDS = 360_000
export const V2_PEER_RETRY_INITIAL_BACKOFF_MILLISECONDS = 1_000
export const V2_PEER_RETRY_BACKOFF_MULTIPLIER = 2
export const V2_PEER_RETRY_BACKOFF_MAXIMUM_MILLISECONDS = 8_000
export const V2_PEER_RETRY_JITTER_MINIMUM_FACTOR = 0.5
export const V2_PEER_RETRY_JITTER_MAXIMUM_FACTOR = 1

export interface V2PeerRecoveryPolicy {
  readonly negotiationBudgetMilliseconds: number
  readonly admissionBudgetMilliseconds: number
  readonly waveMaxAttempts: number
  readonly waveElapsedBudgetMilliseconds: number
  readonly sessionMaxAttempts: number
  readonly sessionActiveElapsedBudgetMilliseconds: number
  readonly retryInitialBackoffMilliseconds: number
  readonly retryBackoffMultiplier: number
  readonly retryBackoffMaximumMilliseconds: number
  readonly retryJitterMinimumFactor: number
  readonly retryJitterMaximumFactor: number
}

export const V2_DEFAULT_PEER_RECOVERY_POLICY: V2PeerRecoveryPolicy = Object.freeze({
  negotiationBudgetMilliseconds: V2_DEFAULT_PEER_NEGOTIATION_BUDGET_MILLISECONDS,
  admissionBudgetMilliseconds: V2_DEFAULT_PEER_ADMISSION_BUDGET_MILLISECONDS,
  waveMaxAttempts: V2_PEER_RECOVERY_WAVE_MAX_ATTEMPTS,
  waveElapsedBudgetMilliseconds: V2_PEER_RECOVERY_WAVE_ELAPSED_BUDGET_MILLISECONDS,
  sessionMaxAttempts: V2_PEER_RECOVERY_SESSION_MAX_ATTEMPTS,
  sessionActiveElapsedBudgetMilliseconds:
    V2_PEER_RECOVERY_SESSION_ACTIVE_ELAPSED_BUDGET_MILLISECONDS,
  retryInitialBackoffMilliseconds: V2_PEER_RETRY_INITIAL_BACKOFF_MILLISECONDS,
  retryBackoffMultiplier: V2_PEER_RETRY_BACKOFF_MULTIPLIER,
  retryBackoffMaximumMilliseconds: V2_PEER_RETRY_BACKOFF_MAXIMUM_MILLISECONDS,
  retryJitterMinimumFactor: V2_PEER_RETRY_JITTER_MINIMUM_FACTOR,
  retryJitterMaximumFactor: V2_PEER_RETRY_JITTER_MAXIMUM_FACTOR,
})

export function createV2PeerRecoveryPolicy(
  overrides: Partial<V2PeerRecoveryPolicy> = {},
): V2PeerRecoveryPolicy {
  const policy = Object.freeze({ ...V2_DEFAULT_PEER_RECOVERY_POLICY, ...overrides })
  validateV2PeerRecoveryPolicy(policy)
  return policy
}

export function validateV2PeerRecoveryPolicy(policy: V2PeerRecoveryPolicy): void {
  requirePositiveMilliseconds(policy.negotiationBudgetMilliseconds, 'negotiation budget')
  requirePositiveMilliseconds(policy.admissionBudgetMilliseconds, 'admission budget')
  if (policy.admissionBudgetMilliseconds > V2_MAXIMUM_PEER_ADMISSION_BUDGET_MILLISECONDS) {
    throw new RangeError('admission budget exceeds the lane-grant completion margin')
  }
  requirePositiveInteger(policy.waveMaxAttempts, 'wave attempt budget')
  requirePositiveInteger(policy.sessionMaxAttempts, 'session attempt budget')
  if (policy.waveMaxAttempts > policy.sessionMaxAttempts) {
    throw new RangeError('wave attempt budget exceeds the session attempt budget')
  }
  requirePositiveMilliseconds(policy.waveElapsedBudgetMilliseconds, 'wave elapsed budget')
  requirePositiveMilliseconds(
    policy.sessionActiveElapsedBudgetMilliseconds,
    'session active elapsed budget',
  )
  if (policy.waveElapsedBudgetMilliseconds > policy.sessionActiveElapsedBudgetMilliseconds) {
    throw new RangeError('wave elapsed budget exceeds the session active elapsed budget')
  }
  requirePositiveMilliseconds(policy.retryInitialBackoffMilliseconds, 'initial retry backoff')
  requirePositiveMilliseconds(policy.retryBackoffMaximumMilliseconds, 'maximum retry backoff')
  if (policy.retryInitialBackoffMilliseconds > policy.retryBackoffMaximumMilliseconds) {
    throw new RangeError('initial retry backoff exceeds the maximum retry backoff')
  }
  if (!Number.isFinite(policy.retryBackoffMultiplier) || policy.retryBackoffMultiplier < 1) {
    throw new RangeError('retry backoff multiplier must be finite and at least one')
  }
  requireJitterRange(policy.retryJitterMinimumFactor, policy.retryJitterMaximumFactor)
}

export function calculateV2PeerRetryDelay(
  policy: V2PeerRecoveryPolicy,
  retryOrdinal: number,
  unitIntervalSample: number,
  authenticatedRetryAfterMilliseconds = 0,
): number {
  if (!Number.isSafeInteger(retryOrdinal) || retryOrdinal < 0) {
    throw new RangeError('retry ordinal must be a non-negative safe integer')
  }
  if (!Number.isFinite(unitIntervalSample) || unitIntervalSample < 0 || unitIntervalSample > 1) {
    throw new RangeError('retry jitter sample must be in the closed unit interval')
  }
  requireNonNegativeMilliseconds(
    authenticatedRetryAfterMilliseconds,
    'authenticated retry-after',
  )
  if (authenticatedRetryAfterMilliseconds > V2_LANE_GRANT_TTL_MILLISECONDS) {
    throw new RangeError('authenticated retry-after exceeds the protocol maximum')
  }
  validateV2PeerRecoveryPolicy(policy)
  const exponential = policy.retryInitialBackoffMilliseconds *
    policy.retryBackoffMultiplier ** retryOrdinal
  const unjittered = Math.min(policy.retryBackoffMaximumMilliseconds, exponential)
  const factor = policy.retryJitterMinimumFactor +
    unitIntervalSample * (policy.retryJitterMaximumFactor - policy.retryJitterMinimumFactor)
  return Math.max(unjittered * factor, authenticatedRetryAfterMilliseconds)
}

export interface V2PeerRecoveryClock {
  now(): number
  sleep(milliseconds: number, signal: AbortSignal): Promise<void>
}

export const browserV2PeerRecoveryClock: V2PeerRecoveryClock = Object.freeze({
  now: () => performance.now(),
  // Browser timers operate at millisecond granularity; rounding upward prevents
  // RetryAfter and budget timers from firing before their monotonic deadline.
  sleep: (milliseconds: number, signal: AbortSignal) =>
    browserConnectivityClock.sleep(Math.ceil(milliseconds), signal),
})

export interface V2PeerAttemptContext {
  readonly protocolSessionId: string
  readonly peerPathId: string
  readonly waveOrdinal: number
  readonly waveAttemptOrdinal: number
  readonly sessionAttemptOrdinal: number
  readonly requestedLaneId: number
  readonly negotiationBudgetMilliseconds: number
  readonly admissionBudgetMilliseconds: number
  phaseChanged(phase: V2PeerAttemptPhase): void
}

export interface V2PeerAttemptHandle {
  readonly attemptId: string
  readonly result: Promise<V2PeerAttemptResult>
  cancel(owner: V2PeerAttemptCancellationOwner): void
}

export interface V2PeerRecoveryAttemptFactory {
  createAttempt(context: V2PeerAttemptContext): V2PeerAttemptHandle
}

export interface V2PeerRecoveryRearmSource {
  subscribe(listener: () => void): () => void
}

export interface V2PeerRecoveryActivation {
  close(): void
}

export interface V2PeerLaneDetachment {
  readonly protocolSessionId: string
  readonly peerPathId: string
  readonly laneId: number
  readonly laneEpoch: number
}

export type V2PeerRecoveryState =
  | { readonly kind: 'idle' }
  | {
      readonly kind: 'attempting'
      readonly phase: V2PeerAttemptPhase
      readonly waveOrdinal: number
      readonly waveAttemptOrdinal: number
      readonly sessionAttemptOrdinal: number
    }
  | {
      readonly kind: 'waiting-retry'
      readonly waveOrdinal: number
      readonly retryOrdinal: number
      readonly delayMilliseconds: number
    }
  | { readonly kind: 'admitted'; readonly lane: V2PeerAdmittedLane }
  | {
      readonly kind: 'quiescent'
      readonly reason: 'wave-attempt-budget' | 'wave-elapsed-budget'
    }
  | {
      readonly kind: 'path-stopped'
      readonly reason: Extract<V2PeerFailureDecision, { readonly type: 'stop-path' }>['reason']
    }
  | {
      readonly kind: 'session-exhausted'
      readonly reason: 'session-attempt-budget' | 'session-elapsed-budget'
    }
  | { readonly kind: 'session-stopped'; readonly reason: V2ProtocolSessionTerminalSnapshot['code'] }

export type V2PeerRecoveryEventPayload =
  | { readonly stage: 'wave-started'; readonly waveOrdinal: number; readonly trigger: V2PeerRecoveryWaveTrigger }
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
  | {
      readonly stage: 'peer-detached'
      readonly lane: { readonly laneId: number; readonly laneEpoch: number }
    }
  | {
      readonly stage: 'session-budget-exhausted'
      readonly reason: 'session-attempt-budget' | 'session-elapsed-budget'
    }
  | {
      readonly stage: 'path-stopped'
      readonly reason: Extract<V2PeerFailureDecision, { readonly type: 'stop-path' }>['reason']
    }
  | { readonly stage: 'session-stopped'; readonly reason: V2ProtocolSessionTerminalSnapshot['code'] }

export type V2PeerRecoveryEvent = V2BrowserConnectivityRecoveryDiagnostic
export type V2PeerRecoveryObserver = (event: V2PeerRecoveryEvent) => void

/** One injectable policy boundary is threaded from the gateway to each session generation. */
export interface V2PeerRecoveryDependencies {
  readonly policy?: V2PeerRecoveryPolicy
  readonly clock?: V2PeerRecoveryClock
  readonly random?: () => number
  readonly rearmSource?: V2PeerRecoveryRearmSource
  readonly observer?: V2PeerRecoveryObserver
  readonly observeAttempt?: V2PeerAttemptObserver
}

export interface V2PeerRecoverySupervisorOptions {
  readonly protocolSessionId: string
  readonly peerPathId: string
  readonly attempts: V2PeerRecoveryAttemptFactory
  readonly policy?: V2PeerRecoveryPolicy
  readonly clock?: V2PeerRecoveryClock
  readonly random?: () => number
  readonly rearmSource?: V2PeerRecoveryRearmSource
  readonly observer?: V2PeerRecoveryObserver
}

export type V2PeerRecoveryAttemptRace =
  | { readonly type: 'result'; readonly result: V2PeerAttemptResult }
  | { readonly type: 'owner-cancelled' }
  | { readonly type: 'budget-expired'; readonly exhaustion: V2PeerRecoveryExhaustion }

export type V2PeerRecoveryExhaustion =
  | 'wave-attempt-budget'
  | 'wave-elapsed-budget'
  | 'session-attempt-budget'
  | 'session-elapsed-budget'

export interface V2PeerRecoveryWaveBudget {
  readonly ordinal: number
  readonly startedAt: number
  attempts: number
}

export interface V2PeerRecoveryPendingWave {
  readonly trigger: V2PeerRecoveryWaveTrigger
  readonly rearmed: boolean
}

export function requireV2PeerRecoveryCorrelation(sessionId: string, peerPathId: string): void {
  requireCorrelation(sessionId, 'ProtocolSession ID')
  requireCorrelation(peerPathId, 'peer path ID')
}

function requirePositiveInteger(value: number, label: string): void {
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new RangeError(`${label} must be a positive safe integer`)
  }
}

function requireCorrelation(value: string, label: string): void {
  if (!/^[A-Za-z0-9_-]{1,128}$/.test(value)) {
    throw new RangeError(`${label} must be a bounded correlation identifier`)
  }
}

function requirePositiveMilliseconds(value: number, label: string): void {
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new RangeError(`${label} must be positive integer milliseconds`)
  }
}

function requireNonNegativeMilliseconds(value: number, label: string): void {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new RangeError(`${label} must be non-negative integer milliseconds`)
  }
}

function requireJitterRange(minimum: number, maximum: number): void {
  if (
    !Number.isFinite(minimum) ||
    !Number.isFinite(maximum) ||
    minimum <= 0 ||
    minimum > maximum ||
    maximum > 1
  ) throw new RangeError('retry jitter factors must satisfy 0 < minimum <= maximum <= 1')
}
