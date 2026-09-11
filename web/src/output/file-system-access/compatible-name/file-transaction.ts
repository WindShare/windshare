import type { PersistentFileRequest, PersistentFileTransactionPort, PersistentMaterializationPort } from '../../persistent-tree/contracts'
import type { CompatibleNamePathAuthority } from './path-authority'

export async function beginCompatibleNameFile(request: PersistentFileRequest,
  materialization: Pick<PersistentMaterializationPort, 'beginFile'>,
  names: Pick<CompatibleNamePathAuthority, 'commitFinalFile'>): Promise<PersistentFileTransactionPort> {
  const transaction = await materialization.beginFile(request)
  return compatibleNameFileTransaction(transaction,
    () => names.commitFinalFile(request.materializationRelativePath, transaction.ownedObjectId))
}

export function compatibleNameFileTransaction(
  transaction: PersistentFileTransactionPort,
  commitMapping: () => Promise<void>,
): PersistentFileTransactionPort {
  return Object.freeze({
    revision: transaction.revision,
    ownedObjectId: transaction.ownedObjectId,
    ...(transaction.checkpointPolicy === undefined ? {} : { checkpointPolicy: transaction.checkpointPolicy }),
    ...(transaction.checkpointObjectId === undefined ? {} : { checkpointObjectId: transaction.checkpointObjectId }),
    get initialDurableRanges() { return transaction.initialDurableRanges },
    get verifiedRanges() { return transaction.verifiedRanges },
    writeRange: (offset: bigint, data: Uint8Array, signal?: AbortSignal) =>
      transaction.writeRange(offset, data, signal),
    checkpoint: (signal?: AbortSignal) => transaction.checkpoint(signal),
    automaticCheckpoint: (
      trigger: Parameters<PersistentFileTransactionPort['automaticCheckpoint']>[0],
      signal?: AbortSignal,
    ) => transaction.automaticCheckpoint(trigger, signal),
    commit: async (signal?: AbortSignal) => {
      const proof = await transaction.commit(signal)
      // A compatible physical name becomes publishable only after its final proof is durable.
      await commitMapping()
      return proof
    },
    pause: (reason?: unknown) => transaction.pause(reason),
    retire: (reason?: unknown) => transaction.retire(reason),
    close: () => transaction.close(),
  })
}
