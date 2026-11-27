import { BandwidthWaitError } from '../contracts/capacity'
import { RelayEndpointFailure } from './relay-selection/race'
import { BlockLaneAttemptsError } from '../content/broker'
import { BlockInactivityTimeoutError } from '../content/scheduling/block-response-wait'
import { OperationRetryError, type SessionLaneIdentity, SessionRuntimeError } from '../session/runtime-types'
import { RelayReceiverError } from '../transport/relay/receiver'
import { RELAY_ERROR, RelayProtocolError, type RelayErrorCode } from '../transport/relay/protocol'
import { SenderObjectError } from '../crypto/sender-object'
import { CborError } from '../protocol/cbor'
import { TranscriptError } from '../session/transcript'
import { EnvelopeError } from '../session/envelope'
import { StaleShareInstanceError } from './session-factory'

export function isRelayAccessDenied(error: unknown): boolean {
  if (error instanceof RelayEndpointFailure) return isRelayAccessDenied(error.cause)
  if (error instanceof AggregateError) return error.errors.length > 0 && error.errors.every(isRelayAccessDenied)
  return error instanceof RelayReceiverError && error.relayError?.code === RELAY_ERROR.accessDenied
}

export function isTerminalRecoveryFailure(error: unknown): boolean {
  if (error instanceof RelayEndpointFailure) return isTerminalRecoveryFailure(error.cause)
  if (error instanceof AggregateError) {
    return error.errors.some(isShareRecoveryFailure) ||
      (error.errors.length > 0 && error.errors.every(isTerminalRecoveryFailure))
  }
  return isShareRecoveryFailure(error) || isRejectedRelayData(error) ||
    (error instanceof RelayReceiverError && error.relayError !== undefined &&
      ![RELAY_ERROR.notFound, RELAY_ERROR.starting, RELAY_ERROR.admission, RELAY_ERROR.accessUnavailable,
        RELAY_ERROR.challengeExpired].some(code => code === error.relayError?.code))
}

export function isShareRecoveryFailure(error: unknown): boolean {
  if (error instanceof RelayEndpointFailure) return isShareRecoveryFailure(error.cause)
  if (error instanceof AggregateError) return error.errors.some(isShareRecoveryFailure)
  // Unverified endpoint data cannot revoke a healthy authenticated session.
  // Only descriptor continuity checks and the established session own that authority.
  return error instanceof StaleShareInstanceError || isSessionFailure(error)
}

function isRejectedRelayData(error: unknown): boolean {
  return error instanceof SenderObjectError || error instanceof CborError ||
    error instanceof TranscriptError || error instanceof EnvelopeError ||
    error instanceof RelayProtocolError
}

export function recoveryBandwidthWait(error: unknown, depth = 0): number {
  if (depth >= 8) return 0
  if (error instanceof BandwidthWaitError) return error.retryAfterMilliseconds
  if (error instanceof AggregateError) return Math.max(0, ...error.errors.map(cause => recoveryBandwidthWait(cause, depth + 1)))
  return error instanceof Error ? recoveryBandwidthWait(error.cause, depth + 1) : 0
}

export type RecoveryRetryReason = 'bandwidth' | 'server' | 'share_not_ready'

export interface RecoveryRetry {
  readonly retryAt: number
  readonly reason: RecoveryRetryReason
}

const RETRY_REASON_PRIORITY: Readonly<Record<RecoveryRetryReason, number>> = Object.freeze({
  share_not_ready: 0,
  server: 1,
  bandwidth: 2,
})

export class RelayCooldownError extends Error {
  readonly retry: RecoveryRetry

  constructor(retry: RecoveryRetry, cause?: unknown) {
    super('Relay cooldown is still active', { cause })
    this.name = 'RelayCooldownError'
    this.retry = retry
  }
}

export function latestRecoveryRetry(first: RecoveryRetry | undefined, second: RecoveryRetry | undefined): RecoveryRetry | undefined {
  if (first === undefined) return second
  if (second === undefined) return first
  if (second.retryAt !== first.retryAt) return second.retryAt > first.retryAt ? second : first
  return RETRY_REASON_PRIORITY[second.reason] > RETRY_REASON_PRIORITY[first.reason] ? second : first
}

export function earliestRecoveryRetry(first: RecoveryRetry | undefined, second: RecoveryRetry | undefined): RecoveryRetry | undefined {
  if (first === undefined) return second
  if (second === undefined) return first
  if (second.retryAt !== first.retryAt) return second.retryAt < first.retryAt ? second : first
  return RETRY_REASON_PRIORITY[second.reason] < RETRY_REASON_PRIORITY[first.reason] ? second : first
}

