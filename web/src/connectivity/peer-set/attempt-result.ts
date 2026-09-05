import {
  classifyV2PeerAttemptFailure,
  type V2PeerAdmittedLane,
  type V2PeerAttemptCancellationOwner,
  type V2PeerAttemptResult,
} from '../v2-peer-failure'
import type { V2PeerAttemptHandle } from './contract'

const UINT32_MAXIMUM = 0xffff_ffff

export function normalizeAttemptResult(value: unknown): V2PeerAttemptResult {
  if (!isRecord(value)) return localContractResult()
  if (value.type === 'admitted' && isLane(value.lane)) {
    return Object.freeze({ type: 'admitted', lane: Object.freeze({ ...value.lane }) })
  }
  if (value.type === 'failed') {
    const decision = classifyV2PeerAttemptFailure(value.failure)
    if (decision.reason !== 'untyped-failure') return value as unknown as V2PeerAttemptResult
  }
  if (
    value.type === 'lifecycle-cancelled' &&
    isCancellationOwner(value.owner)
  ) return value as unknown as V2PeerAttemptResult
  return localContractResult()
}

export function localContractResult(): V2PeerAttemptResult {
  return Object.freeze({
    type: 'failed',
    failure: Object.freeze({
      kind: 'local-contract',
      code: 'invalid-adapter-result',
    }),
  })
}

/**
 * Authentication may settle after local cancellation wins the race. Those facts
 * retain protocol authority and must not be replaced by a local budget outcome.
 */
export function authoritativeAfterCancellation(result: V2PeerAttemptResult): boolean {
  if (result.type === 'admitted') return true
  if (result.type !== 'failed') return false
  return result.failure.kind === 'authenticated-peer-operation' ||
    result.failure.kind === 'authenticated-lane-rejection' ||
    result.failure.kind === 'session-terminal'
}

export function isAttemptHandle(value: unknown): value is V2PeerAttemptHandle {
  if (typeof value !== 'object' || value === null) return false
  const candidate = value as Partial<V2PeerAttemptHandle>
  return candidate.attemptId?.kind === 'peer_attempt' &&
    candidate.attemptId.byteLength === 16 &&
    candidate.attemptId.copyBytes().byteLength === 16 &&
    candidate.result instanceof Promise &&
    typeof candidate.cancel === 'function'
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

function isLane(value: unknown): value is V2PeerAdmittedLane {
  if (typeof value !== 'object' || value === null) return false
  const lane = value as Partial<V2PeerAdmittedLane>
  return isNonzeroUint32(lane.laneId) && isNonzeroUint32(lane.laneEpoch)
}

function isNonzeroUint32(value: unknown): value is number {
  return Number.isSafeInteger(value) && (value as number) > 0 && (value as number) <= UINT32_MAXIMUM
}

function isCancellationOwner(value: unknown): value is V2PeerAttemptCancellationOwner {
  return value === 'last-activation' || value === 'generation-replaced' ||
    value === 'runtime-stop' || value === 'recovery-budget'
}

export function waitForAbort(signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.resolve()
  return new Promise((resolve) => signal.addEventListener('abort', () => resolve(), { once: true }))
}

export function abortOwner(signal: AbortSignal): V2PeerAttemptCancellationOwner {
  return isCancellationOwner(signal.reason) ? signal.reason : 'runtime-stop'
}
