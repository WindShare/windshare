import { bindFailureEvidence } from '../../diagnostics/failure/evidence'
import { isFailureFact, type FailureFact, type FailureFactRef } from '../../diagnostics/incident'
import { BoundaryFaultError, isFault, type Fault } from './fault'
import type { TransferFileOutcomeEvidence } from './file-outcome'
import { MATERIALIZATION_FAILURE_REASONS, type MaterializationFailureReason } from './materialization'

export interface ClassifiedTransferFailure {
  readonly fault: Fault
  readonly fact: FailureFact
  readonly factRef?: FailureFactRef
  readonly materializationFailureReason: MaterializationFailureReason
}

export class ClassifiedTransferFailureError extends BoundaryFaultError {
  readonly classification: ClassifiedTransferFailure
  readonly fileOutcomeEvidence: TransferFileOutcomeEvidence | undefined

  constructor(
    classification: ClassifiedTransferFailure,
    fileOutcomeEvidence?: TransferFileOutcomeEvidence,
  ) {
    super(classification.fault, classification.fact.evidence?.exception.message ?? 'File transfer failed at a classified boundary')
    if (classification.fact.evidence !== undefined) bindFailureEvidence(this, classification.fact.evidence)
    this.name = 'ClassifiedTransferFailureError'
    this.classification = snapshotClassification(classification)
    this.fileOutcomeEvidence = fileOutcomeEvidence === undefined
      ? undefined
      : snapshotTransferFileOutcomeEvidence(fileOutcomeEvidence)
  }
}

function snapshotTransferFileOutcomeEvidence(
  evidence: TransferFileOutcomeEvidence,
): TransferFileOutcomeEvidence {
  return Object.freeze({ ...evidence })
}

export function snapshotClassification(
  classification: ClassifiedTransferFailure,
): ClassifiedTransferFailure {
  if (!isClassifiedTransferFailure(classification)) {
    throw new TypeError('Transfer failure classification is not closed and immutable')
  }
  return Object.freeze({
    fault: classification.fault,
    fact: classification.fact,
    ...(classification.factRef === undefined ? {} : { factRef: classification.factRef }),
    materializationFailureReason: classification.materializationFailureReason,
  })
}

export function isClassifiedTransferFailure(
  value: unknown,
): value is ClassifiedTransferFailure {
  if (typeof value !== 'object' || value === null || !Object.isFrozen(value)) return false
  const candidate = value as Partial<ClassifiedTransferFailure>
  return isFault(candidate.fault) &&
    Object.isFrozen(candidate.fault) &&
    isFailureFact(candidate.fact) &&
    factMatchesFault(candidate.fact, candidate.fault) &&
    typeof candidate.materializationFailureReason === 'string' &&
    MATERIALIZATION_FAILURE_REASONS.some(reason =>
      reason === candidate.materializationFailureReason)
}

function factMatchesFault(fact: FailureFact, fault: Fault): boolean {
  return fact.kind !== 'fault' || fact.payload.fault === fault
}
