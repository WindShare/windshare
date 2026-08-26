import type { PersistentFileTransactionPort } from '../../persistent-tree/contracts'

export function compatibleNameFileTransaction(
  transaction: PersistentFileTransactionPort,
  commitMapping: () => Promise<void>,
): PersistentFileTransactionPort {
  return Object.freeze({
    revision: transaction.revision,
    ownedObjectId: transaction.ownedObjectId,
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
