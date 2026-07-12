export class PeerNegotiationError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(`WebRTC negotiation failed: ${message}`, options)
    this.name = 'PeerNegotiationError'
  }
}

export class CandidateLimitExceededError extends PeerNegotiationError {
  constructor(limit: number) {
    super(`ICE candidate limit ${limit} exceeded`)
    this.name = 'CandidateLimitExceededError'
  }
}

export class UnexpectedDataChannelError extends PeerNegotiationError {
  constructor() {
    super('peer created an unexpected DataChannel')
    this.name = 'UnexpectedDataChannelError'
  }
}

export class ReceiverConnectivityClosedError extends Error {
  constructor() {
    super('browser receiver connectivity is closed')
    this.name = 'ReceiverConnectivityClosedError'
  }
}
