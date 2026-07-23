import { describe, expect, it } from 'vitest'

import { BoundedTaskPool } from '../../src/transfer/bounded-task-pool'

describe('bounded task admission', () => {
  it('retains only active tasks and requires the caller to keep compact pending state', async () => {
    const pool = new BoundedTaskPool(2)
    const first = deferred<void>()
    const second = deferred<void>()
    let active = 0
    let maximumActive = 0
    const gated = async (gate: Promise<void>) => {
      active += 1
      maximumActive = Math.max(maximumActive, active)
      await gate
      active -= 1
    }

    pool.run(() => gated(first.promise))
    pool.run(() => gated(second.promise))
    await Promise.resolve()
    expect(pool.hasCapacity).toBe(false)
    expect(() => pool.run(async () => undefined)).toThrow(/available slot/u)

    first.resolve()
    await pool.waitForCapacity()
    pool.run(async () => undefined)
    second.resolve()
    await pool.drain()

    expect(maximumActive).toBe(2)
    expect(pool.hasCapacity).toBe(true)
  })

  it('retains the first task failure until authoritative drain', async () => {
    const pool = new BoundedTaskPool(1)
    const failure = new Error('fatal task')
    pool.run(async () => { throw failure })

    await expect(pool.drain()).rejects.toBe(failure)
    await expect(pool.settle()).resolves.toBeUndefined()
  })
})

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((innerResolve) => {
    resolve = innerResolve
  })
  return { promise, resolve }
}
