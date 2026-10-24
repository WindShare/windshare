import { RelayEndpointFailure } from './relay-race'
import { BlockLaneAttemptsError } from '../content/broker'
import { SessionRuntimeError } from '../session/runtime-types'
import { RelayReceiverError } from '../transport/relay/receiver'
import { RELAY_ERROR, RelayProtocolError } from '../transport/relay/protocol'
import { SenderObjectError } from '../crypto/sender-object'
import { CborError } from '../protocol/cbor'
import { TranscriptError } from '../session/transcript'
import { EnvelopeError } from '../session/envelope'
import { StaleShareInstanceError } from './session-factory'

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

export function recoveryRetryAfter(error: unknown): number {
  if (error instanceof RelayEndpointFailure) return recoveryRetryAfter(error.cause)
  if (error instanceof AggregateError) return Math.max(0, ...error.errors.map(recoveryRetryAfter))
  return error instanceof RelayReceiverError ? error.relayError?.retryAfterMilliseconds ?? 0 : 0
}

export function isLaneRecoveryFailure(error: unknown): boolean {
  return error instanceof BlockLaneAttemptsError ||
    (error instanceof SessionRuntimeError && error.scope === 'lane')
}

export function isSessionFailure(error: unknown): boolean {
  return error instanceof SessionRuntimeError && error.scope === 'session'
}
