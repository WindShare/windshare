import { INDEXEDDB_BROWSER_SAVE_POLICY_STORE, INDEXEDDB_BROWSER_DELIVERY_FILE_STORE,
  INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, INDEXEDDB_FILE_FINAL_PROOF_STORE,
  INDEXEDDB_BY_OPERATION_RECORD_PROOF_INDEX, requestResult } from '../../browser/indexeddb-database'
import { readStoredCheckpoint } from '../../browser/indexeddb/repository-transactions'
import { FILE_ID_BYTES, OPERATION_ID_BYTES, type FileCheckpointV2 } from '../../persistence/checkpoint'
import type { MaterializationFinalFileProofV1 } from '../../materialization-ledger/model'
import { finalFileCheckpointProof, type FinalFileCheckpointProof } from '../../persistence/journal'
import { snapshotIdentity } from '../../workspace/canonical'
import { validateBrowserTargetProof, validateDirectCheckpointProof } from '../final-proof'
import { advanceBrowserDeliveryRecord } from '../lifecycle'
import { snapshotBrowserDeliveryRecord, validateBrowserDeliveryRecord } from '../records'
import { validateBrowserSavePolicy } from '../policy'
import type { BrowserDeliveryRecordV1, BrowserSavePolicyV1 } from '../model'
import { BrowserDeliveryConcurrencyError } from '../repository'

export async function createBrowserDeliveryFile(transaction: IDBTransaction, policy: BrowserSavePolicyV1,
  input: BrowserDeliveryRecordV1): Promise<BrowserDeliveryRecordV1> {
  const record = validateBrowserDeliveryRecord(policy, input)
  if (record.generation !== 1n || record.state.kind !== 'receiving' || record.state.checkpoint !== undefined ||
      record.localMutation !== undefined) {
    throw new TypeError('File placement must be persisted before receiving starts')
  }
  const store = transaction.objectStore(INDEXEDDB_BROWSER_DELIVERY_FILE_STORE)
  const raw: unknown = await requestResult(store.get(fileKey(record.operationId, record.fileId)))
  if (raw !== undefined) {
    const existing = validateBrowserDeliveryRecord(policy, raw as BrowserDeliveryRecordV1)
    // Repeated placement acquisition returns its current state, never rewinds received progress.
    const comparable = snapshotBrowserDeliveryRecord(policy, {
      ...record, generation: existing.generation, state: existing.state,
      ...(existing.localMutation === undefined ? {} : { localMutation: existing.localMutation }),
    })
    if (comparable.digest !== existing.digest) throw new BrowserDeliveryConcurrencyError('Started source or placement changed')
    return existing
  }
  await requestResult(store.add(record))
  return record
}

export async function finalizeDirectBrowserDelivery(transaction: IDBTransaction,
  previousInput: BrowserDeliveryRecordV1, proof: FinalFileCheckpointProof): Promise<BrowserDeliveryRecordV1> {
  const store = transaction.objectStore(INDEXEDDB_BROWSER_DELIVERY_FILE_STORE)
  const [policyRaw, currentRaw, checkpointRaw, finalProofRaw] = await Promise.all([
    requestResult<unknown>(transaction.objectStore(INDEXEDDB_BROWSER_SAVE_POLICY_STORE).get(previousInput.operationId)),
    requestResult<unknown>(store.get(fileKey(previousInput.operationId, previousInput.fileId))),
    requestResult<unknown>(transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE).get(proof.recordId)),
    requestResult<unknown>(transaction.objectStore(INDEXEDDB_FILE_FINAL_PROOF_STORE)
      .index(INDEXEDDB_BY_OPERATION_RECORD_PROOF_INDEX).get([previousInput.operationId, proof.recordId])),
  ])
  if (policyRaw === undefined || currentRaw === undefined || checkpointRaw === undefined) {
    throw new BrowserDeliveryConcurrencyError('Direct finalization requires persisted policy, file, and checkpoint authority')
  }
  const checkpoint = readStoredCheckpoint(checkpointRaw)
  validateDirectCheckpointProof(proof, checkpoint)
  return writeDirectCompletion(transaction, previousInput, policyRaw, currentRaw, checkpoint, finalProofRaw)
}

/** The host commits this proof and checkpoint in the same transaction as the delivery CAS. */
export async function commitDirectBrowserDelivery(transaction: IDBTransaction,
  previous: BrowserDeliveryRecordV1, checkpoint: FileCheckpointV2,
  finalProof: MaterializationFinalFileProofV1): Promise<BrowserDeliveryRecordV1> {
  const [policy, current] = await Promise.all([
    requestResult<unknown>(transaction.objectStore(INDEXEDDB_BROWSER_SAVE_POLICY_STORE).get(previous.operationId)),
    requestResult<unknown>(transaction.objectStore(INDEXEDDB_BROWSER_DELIVERY_FILE_STORE).get(fileKey(previous.operationId, previous.fileId))),
  ])
  if (policy === undefined || current === undefined) throw new BrowserDeliveryConcurrencyError('Direct completion lost its policy or placement')
  return writeDirectCompletion(transaction, previous, policy, current, checkpoint, finalProof)
}

async function writeDirectCompletion(transaction: IDBTransaction, previousInput: BrowserDeliveryRecordV1,
  policyRaw: unknown, currentRaw: unknown, checkpoint: FileCheckpointV2, finalProofRaw: unknown): Promise<BrowserDeliveryRecordV1> {
  const policy = validateBrowserSavePolicy(policyRaw as BrowserSavePolicyV1)
  const previous = validateBrowserDeliveryRecord(policy, previousInput)
  const current = validateBrowserDeliveryRecord(policy, currentRaw as BrowserDeliveryRecordV1)
  validateBrowserTargetProof(finalProofRaw, checkpoint)
  if (previous.placement === 'direct' && previous.state.kind === 'cleaned') {
    validateDirectCheckpointProof(finalFileCheckpointProof(previous.state.target), checkpoint)
    if (current.digest !== previous.digest) throw new BrowserDeliveryConcurrencyError()
    return current
  }
  if (previous.placement !== 'direct' || previous.state.kind !== 'receiving') {
    throw new TypeError('Only direct receiving can finalize without staging cleanup')
  }
  const saved = advanceBrowserDeliveryRecord(policy, previous, { kind: 'target-saved', target: checkpoint })
  const cleaned = advanceBrowserDeliveryRecord(policy, saved, { kind: 'cleaned', target: checkpoint })
  if (current.digest === cleaned.digest) return current
  if (current.digest !== previous.digest) throw new BrowserDeliveryConcurrencyError()
  await requestResult(transaction.objectStore(INDEXEDDB_BROWSER_DELIVERY_FILE_STORE).put(cleaned))
  return cleaned
}

export function fileKey(operationId: string, fileId: string): IDBValidKey[] {
  return [
    snapshotIdentity(operationId, OPERATION_ID_BYTES, 'operation ID'),
    snapshotIdentity(fileId, FILE_ID_BYTES, 'file ID'),
  ]
}

export async function requirePolicy(transaction: IDBTransaction, operationId: string): Promise<BrowserSavePolicyV1> {
  const raw: unknown = await requestResult(transaction.objectStore(INDEXEDDB_BROWSER_SAVE_POLICY_STORE)
    .get(snapshotIdentity(operationId, OPERATION_ID_BYTES, 'operation ID')))
  if (raw === undefined) throw new BrowserDeliveryConcurrencyError('Browser save policy must commit before receiving')
  return validateBrowserSavePolicy(raw as BrowserSavePolicyV1)
}
