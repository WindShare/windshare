import { BoundaryFaultError, FaultScope, OutputFaultCode, outputFault } from '../fault'

/** A declined in-place growth admission owns no write; existing admitted files drain before task pause. */
export class OutputCapacityBlockedError extends BoundaryFaultError {
  readonly #drained: Promise<void>

  constructor(cause: unknown, drained: Promise<void>) {
    super(outputFault(FaultScope.OutputPause, OutputFaultCode.ResourceBudget),
      'Browser storage growth is paused until admitted files checkpoint')
    this.cause = cause
    this.name = 'OutputCapacityBlockedError'
    this.#drained = drained
  }

  async waitForDrain(signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    await new Promise<void>((resolve, reject) => {
      const abort = () => {
        signal.removeEventListener('abort', abort)
        reject(signal.reason)
      }
      signal.addEventListener('abort', abort, { once: true })
      this.#drained.then(() => {
        signal.removeEventListener('abort', abort)
        resolve()
      }, reason => {
        signal.removeEventListener('abort', abort)
        reject(reason)
      })
      if (signal.aborted) abort()
    })
  }
}
