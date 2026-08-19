import {
  protocolFailureFact,
  type FailureFact,
  type ProtocolFailure,
} from '../diagnostics/incident/fact'

export class V2SessionSignalingError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2SessionSignalingError'
  }
}

/**
 * The authenticated wire message is intentionally absent. The reviewed failure
 * is sufficient for retry, incident, and cross-runtime correlation.
 */
export class V2AuthenticatedPeerOperationError extends V2SessionSignalingError {
  readonly protocolFailure: ProtocolFailure
  readonly failureFact: FailureFact<'protocol_failure'>

  constructor(protocolFailure: ProtocolFailure) {
    super('Sender rejected the authenticated peer operation')
    if (protocolFailure.wireScope !== 'peer') {
      throw new TypeError('Authenticated peer failures require peer scope')
    }
    this.name = 'V2AuthenticatedPeerOperationError'
    this.protocolFailure = protocolFailure
    this.failureFact = protocolFailureFact({
      stage: 'peer_attempt',
      recoveryDisposition: protocolFailure.retryable ? 'retryable' : 'terminal',
      protocolFailure,
    })
  }
}

export class V2PeerProtocolError extends V2SessionSignalingError {
  constructor(message: string) {
    super(message)
    this.name = 'V2PeerProtocolError'
  }
}
