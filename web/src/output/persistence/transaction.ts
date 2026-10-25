import { transactionCompletion } from '../browser/indexeddb-database'

/** Metadata, ownership and lifecycle participants share the same commit/abort boundary. */
export async function commitOutputTransaction<T>(
  database: IDBDatabase,
  stores: readonly string[],
  mutate: (transaction: IDBTransaction) => Promise<T>,
): Promise<T> {
  const transaction = database.transaction([...stores], 'readwrite', { durability: 'strict' })
  const completion = transactionCompletion(transaction)
  // A request can fail before mutate returns; observe the abort immediately.
  completion.catch(() => undefined)
  try {
    const result = await mutate(transaction)
    await completion
    return result
  } catch (error) {
    try { transaction.abort() } catch { /* A completed or aborted transaction is already settled. */ }
    await completion.catch(() => undefined)
    throw error
  }
}
