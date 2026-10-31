import type { FailureFactV2 } from '../export/incident-record-v2'
import type { FailureEvidence } from './evidence'
import type { DiagnosticExceptionProjection } from '../exception'
import { boundedDiagnosticText } from '../exception/text'

export const MAX_FAILURE_SUMMARY_TEXT_BYTES = 192
export interface FailureEvidenceSummary {
  readonly id: string
  readonly operation: string
  readonly signature: string
  readonly message: string | null
}
export function summarizeFailureEvidence(evidence: FailureEvidence): FailureEvidenceSummary {
  const message = exceptionSummary(evidence.exception, MAX_FAILURE_SUMMARY_TEXT_BYTES)
  return Object.freeze({
    id: evidence.id, operation: evidence.operation, signature: evidence.signature,
    message: message === null ? null : boundedDiagnosticText(message, MAX_FAILURE_SUMMARY_TEXT_BYTES),
  })
}

function exceptionSummary(node: DiagnosticExceptionProjection, capacity: number): string | null {
  const message = node.message ?? node.thrownValue
  const branches = [
    ...(node.cause === null ? [] : [{ label: 'cause', node: node.cause }]),
    ...node.errors.map(error => ({ label: 'branch', node: error })),
  ]
  if (branches.length === 0) return message === null ? null : boundedDiagnosticText(message, capacity)
  // A generic wrapper must not consume the entire summary before its concrete causes.
  const share = Math.floor(capacity / (branches.length + 1))
  const parts = [boundedDiagnosticText(message ?? node.errorName ?? '', share)]
  for (const branch of branches) {
    const prefix = '; ' + branch.label + ': '
    parts.push(prefix + (exceptionSummary(branch.node, Math.max(0, share - prefix.length)) ?? ''))
  }
  return boundedDiagnosticText(parts.join(''), capacity)
}

export function summarizeFailureFact(fact: FailureFactV2): FailureFactV2 {
  if (fact.evidence === undefined) return fact
  const evidence = fact.evidence
  const exception = evidence.exception
  return Object.freeze({ ...fact, evidence: Object.freeze({
    ...evidence, context: Object.freeze({}), contextTruncated: true,
    exception: Object.freeze({
      ...exception,
      constructorName: null, errorName: exception.errorName === null ? null : boundedDiagnosticText(exception.errorName, MAX_FAILURE_SUMMARY_TEXT_BYTES),
      message: summarizeFailureEvidence(evidence).message, thrownValue: null, stack: null,
      cause: null, errors: Object.freeze([]),
      omittedBranches: exception.omittedBranches + exception.errors.length + (exception.cause === null ? 0 : 1),
      truncated: Object.freeze(['summary']), unavailable: Object.freeze([]),
    }),
  }) })
}