function relayRetryReason(code: RelayErrorCode): 'server' | 'share_not_ready' {
  // Code 9 (`starting`) covers both initial publication and a detached sender
  // in grace: its RetryAfter paces polling rather than signalling a relay outage.
  return code === RELAY_ERROR.starting ? 'share_not_ready' : 'server'
}

export function recoveryRetry(error: unknown, now: number, depth = 0): RecoveryRetry | undefined {
  // A relay race delegates endpoint-scoped waits to the owner of its candidates.
  if (depth >= 8 || error instanceof RelayEndpointFailure) return undefined
  if (error instanceof RelayCooldownError) return error.retry
  if (error instanceof BandwidthWaitError) return { retryAt: now + error.retryAfterMilliseconds, reason: 'bandwidth' }
  if (error instanceof AggregateError) {
    return error.errors.reduce<RecoveryRetry | undefined>(
      (latest, cause: unknown) => latestRecoveryRetry(latest, recoveryRetry(cause, now, depth + 1)), undefined)
  }
  if (!(error instanceof Error)) return undefined
  const relayError = error instanceof RelayReceiverError ? error.relayError : undefined
  const milliseconds = relayError?.retryAfterMilliseconds ?? 0
  return latestRecoveryRetry(
    milliseconds > 0 && relayError !== undefined
      ? { retryAt: now + milliseconds, reason: relayRetryReason(relayError.code) }
      : undefined,
    recoveryRetry(error.cause, now, depth + 1),
  )
}

export interface RecoveryWaitPlan {
  readonly retryAt: number
  readonly mandatory?: Readonly<{
    readonly milliseconds: number
    readonly reason: 'capacity' | 'server' | 'bandwidth'
  }>
  readonly shareFloorAt?: number
}

export function planRecoveryWait(
  now: number,
  capacityDelay: number,
  backoff: number,
  retry: RecoveryRetry | undefined,
): RecoveryWaitPlan {
  const retryDelay = Math.max(0, (retry?.retryAt ?? now) - now)
  // Share-not-ready polling hints still pace reconnects, but are not relay
  // outages and must not flash the service-cooldown banner.
  const serviceReason = retry?.reason === 'bandwidth' || retry?.reason === 'server' ? retry.reason : undefined
  const serviceDelay = serviceReason === undefined ? 0 : retryDelay
  const mandatoryMilliseconds = Math.max(capacityDelay, serviceDelay)
  const retryAt = now + Math.max(capacityDelay, retryDelay, backoff)
  const mandatoryReason = serviceDelay >= capacityDelay && serviceReason !== undefined ? serviceReason : 'capacity'
  return Object.freeze({
    retryAt,
    ...(mandatoryMilliseconds > 0
      ? { mandatory: Object.freeze({ milliseconds: mandatoryMilliseconds, reason: mandatoryReason }) }
      : {}),
    ...(retry?.reason === 'share_not_ready' && retryDelay > 0 ? { shareFloorAt: retry.retryAt } : {}),
  })
}

export function isLaneRecoveryFailure(error: unknown): boolean {
  return error instanceof BandwidthWaitError || error instanceof OperationRetryError ||
    error instanceof BlockLaneAttemptsError ||
    (error instanceof SessionRuntimeError && error.scope === 'lane')
}

export function hasBlockInactivityTimeout(error: unknown, depth = 0): boolean {
  if (depth >= 8) return false
  if (error instanceof BlockInactivityTimeoutError) return true
  if (error instanceof BlockLaneAttemptsError || error instanceof AggregateError) {
    return error.errors.some(cause => hasBlockInactivityTimeout(cause, depth + 1))
  }
  return false
}

export function stalledBlockLanes(error: unknown): readonly SessionLaneIdentity[] {
  const lanes = new Map<string, SessionLaneIdentity>()
  collectStalledLanes(error, lanes, 0)
  return Object.freeze([...lanes.values()])
}

function collectStalledLanes(error: unknown, lanes: Map<string, SessionLaneIdentity>, depth: number): void {
  if (depth >= 8) return
  if (error instanceof BlockInactivityTimeoutError) {
    if (error.lane !== undefined) {
      const key = `${error.lane.laneId}:${error.lane.laneEpoch}`
      if (!lanes.has(key)) lanes.set(key, error.lane)
    }
    return
  }
  if (error instanceof BlockLaneAttemptsError || error instanceof AggregateError) {
    for (const cause of error.errors) collectStalledLanes(cause, lanes, depth + 1)
  }
}

export function isSessionFailure(error: unknown): boolean {
  return error instanceof SessionRuntimeError && error.scope === 'session'
}
