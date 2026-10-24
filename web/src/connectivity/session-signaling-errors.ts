import { protocolFailureFact, type FailureFact } from '../diagnostics/incident/fact'
import { type ReceivedProtocolError } from '../diagnostics/incident/protocol-error'

export class SessionSignalingError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'SessionSignalingError'
  }
}

/**
 * The authenticated wire message is intentionally absent. The reviewed failure
 * is sufficient for retry, incident, and cross-runtime correlation.
 */
export class AuthenticatedPeerOperationError extends SessionSignalingError {
  readonly protocolFailure: ReceivedProtocolError
  readonly failureFact: FailureFact<'protocol_failure'>

  constructor(protocolFailure: ReceivedProtocolError) {
    super('Sender rejected the authenticated peer operation')
    if (protocolFailure.content.scope !== 'peer') {
      throw new TypeError('Authenticated peer failures require peer scope')
    }
    this.name = 'AuthenticatedPeerOperationError'
    this.protocolFailure = protocolFailure
    this.failureFact = protocolFailureFact({
      stage: 'peer_attempt',
      recoveryDisposition: protocolFailure.content.retryable ? 'retryable' : 'terminal',
      protocolFailure,
    })
  }
}

export class PeerProtocolError extends SessionSignalingError {
  constructor(message: string) {
    super(message)
    this.name = 'PeerProtocolError'
  }
}
