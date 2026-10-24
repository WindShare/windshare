export function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError'
}

export class ReceiveSourceInvalidatedError extends Error {
  constructor(cause: unknown) {
    super('The receive source revision changed', { cause })
    this.name = 'ReceiveSourceInvalidatedError'
  }
}
