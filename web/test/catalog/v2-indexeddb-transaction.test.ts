import { describe, expect, it } from 'vitest'

import {
  isIndexedDbConstraintError,
  waitForIndexedDbTransaction,
} from '../../src/catalog/v2-indexeddb-transaction'

const ABORT_CAUSES: readonly (readonly [
  scenario: string,
  cause: DOMException | null,
  ownershipCollision: boolean,
])[] = [
  ['constraint', new DOMException('unique ownership failed', 'ConstraintError'), true],
  ['quota', new DOMException('profile quota failed', 'QuotaExceededError'), false],
  ['I/O', new DOMException('storage engine failed', 'UnknownError'), false],
  ['caller abort', null, false],
]

describe('IndexedDB transaction completion', () => {
  it('waits for abort before reading the transaction error', async () => {
    const transaction = new TransactionHarness()
    const completion = waitForIndexedDbTransaction(transaction.value)
    let settled = false
    const observed = completion.then(
      () => { settled = true },
      () => { settled = true },
    )

    transaction.dispatch('error')
    await Promise.resolve()
    expect(settled).toBe(false)

    const cause = new DOMException('unique ownership failed', 'ConstraintError')
    const rejection = expect(completion).rejects.toBe(cause)
    transaction.error = cause
    transaction.dispatch('abort')
    await rejection
    await observed
  })

  it('resolves only when the transaction completes', async () => {
    const transaction = new TransactionHarness()
    const completion = waitForIndexedDbTransaction(transaction.value)

    transaction.dispatch('complete')

    await expect(completion).resolves.toBeUndefined()
  })

  it.each(ABORT_CAUSES)(
    'preserves the exact %s abort cause',
    async (_scenario, cause, ownershipCollision) => {
      const transaction = new TransactionHarness()
      const completion = waitForIndexedDbTransaction(transaction.value)
      const rejection = expect(completion).rejects.toBe(cause)

      transaction.error = cause
      transaction.dispatch('abort')

      await rejection
      expect(isIndexedDbConstraintError(cause)).toBe(ownershipCollision)
    },
  )

  it('does not accept a ConstraintError-shaped non-DOM failure as IndexedDB causality', () => {
    expect(isIndexedDbConstraintError({ name: 'ConstraintError' })).toBe(false)
  })
})

class TransactionHarness extends EventTarget {
  error: DOMException | null = null

  get value(): IDBTransaction {
    return this as unknown as IDBTransaction
  }

  dispatch(type: 'abort' | 'complete' | 'error'): void {
    this.dispatchEvent(new Event(type))
  }
}
