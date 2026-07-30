export class RemotePionContainmentError extends Error {
  constructor() {
    super('remote Pion lease cleanup did not prove terminal reaping')
    this.name = 'RemotePionContainmentError'
  }
}

export class RemotePionTransportUnavailableError extends Error {
  constructor() {
    super('remote Pion control transport is unreachable')
    this.name = 'RemotePionTransportUnavailableError'
  }
}

export class RemotePionAuthorityExpiredError extends Error {
  constructor() {
    super('remote Pion signed authority or attempt lease expired')
    this.name = 'RemotePionAuthorityExpiredError'
  }
}

export class RemotePionProtocolError extends Error {
  readonly failureCode: 'proof-invalid' | 'authority-binding-mismatch'

  constructor(
    failureCode: 'proof-invalid' | 'authority-binding-mismatch',
    message: string,
  ) {
    super(message)
    this.name = 'RemotePionProtocolError'
    this.failureCode = failureCode
  }
}

export class RemotePionUnexpectedStatusError extends Error {
  readonly statusCode: number

  constructor(statusCode: number) {
    super('remote Pion control response has an unexpected status')
    this.name = 'RemotePionUnexpectedStatusError'
    this.statusCode = statusCode
  }
}

export class RemotePionOperationAbortedError extends Error {
  constructor() {
    super('remote Pion control operation was aborted')
    this.name = 'RemotePionOperationAbortedError'
  }
}
