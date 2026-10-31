import type { FailureFact } from '../incident/fact'
import { captureFailureEvidence, type FailureEvidenceInput } from './evidence'

export function withFailureEvidence<Fact extends FailureFact>(
  fact: Fact, error: unknown, input: Omit<FailureEvidenceInput, 'error'> = {},
): Fact {
  if (fact.evidence !== undefined) return fact
  return Object.freeze({
    ...fact,
    evidence: captureFailureEvidence(error, { ...input, operation: input.operation ?? fact.stage }),
  }) as Fact
}
