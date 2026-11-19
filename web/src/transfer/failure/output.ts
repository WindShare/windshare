import {
  snapshotClassification,
  isClassifiedTransferFailure,
  type ClassifiedTransferFailure,
} from './classification'
import type { TransferFileOutcomeEvidence } from './file-outcome'
import { FaultScope } from './fault'

/**
 * An explicit boundary protocol preserves native exception identity and causes.
 * Output implementations declare semantics before their errors reach scheduling;
 * transfer must never infer recovery authority from a backend class or message.
 */
export const OUTPUT_FAILURE = Symbol('windshare.output-failure')

export type OutputFailure =
  | Readonly<{
      kind: 'fault'
      classification: ClassifiedTransferFailure
      fileOutcomeEvidence?: TransferFileOutcomeEvidence
    }>
  | Readonly<{ kind: 'deferred-file-admission'; classification: ClassifiedTransferFailure }>

export interface OutputFailureSource {
  readonly [OUTPUT_FAILURE]: OutputFailure
}

export function declareOutputFailure(
  classification: ClassifiedTransferFailure,
  fileOutcomeEvidence?: TransferFileOutcomeEvidence,
): OutputFailure {
  return Object.freeze({
    kind: 'fault',
    classification: snapshotClassification(classification),
    ...(fileOutcomeEvidence === undefined
      ? {}
      : { fileOutcomeEvidence: Object.freeze({ ...fileOutcomeEvidence }) }),
  })
}

// A backend may defer one file before it opens a transaction when a later
// attempt can still use the retained operation. A file-local fault alone does
// not authorize keeping a completed batch in a resumable state.
export function declareDeferredFileAdmission(classification: ClassifiedTransferFailure): OutputFailure {
  if (classification.fault.scope !== FaultScope.FileLocal) throw new TypeError('Deferred admission requires a file-local fault')
  return Object.freeze({ kind: 'deferred-file-admission', classification: snapshotClassification(classification) })
}

export function declaredOutputFailure(error: unknown): OutputFailure | undefined {
  if (typeof error !== 'object' || error === null || !(OUTPUT_FAILURE in error)) return undefined
  const declared = (error as OutputFailureSource)[OUTPUT_FAILURE]
  // A malformed declaration is an unknown collaborator failure, never recovery authority.
  if (typeof declared !== 'object' || declared === null ||
      !isClassifiedTransferFailure(declared.classification)) return undefined
  switch (declared.kind) {
    case 'fault': return declareOutputFailure(declared.classification, declared.fileOutcomeEvidence)
    case 'deferred-file-admission':
      return declared.classification.fault.scope === FaultScope.FileLocal
        ? declareDeferredFileAdmission(declared.classification)
        : undefined
    default: return undefined
  }
}
