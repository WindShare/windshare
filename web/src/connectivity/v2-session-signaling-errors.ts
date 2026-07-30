import type { V2OperationErrorControl } from '../session/v2-message'

export class V2SessionSignalingError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2SessionSignalingError'
  }
}

/** Authenticated sender text remains byte-for-byte visible to evidence consumers. */
export class V2AuthenticatedPeerOperationError extends V2SessionSignalingError {
  readonly operationFailure: V2OperationErrorControl & { readonly scope: 'peer' }

  constructor(operationFailure: V2OperationErrorControl & { readonly scope: 'peer' }) {
    super(operationFailure.message)
    this.name = 'V2AuthenticatedPeerOperationError'
    this.operationFailure = Object.freeze({ ...operationFailure })
  }
}

export class V2PeerProtocolError extends V2SessionSignalingError {
  constructor(message: string) {
    super(message)
    this.name = 'V2PeerProtocolError'
  }
}
