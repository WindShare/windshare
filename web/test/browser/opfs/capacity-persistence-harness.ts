import {
  openOriginCapacityDatabase, capacityRequest, capacityTransaction, capacityTransactionCompletion,
  WORKSPACE_CLAIM_STORE, STAGING_FILE_STORE, WORKSPACE_OBJECT_STORE,
} from '../../../src/output/origin-private/capacity/database'
import { IndexedDbOriginPrivateWorkspaceBudgetLeaseAuthority } from '../../../src/output/origin-private/admission-authority'
import { IndexedDbStagingBudgetStore } from '../../../src/output/staging-budget/indexeddb-store'
import { openCapacitySession, capacityAction, closeCapacitySessions, deleteCapacityDatabase } from './opfs-capacity-harness'

const OLD_SCHEMA = 3
const EXPIRED = {
  id: 'expired', operationId: 'expired', token: 'old', budgetDigest: 'digest',
  peakOwnedBytes: 339260n, expiresAtMilliseconds: 1, occupiedBytes: 10n,
  outstandingGrowthBytes: 20n, metadataHeadroomBytes: 2n,
}

async function readAll(database: IDBDatabase, store: string): Promise<unknown[]> {
  const tx = database.transaction(store)
  const [rows] = await Promise.all([
    capacityRequest<unknown[]>(tx.objectStore(store).getAll()), capacityTransactionCompletion(tx),
  ])
  return rows
}

export async function incompatibleSchema() {
  const name = crypto.randomUUID()
  const request = indexedDB.open(name, OLD_SCHEMA)
  request.onupgradeneeded = () => request.result.createObjectStore(WORKSPACE_CLAIM_STORE, { keyPath: 'id' })
  const legacy = await capacityRequest(request)
  const tx = legacy.transaction(WORKSPACE_CLAIM_STORE, 'readwrite')
  await Promise.all([
    capacityRequest(tx.objectStore(WORKSPACE_CLAIM_STORE).put(EXPIRED)), capacityTransactionCompletion(tx),
  ])
  legacy.close()
  try {
    const failure = await failureName(async () => { (await openOriginCapacityDatabase(name)).close() })
    const preserved = await capacityRequest(indexedDB.open(name))
    const version = preserved.version
    const rows = await readAll(preserved, WORKSPACE_CLAIM_STORE)
    preserved.close()
    await deleteCapacityDatabase(name)
    const fresh = await openOriginCapacityDatabase(name)
    const freshRows = await readAll(fresh, WORKSPACE_CLAIM_STORE)
    fresh.close()
    return { failure, version, preserved: rows.length, fresh: freshRows.length }
  } finally { await deleteCapacityDatabase(name) }
}

export async function corruptWorkspace(kind: 'legacy' | 'nan') {
  const name = crypto.randomUUID()
  await openCapacitySession(name, 0, 'new', 10)
  const database = await openOriginCapacityDatabase(name)
  try {
    const corrupt: Record<string, unknown> = { ...EXPIRED }
    if (kind === 'legacy') {
      delete corrupt.occupiedBytes
      delete corrupt.outstandingGrowthBytes
      delete corrupt.metadataHeadroomBytes
    } else {
      Object.assign(corrupt, { occupiedBytes: NaN, token: '', expiresAtMilliseconds: 0 })
    }
    await capacityTransaction(database, async tx => {
      tx.objectStore(WORKSPACE_CLAIM_STORE).put(corrupt)
    })
    const claim = await capacityAction({ kind: 'claim', token: 'new', now: 10 })
    const authority = await IndexedDbOriginPrivateWorkspaceBudgetLeaseAuthority.open(name)
    const release = await failureName(() => authority.release('expired', kind === 'legacy' ? 'old' : ''))
    authority.close()
    const staging = await IndexedDbStagingBudgetStore.open(name)
    let entered = false
    const stage = await failureName(() => staging.transact(() => { entered = true; return { result: null } }))
    staging.close()
    const rows = await readAll(database, WORKSPACE_CLAIM_STORE)
    const row = rows[0] as Record<string, unknown>
    return { claim, release, stage, entered, rows: rows.length,
      preserved: kind === 'legacy' ? !('occupiedBytes' in row) : Number.isNaN(row.occupiedBytes) }
  } finally {
    database.close()
    closeCapacitySessions()
    await deleteCapacityDatabase(name)
  }
}

export async function expiryRollback() {
  const name = crypto.randomUUID()
  await openCapacitySession(name, 0, 'new', 10)
  const database = await openOriginCapacityDatabase(name)
  try {
    await capacityTransaction(database, async tx => {
      tx.objectStore(WORKSPACE_CLAIM_STORE).put(EXPIRED)
      tx.objectStore(STAGING_FILE_STORE).put({ id: 'broken-staging' })
    })
    const failure = await capacityAction({ kind: 'claim', token: 'new', now: 10 })
    const rows = await readAll(database, WORKSPACE_CLAIM_STORE)
    const row = rows[0] as typeof EXPIRED
    return { failure, rows: rows.length, token: row.token, occupied: String(row.occupiedBytes),
      outstanding: String(row.outstandingGrowthBytes), expires: row.expiresAtMilliseconds }
  } finally {
    database.close()
    closeCapacitySessions()
    await deleteCapacityDatabase(name)
  }
}

export async function settlementRollback() {
  const name = crypto.randomUUID()
  await openCapacitySession(name, 0, 'owner')
  const database = await openOriginCapacityDatabase(name)
  try {
    await capacityAction({ kind: 'claim', token: 'owner' })
    await capacityAction({ kind: 'reserve', token: 'owner', target: '100' })
    // Individually valid rows disagree: settling queues an object update before
    // the account delta detects the inconsistency. Neither update may commit.
    await capacityTransaction(database, async tx => {
      const store = tx.objectStore(WORKSPACE_CLAIM_STORE)
      const rows = await capacityRequest<Record<string, unknown>[]>(store.getAll())
      store.put({ ...rows[0], outstandingGrowthBytes: 0n })
    })
    const before = await readAll(database, WORKSPACE_OBJECT_STORE)
    const failure = await capacityAction({ kind: 'settle', token: 'owner', current: '100' })
    const after = await readAll(database, WORKSPACE_OBJECT_STORE)
    const serialize = (value: unknown) => JSON.stringify(value, (_, entry: unknown) =>
      typeof entry === 'bigint' ? entry.toString() : entry)
    return { failure, unchanged: serialize(before) === serialize(after) }
  } finally {
    database.close()
    closeCapacitySessions()
    await deleteCapacityDatabase(name)
  }
}

async function failureName(operation: () => Promise<unknown>): Promise<string> {
  try { await operation(); return 'accepted' }
  catch (error) { return error instanceof Error ? error.name : String(error) }
}
