import type { ContentReplica } from '../../transfer/output/content-availability'
import type { FileCheckpointV2 } from './checkpoint-model'

/** The caller owns local verification; persisted metadata alone cannot establish availability. */
export function fileCheckpointReplica(checkpoint: FileCheckpointV2, backend: string): ContentReplica | null {
  if (checkpoint.checkpointGeneration === 0n) return null
  return Object.freeze({
    backend, operationId: checkpoint.operationId, objectId: checkpoint.ownedObjectId,
    checkpointId: checkpoint.recordId, generation: checkpoint.checkpointGeneration, payloadOffset: 0n,
  })
}
