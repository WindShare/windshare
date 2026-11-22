import type { ReceivedProtocolError } from '../session/diagnostics'

export class SessionSignalingError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'SessionSignalingError'
  }
}

/**
 * The authenticated wire message is intentionally absent. The decoded failure
 * is sufficient for retry and cross-runtime correlation.
 */
export class AuthenticatedPeerOperationError extends SessionSignalingError {
  readonly protocolFailure: ReceivedProtocolError

  constructor(protocolFailure: ReceivedProtocolError) {
    super('Sender rejected the authenticated peer operation')
    if (protocolFailure.content.scope !== 'peer') {
      throw new TypeError('Authenticated peer failures require peer scope')
    }
    this.name = 'AuthenticatedPeerOperationError'
    this.protocolFailure = protocolFailure
  }
}

export class PeerProtocolError extends SessionSignalingError {
  constructor(message: string) {
    super(message)
    this.name = 'PeerProtocolError'
  }
}
