import type { PersistentOutputTree } from '../persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type { FileCheckpointV2 } from '../persistence/checkpoint'
import { checkpointMatchesNamespace } from '../persistence/journal'
import type { BrowserDeliveryRecordV1, BrowserSavePolicyV1 } from './model'

const TARGET_VERIFICATION_CHUNK_BYTES = 1024 * 1024

/** Browser handles can still resolve after same-path replacement; only matching local content may be retired. */
export async function verifyStagedDeliveryTarget(input: {
  readonly record: BrowserDeliveryRecordV1
  readonly content: Blob
  readonly checkpoint: FileCheckpointV2 | undefined
  readonly tree: Pick<PersistentOutputTree, 'openFile'>
  readonly namespace: BrowserSavePolicyV1['target']
}): Promise<'empty' | 'matching-staged-content'> {
  const { record, content, checkpoint, tree, namespace } = input
  if (checkpoint === undefined) return 'empty'
  if (!checkpointMatchesNamespace(checkpoint, namespace) ||
      checkpoint.fileId !== record.fileId || checkpoint.fileRevision !== record.source.fileRevision ||
      checkpoint.exactSize !== record.source.exactSize || BigInt(content.size) !== record.source.exactSize ||
      JSON.stringify(checkpoint.canonicalPath) !== JSON.stringify(record.materializationRelativePath)) {
    throw new TypeError('Staged target verification requires the original owned checkpoint')
  }
  const target = await tree.openFile(checkpoint.canonicalPath, checkpoint.ownedObjectId)
  if (target === undefined) throw new TargetOwnershipUnknownError('writer-open', record.operationId)
  try {
    const size = await target.size()
    if (size > record.source.exactSize) throw new TargetOwnershipUnknownError('writer-open', record.operationId)
    if (size > 0n) {
      const existing = await target.read()
      if (BigInt(existing.size) !== size) throw new TargetOwnershipUnknownError('writer-open', record.operationId)
      for (let offset = 0; offset < existing.size; offset += TARGET_VERIFICATION_CHUNK_BYTES) {
        const end = Math.min(existing.size, offset + TARGET_VERIFICATION_CHUNK_BYTES)
        const [actual, expected] = await Promise.all([
          existing.slice(offset, end).arrayBuffer(), content.slice(offset, end).arrayBuffer(),
        ])
        const expectedBytes = new Uint8Array(expected)
        if (new Uint8Array(actual).some((value, index) => value !== expectedBytes[index])) {
          throw new TargetOwnershipUnknownError('writer-open', record.operationId)
        }
      }
    }
    await target.verify('writer-open')
    return size === 0n ? 'empty' : 'matching-staged-content'
  } finally { await target.close() }
}
