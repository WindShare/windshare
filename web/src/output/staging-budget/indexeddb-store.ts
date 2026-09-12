import { CAPACITY_INVENTORY_BOUND, STAGING_FILE_STORE,
  WORKSPACE_CLAIM_STORE, capacityRequest, capacityTransaction,
  openOriginCapacityDatabase } from '../origin-private/capacity/database'
import { workspaceCapacityAccount, stagingFileCapacity } from '../origin-private/capacity/records'
import { requireCapacityLength } from '../origin-private/object-capacity'
import type { StagingBudgetInventory, StagingBudgetMutation, StagingBudgetStore } from './contracts'

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
    return capacityTransaction(this.#database, async transaction => {
      const [stagingValues, workspaceValues] = await Promise.all([
        capacityRequest<unknown[]>(transaction.objectStore(STAGING_FILE_STORE)
          .getAll(undefined, CAPACITY_INVENTORY_BOUND + 1)),
        capacityRequest<unknown[]>(transaction.objectStore(WORKSPACE_CLAIM_STORE)
          .getAll(undefined, CAPACITY_INVENTORY_BOUND + 1)),
      ])
      if (stagingValues.length > CAPACITY_INVENTORY_BOUND || workspaceValues.length > CAPACITY_INVENTORY_BOUND) {
        throw new DOMException('Origin capacity inventory exceeds its bound', 'QuotaExceededError')
      }
      const records = stagingValues.map(stagingFileCapacity)
      const workspace = workspaceValues.map(workspaceCapacityAccount)
      const mutation = update({ records, workspace: {
        occupiedBytes: workspace.reduce((total, record) => requireCapacityLength(total + record.occupiedBytes), 0n),
        outstandingBytes: workspace.reduce((total, record) => requireCapacityLength(
          total + record.outstandingGrowthBytes + record.metadataHeadroomBytes), 0n),
      } })
      const store = transaction.objectStore(STAGING_FILE_STORE)
      if (mutation.put !== undefined) store.put(stagingFileCapacity(mutation.put))
      if (mutation.deleteId !== undefined) store.delete(mutation.deleteId)
      for (const record of mutation.puts ?? []) store.put(stagingFileCapacity(record))
      for (const id of mutation.deleteIds ?? []) store.delete(id)
      return mutation.result
    })
  }

  close(): void { this.#database.close() }
}
