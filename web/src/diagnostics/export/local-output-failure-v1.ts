import {
  isBoundedOutputExceptionProjection,
  type OutputExceptionProjection,
} from '../../output/diagnostics/exception'
import type { LocalOutputOperationFailureV1 } from '../../output/diagnostics/local-output-failure'
import { PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS } from '../../output/persistent-tree/stage-diagnostics'

export function hasBoundedOutputExceptionEvidence(
  record: LocalOutputOperationFailureV1,
): boolean {
  return outputExceptionEvidence(record).every(exception =>
    isBoundedOutputExceptionProjection(
      exception,
      PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.stringBytes,
    ))
}

function outputExceptionEvidence(
  record: LocalOutputOperationFailureV1,
): readonly OutputExceptionProjection[] {
  const facts = record.stageFailure.facts
  const exceptions: OutputExceptionProjection[] = [record.stageFailure.exception]
  const observed = [
    facts.fsa?.entry,
    facts.fsa?.committedBytes,
    facts.fsa?.permissions.read,
    facts.fsa?.permissions.readwrite,
    facts.fsa?.persistedHandle,
    facts.checkpoint?.candidates,
    facts.checkpoint?.committed,
  ]
  for (const fact of observed) {
    if (fact?.status === 'unavailable') exceptions.push(fact.exception)
  }
  if (facts.fsa?.writer?.closeFailure !== undefined) {
    exceptions.push(facts.fsa.writer.closeFailure)
  }
  exceptions.push(...(facts.probeFailures ?? []))
  for (const provider of facts.observation.unavailableProviders) {
    if (provider.exception !== undefined) exceptions.push(provider.exception)
  }
  return exceptions
}
