import { OUTPUT_FAILURE, type OutputFailureSource } from '../../../transfer/failure/output'
import { outputInitializationFailure } from '../../failure/semantics'

/** No writer was returned, so initialization can be retried without replaying writes. */
export class NativeOutputInitializationError extends Error implements OutputFailureSource {
  readonly [OUTPUT_FAILURE]: OutputFailureSource[typeof OUTPUT_FAILURE]

  constructor(cause: unknown) {
    super('Browser output could not be initialized', { cause })
    this.name = 'NativeOutputInitializationError'
    this[OUTPUT_FAILURE] = outputInitializationFailure(this)
  }
}
