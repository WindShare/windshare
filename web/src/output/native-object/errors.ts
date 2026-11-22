import { FaultScope, OutputFaultCode, TransferFaultError, outputFault } from '../../transfer/fault'

/** No writer was returned, so initialization can be retried without replaying writes. */
export class NativeOutputInitializationError extends TransferFaultError {
  constructor(cause: unknown) {
    super(
      outputFault(FaultScope.Job, OutputFaultCode.StorageUnavailable),
      'Browser output could not be initialized',
      { cause },
    )
    this.name = 'NativeOutputInitializationError'
  }
}
