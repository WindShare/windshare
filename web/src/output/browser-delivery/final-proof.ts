import {
  INDEXEDDB_BY_OPERATION_RECORD_PROOF_INDEX, INDEXEDDB_FILE_FINAL_PROOF_STORE, requestResult,
} from '../browser/indexeddb-database'
import { createMaterializationLedgerBindingSync } from '../materialization-ledger/codec'
import {
  decodeMaterializationFinalFileProofV1Sync, validateFinalOutputAgainstCheckpoint,
} from '../materialization-ledger/journal'
import { fileCheckpointDigest, type FileCheckpointV2 } from '../persistence/checkpoint'
import { finalFileCheckpointProof, type FinalFileCheckpointProof } from '../persistence/journal'
import { BrowserDeliveryConcurrencyError } from './repository'

export function validateDirectCheckpointProof(input: FinalFileCheckpointProof, checkpoint: FileCheckpointV2): void {
  const expected = finalFileCheckpointProof(checkpoint)
  const keys = Object.keys(expected) as (keyof FinalFileCheckpointProof)[]
  for (const key of keys) {
    const value = input[key]
    const canonical = expected[key]
    const equal = Array.isArray(value)
      ? JSON.stringify(value) === JSON.stringify(canonical) : value === canonical
    if (!equal) throw new BrowserDeliveryConcurrencyError('Direct final checkpoint proof changed')
  }
}

export async function requireBrowserTargetProof(
  transaction: IDBTransaction,
  checkpoint: FileCheckpointV2,
): Promise<void> {
  const raw: unknown = await requestResult(transaction.objectStore(INDEXEDDB_FILE_FINAL_PROOF_STORE)
    .index(INDEXEDDB_BY_OPERATION_RECORD_PROOF_INDEX).get([checkpoint.operationId, checkpoint.recordId]))
  validateBrowserTargetProof(raw, checkpoint)
}

/** Synchronous canonical validation lets checkpoint and final-output authority share one IndexedDB cut. */
export function validateBrowserTargetProof(raw: unknown, checkpoint: FileCheckpointV2): void {
  if (raw === undefined) throw new BrowserDeliveryConcurrencyError('Saved target requires a committed final output proof')
  const binding = createMaterializationLedgerBindingSync({
    operationId: checkpoint.operationId, receiveIntentDigest: checkpoint.receiveIntentDigest,
    materializationBindingDigest: checkpoint.materializationBindingDigest, authorityRef: checkpoint.authorityRef,
  })
  const proof = decodeMaterializationFinalFileProofV1Sync(raw, binding)
  if (proof.checkpoint.recordDigest !== fileCheckpointDigest(checkpoint)) {
    throw new BrowserDeliveryConcurrencyError('Target final proof differs from the claimed checkpoint')
  }
  validateFinalOutputAgainstCheckpoint(proof.finalOutput, checkpoint)
}
