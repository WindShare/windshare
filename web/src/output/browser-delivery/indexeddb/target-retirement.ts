import {
  INDEXEDDB_BROWSER_DELIVERY_FILE_STORE, INDEXEDDB_BROWSER_SAVE_POLICY_STORE,
  INDEXEDDB_BY_OPERATION_FILE_INDEX, requestResult,
} from '../../browser/indexeddb-database'
import type { MaterializationLedgerBindingV1 } from '../../materialization-ledger/model'
import type { BrowserDeliveryRecordV1, BrowserSavePolicyV1 } from '../model'
import { validateBrowserSavePolicy } from '../policy'
import { validateBrowserDeliveryRecord } from '../records'

/** Child local saves still depend on the target namespace, including ancestor ownership and final proofs. */
export async function browserDeliveryRetainsTargetMetadata(
  transaction: IDBTransaction,
  binding: MaterializationLedgerBindingV1,
): Promise<boolean> {
  const raw = await requestResult<BrowserSavePolicyV1>(
    transaction.objectStore(INDEXEDDB_BROWSER_SAVE_POLICY_STORE).get(binding.operationId))
  if (raw === undefined) return false
  const policy = validateBrowserSavePolicy(raw)
  if (policy.receiveIntentDigest !== binding.receiveIntentDigest ||
      policy.target.materializationBindingDigest !== binding.materializationBindingDigest ||
      policy.target.authorityRef !== binding.authorityRef) {
    throw new TypeError('Target metadata retirement escaped its browser delivery policy')
  }
  if (policy.staging === undefined) return false
  return new Promise((resolve, reject) => {
    const request = transaction.objectStore(INDEXEDDB_BROWSER_DELIVERY_FILE_STORE)
      .index(INDEXEDDB_BY_OPERATION_FILE_INDEX).openCursor(
        IDBKeyRange.bound([binding.operationId], [binding.operationId, []]))
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      const cursor = request.result
      if (cursor === null) { resolve(false); return }
      try {
        const record = validateBrowserDeliveryRecord(policy, cursor.value as BrowserDeliveryRecordV1)
        if (record.placement === 'staged' && (record.state.kind === 'receiving' ||
            record.state.kind === 'staged-complete' || record.state.kind === 'copying')) {
          resolve(true)
        } else cursor.continue()
      } catch (error) { reject(error) }
    })
  })
}
