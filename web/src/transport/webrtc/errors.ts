export class WebRTCDataChannelConfigurationError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'WebRTCDataChannelConfigurationError'
  }
}

export class WebRTCChannelNotOpenError extends Error {
  constructor() {
    super('WebRTC frame channel is not open')
    this.name = 'WebRTCChannelNotOpenError'
  }
}

export class WebRTCChannelClosedError extends Error {
  constructor(message = 'WebRTC frame channel is closed') {
    super(message)
    this.name = 'WebRTCChannelClosedError'
  }
}

export class WebRTCFrameBoundsError extends RangeError {
  constructor(length: number, maximum: number) {
    super(`frame must be 1..${maximum} bytes; got ${length}`)
    this.name = 'WebRTCFrameBoundsError'
  }
}

export class WebRTCPeerProtocolError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(`WebRTC peer protocol violation: ${message}`, options)
    this.name = 'WebRTCPeerProtocolError'
  }
}

export class WebRTCTerminalNotAcknowledgedError extends Error {
  constructor(cause: unknown) {
    super('WebRTC terminal frame was not acknowledged', { cause })
    this.name = 'WebRTCTerminalNotAcknowledgedError'
  }
}

export class WebRTCRemoteClosedError extends Error {
  constructor() {
    super('WebRTC peer closed the DataChannel')
    this.name = 'WebRTCRemoteClosedError'
  }
}

export class WebRTCTransportError extends Error {
  constructor(message: string, cause: unknown) {
    super(`WebRTC DataChannel transport failed: ${message}`, { cause })
    this.name = 'WebRTCTransportError'
  }
}

export class WebRTCIngressOverflowError extends Error {
  constructor() {
    super('WebRTC inbound frame queue is full')
    this.name = 'WebRTCIngressOverflowError'
  }
}
