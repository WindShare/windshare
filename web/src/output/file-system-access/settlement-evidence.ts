import type { DirectoryAdmissionScope } from '../../transfer/directory-admission'
import type { MaterializationSummary } from '../../transfer/output-session'
import type { PersistentDirectTreeMaterializationEvidence } from '../../transfer/settlement/persistent-execution'
import {
  MaterializationLedgerEvidenceOutcome,
  validateMaterializationLedgerEvidence,
  type ValidatedMaterializationLedgerEvidence,
} from '../materialization-ledger/evidence'
import type { MaterializationLedgerSealV1 } from '../materialization-ledger/model'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type { DirectTreeIntent } from './settlement-proof'

export interface ValidateSealedFSASettlementEvidenceOptions {
  readonly intent: DirectTreeIntent
  readonly directoryScope: DirectoryAdmissionScope
  readonly evidence: PersistentDirectTreeMaterializationEvidence
  readonly seal: MaterializationLedgerSealV1
  readonly summary: MaterializationSummary
  readonly outcome: MaterializationLedgerEvidenceOutcome
}

/**
 * The locator and independently authenticated worker summary meet only at the immutable
 * repository seal. Keeping this join here prevents terminal code from growing a second manifest.
 */
export function validateSealedFSASettlementEvidence(
  options: ValidateSealedFSASettlementEvidenceOptions,
): ValidatedMaterializationLedgerEvidence {
  if (options.evidence.kind !== 'direct-tree-ledger' ||
      options.evidence.materializationBindingDigest !== options.intent.plan.reservation.digest ||
      options.seal.operationId !== options.intent.operationId) {
    throw new TargetOwnershipUnknownError('settlement', options.intent.operationId)
  }
  if (options.directoryScope.receiveIntentDigest !== options.intent.digest) {
    throw new TypeError('FSA settlement root expectation belongs to another receive intent')
  }
  return validateMaterializationLedgerEvidence({
    seal: options.seal,
    worker: options.summary,
    rootExpectation: options.directoryScope.rootExpectation,
    outcome: options.outcome,
  })
}
