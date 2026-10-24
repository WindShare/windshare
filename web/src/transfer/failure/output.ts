import {
  snapshotClassification,
  isClassifiedTransferFailure,
  type ClassifiedTransferFailure,
} from './classification'
import type { TransferFileOutcomeEvidence } from './file-outcome'

/**
 * An explicit boundary protocol preserves native exception identity and causes.
 * Output implementations declare semantics before their errors reach scheduling;
 * transfer must never infer recovery authority from a backend class or message.
 */
export const OUTPUT_FAILURE = Symbol('windshare.output-failure')

export interface OutputFailure {
  readonly classification: ClassifiedTransferFailure
  readonly fileOutcomeEvidence?: TransferFileOutcomeEvidence
}

export interface OutputFailureSource {
  readonly [OUTPUT_FAILURE]: OutputFailure
}

export function declareOutputFailure(
  classification: ClassifiedTransferFailure,
  fileOutcomeEvidence?: TransferFileOutcomeEvidence,
): OutputFailure {
  return Object.freeze({
    classification: snapshotClassification(classification),
    ...(fileOutcomeEvidence === undefined
      ? {}
      : { fileOutcomeEvidence: Object.freeze({ ...fileOutcomeEvidence }) }),
  })
}

export function declaredOutputFailure(error: unknown): OutputFailure | undefined {
  if (typeof error !== 'object' || error === null || !(OUTPUT_FAILURE in error)) return undefined
  const declared = (error as OutputFailureSource)[OUTPUT_FAILURE]
  // A malformed declaration is an unknown collaborator failure, never recovery authority.
  if (typeof declared !== 'object' || declared === null ||
      !isClassifiedTransferFailure(declared.classification)) return undefined
  return declareOutputFailure(declared.classification, declared.fileOutcomeEvidence)
}
