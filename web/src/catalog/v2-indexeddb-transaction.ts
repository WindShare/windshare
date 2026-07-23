/**
 * Waits for the transaction's terminal outcome.
 *
 * Request errors bubble through the transaction before abort finalization, when
 * `transaction.error` may still be null. Reading it only from the terminal abort
 * event preserves the browser's actual ConstraintError, storage error, or null
 * caller-abort cause for the owning catalog operation.
 */
export function waitForIndexedDbTransaction(transaction: IDBTransaction): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    transaction.addEventListener('complete', () => resolve(), { once: true })
    transaction.addEventListener('abort', () => reject(transaction.error), { once: true })
  })
}

export function isIndexedDbConstraintError(error: unknown): error is DOMException {
  return error instanceof DOMException && error.name === 'ConstraintError'
}
