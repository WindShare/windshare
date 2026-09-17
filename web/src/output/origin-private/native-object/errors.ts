/** No writer was returned, so initialization can be retried without replaying writes. */
export class NativeOutputInitializationError extends Error {
  constructor(cause: unknown) {
    super('Browser output could not be initialized', { cause })
    this.name = 'NativeOutputInitializationError'
  }
}
