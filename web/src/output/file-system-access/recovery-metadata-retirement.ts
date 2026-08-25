import {
  MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  type MaterializationLedgerBindingV1,
} from '../materialization-ledger/model'
import type {
  FSAFileCheckpointRepository,
  FSASemanticOutputRepository,
} from './checkpoint-repository'

export async function retireFSAMaterializationRecoveryMetadata(
  checkpoints: FSAFileCheckpointRepository,
  binding: MaterializationLedgerBindingV1,
): Promise<void> {
  const candidate = checkpoints as Partial<FSASemanticOutputRepository>
  if (typeof candidate.retireMaterializationLedgerBatch !== 'function') {
    throw new DOMException(
      'DirectTree recovery requires semantic ledger repository authority',
      'InvalidStateError',
    )
  }
  const repository = candidate as FSASemanticOutputRepository
  for (;;) {
    const result = await repository.retireMaterializationLedgerBatch(
      binding,
      MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
    )
    if (result.state === 'complete') return
    if (result.deletedRows === 0) {
      throw new DOMException('FSA recovery metadata retirement made no progress', 'OperationError')
    }
  }
}
