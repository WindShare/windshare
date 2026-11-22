import { RECOVERY_METADATA_RETIREMENT_BATCH_LIMIT } from '../persistence/journal'
import type {
  FSAFileCheckpointRepository,
  FSASemanticOutputRepository,
} from './checkpoint-repository'

export async function retireFSAMaterializationRecoveryMetadata(
  checkpoints: FSAFileCheckpointRepository,
): Promise<void> {
  const candidate = checkpoints as Partial<FSASemanticOutputRepository>
  if (typeof candidate.retireRecoveryMetadataBatch !== 'function') {
    throw new DOMException(
      'DirectTree recovery requires semantic checkpoint repository authority',
      'InvalidStateError',
    )
  }
  const repository = candidate as FSASemanticOutputRepository
  for (;;) {
    const result = await repository.retireRecoveryMetadataBatch(
      RECOVERY_METADATA_RETIREMENT_BATCH_LIMIT,
    )
    if (result.state === 'complete') return
    if (result.deletedRows === 0) {
      throw new DOMException('FSA recovery metadata retirement made no progress', 'OperationError')
    }
  }
}
