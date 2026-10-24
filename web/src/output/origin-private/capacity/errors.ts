import { OUTPUT_FAILURE, type OutputFailureSource } from '../../../transfer/failure/output'
import { outputCapacityStateFailure } from '../../failure/semantics'

/** Invalid persistence must never be interpreted as zero usage or repaired by arithmetic. */
export class OriginCapacityDataError extends DOMException implements OutputFailureSource {
  readonly [OUTPUT_FAILURE]: OutputFailureSource[typeof OUTPUT_FAILURE]

  constructor(detail: string) {
    super(`Invalid origin capacity state: ${detail}`, 'DataError')
    this[OUTPUT_FAILURE] = outputCapacityStateFailure(this)
  }
}
