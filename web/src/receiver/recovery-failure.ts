import { RelayEndpointFailure } from './relay-race'
import { V2BlockLaneAttemptsError } from '../content/v2-broker'
import { V2SessionRuntimeError } from '../session/v2-runtime-types'
import { V2RelayReceiverError } from '../transport/relay/v2-receiver'
import { V2_RELAY_ERROR } from '../transport/relay/v2-protocol'
import { GenerationRecoveryExhaustedError } from './generation-recovery'
import { V2StaleShareInstanceError } from './v2-session-factory'

export function isTerminalRecoveryFailure(error: unknown): boolean {
  if (error instanceof RelayEndpointFailure) return isTerminalRecoveryFailure(error.cause)
  if (error instanceof AggregateError) {
    return error.errors.some(isSessionRecoveryFailure) ||
      (error.errors.length > 0 && error.errors.every(isTerminalRecoveryFailure))
  }
  return isSessionRecoveryFailure(error) ||
    (error instanceof V2RelayReceiverError && error.relayError?.code === V2_RELAY_ERROR.stopped)
}

function isSessionRecoveryFailure(error: unknown): boolean {
  if (error instanceof RelayEndpointFailure) return isSessionRecoveryFailure(error.cause)
  if (error instanceof AggregateError) return error.errors.some(isSessionRecoveryFailure)
  return error instanceof GenerationRecoveryExhaustedError || error instanceof V2StaleShareInstanceError
}

export function isLaneRecoveryFailure(error: unknown): boolean {
  return error instanceof V2BlockLaneAttemptsError ||
    (error instanceof V2SessionRuntimeError && error.scope === 'lane')
}

export function isSessionFailure(error: unknown): boolean {
  return error instanceof V2SessionRuntimeError && error.scope === 'session'
}
