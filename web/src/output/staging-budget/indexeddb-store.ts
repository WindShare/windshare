import { CAPACITY_INVENTORY_BOUND, ORIGIN_CAPACITY_STORES, STAGING_FILE_STORE,
  WORKSPACE_CLAIM_STORE, capacityRequest, capacityTransactionCompletion,
  openOriginCapacityDatabase } from '../origin-private/capacity/database'
import type { StagingBudgetInventory, StagingBudgetMutation, StagingBudgetRecord,
  StagingBudgetStore } from './contracts'

interface WorkspaceAccount {
  readonly occupiedBytes: bigint
  readonly outstandingGrowthBytes: bigint
  readonly metadataHeadroomBytes: bigint
}

/** Sharing the workspace transaction scope makes ZIP growth and staged-file admission mutually visible. */
export class IndexedDbStagingBudgetStore implements StagingBudgetStore {
  readonly coordinationScope = 'origin' as const
  readonly #database: IDBDatabase
  private constructor(database: IDBDatabase) {
    this.#database = database
    database.addEventListener('versionchange', () => this.close())
  }

  static async open(databaseName?: string): Promise<IndexedDbStagingBudgetStore> {
    return new IndexedDbStagingBudgetStore(await openOriginCapacityDatabase(databaseName))
  }

  async transact<T>(update: (inventory: StagingBudgetInventory) => StagingBudgetMutation<T>): Promise<T> {
    const transaction = this.#database.transaction(ORIGIN_CAPACITY_STORES, 'readwrite', { durability: 'strict' })
    const completion = capacityTransactionCompletion(transaction)
    // Attach before requests so an abort is always observed, even if the pure update rejects.
    completion.catch(() => undefined)
    try {
      const [records, workspace] = await Promise.all([
        capacityRequest<StagingBudgetRecord[]>(transaction.objectStore(STAGING_FILE_STORE)
          .getAll(undefined, CAPACITY_INVENTORY_BOUND + 1)),
        capacityRequest<WorkspaceAccount[]>(transaction.objectStore(WORKSPACE_CLAIM_STORE)
          .getAll(undefined, CAPACITY_INVENTORY_BOUND + 1)),
      ])
      if (records.length > CAPACITY_INVENTORY_BOUND || workspace.length > CAPACITY_INVENTORY_BOUND) {
        throw new DOMException('Origin capacity inventory exceeds its bound', 'QuotaExceededError')
      }
      const mutation = update({ records, workspace: {
        occupiedBytes: workspace.reduce((total, record) => total + record.occupiedBytes, 0n),
        outstandingBytes: workspace.reduce((total, record) =>
          total + record.outstandingGrowthBytes + record.metadataHeadroomBytes, 0n),
      } })
      const store = transaction.objectStore(STAGING_FILE_STORE)
      if (mutation.put !== undefined) store.put(mutation.put)
      if (mutation.deleteId !== undefined) store.delete(mutation.deleteId)
      for (const record of mutation.puts ?? []) store.put(record)
      for (const id of mutation.deleteIds ?? []) store.delete(id)
      await completion
      return mutation.result
    } catch (error) {
      try { transaction.abort() } catch { /* A failed commit may already have ended the transaction. */ }
      throw error
    }
  }

  close(): void { this.#database.close() }
}
