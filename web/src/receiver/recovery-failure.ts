import { RelayEndpointFailure } from './relay-race'
import { V2BlockLaneAttemptsError } from '../content/v2-broker'
import { V2SessionRuntimeError } from '../session/v2-runtime-types'
import { V2RelayReceiverError } from '../transport/relay/v2-receiver'
import { V2_RELAY_ERROR, V2RelayProtocolError } from '../transport/relay/v2-protocol'
import { SenderObjectError } from '../crypto/sender-object'
import { V2CborError } from '../protocol/cbor'
import { V2TranscriptError } from '../session/v2-transcript'
import { V2EnvelopeError } from '../session/v2-envelope'
import { V2StaleShareInstanceError } from './v2-session-factory'

export function isTerminalRecoveryFailure(error: unknown): boolean {
  if (error instanceof RelayEndpointFailure) return isTerminalRecoveryFailure(error.cause)
  if (error instanceof AggregateError) {
    return error.errors.some(isShareRecoveryFailure) ||
      (error.errors.length > 0 && error.errors.every(isTerminalRecoveryFailure))
  }
  return isShareRecoveryFailure(error) || isRejectedRelayData(error) ||
    (error instanceof V2RelayReceiverError && error.relayError !== undefined &&
      ![V2_RELAY_ERROR.notFound, V2_RELAY_ERROR.starting, V2_RELAY_ERROR.admission,
        V2_RELAY_ERROR.challengeExpired].some(code => code === error.relayError?.code))
}

export function isShareRecoveryFailure(error: unknown): boolean {
  if (error instanceof RelayEndpointFailure) return isShareRecoveryFailure(error.cause)
  if (error instanceof AggregateError) return error.errors.some(isShareRecoveryFailure)
  // Unverified endpoint data cannot revoke a healthy authenticated session.
  // Only descriptor continuity checks and the established session own that authority.
  return error instanceof V2StaleShareInstanceError || isSessionFailure(error)
}

function isRejectedRelayData(error: unknown): boolean {
  return error instanceof SenderObjectError || error instanceof V2CborError ||
    error instanceof V2TranscriptError || error instanceof V2EnvelopeError ||
    error instanceof V2RelayProtocolError
}

export function recoveryRetryAfter(error: unknown): number {
  if (error instanceof RelayEndpointFailure) return recoveryRetryAfter(error.cause)
  if (error instanceof AggregateError) return Math.max(0, ...error.errors.map(recoveryRetryAfter))
  return error instanceof V2RelayReceiverError ? error.relayError?.retryAfterMilliseconds ?? 0 : 0
}

export function isLaneRecoveryFailure(error: unknown): boolean {
  return error instanceof V2BlockLaneAttemptsError ||
    (error instanceof V2SessionRuntimeError && error.scope === 'lane')
}

export function isSessionFailure(error: unknown): boolean {
  return error instanceof V2SessionRuntimeError && error.scope === 'session'
}
