import { bindFailureEvidence, captureFailureEvidence, type FailureContextValue } from '../diagnostics/failure/evidence'

/** Validation owns expected/actual values; downstream classifiers cannot reconstruct them. */
export function catalogFailure<Failure extends Error>(
  failure: Failure, context: Readonly<Record<string, FailureContextValue>>,
): Failure {
  captureFailureEvidence(failure, { operation: 'catalog.validate', context })
  return failure
}

/** Keep rejected interpretations without changing the error selected by the protocol decoder. */
export function catalogAlternativesFailure<Failure>(
  failure: Failure, alternatives: readonly unknown[], context: Readonly<Record<string, FailureContextValue>>,
): Failure {
  bindFailureEvidence(failure, captureFailureEvidence(
    new AggregateError([...alternatives, failure], 'Catalog object failed its authenticated interpretations'),
    { operation: 'catalog.open', context },
  ))
  return failure
}
