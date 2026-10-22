import { describe, expect, it } from 'vitest'
import { BlockReceiveWindow } from '../../src/content/scheduling/receive-window'

const MIB = 1024 * 1024
const FRAGMENT = 64 * 1024

describe('physical lane receive admission', () => {
  it('leaves cold and slow work public until unfinished bytes fit the observed window', () => {
    let now = 0
    const window = new BlockReceiveWindow(() => now)
    const first = window.tryAcquire(MIB)!
    expect(window.tryAcquire(MIB)).toBeUndefined()
    for (let fragment = 0; fragment < 14; fragment++) {
      now += 500
      first.receive(FRAGMENT)
      expect(window.tryAcquire(MIB)).toBeUndefined()
    }
    now += 500
    first.receive(FRAGMENT)
    const next = window.tryAcquire(MIB)!
    expect(next).toBeDefined()
    first.close(); next.close()
    expect(window.available).toBe(true)
  })

  it('replenishes response capacity before authentication releases its permit', () => {
    let now = 0
    const window = new BlockReceiveWindow(() => now)
    const first = window.tryAcquire(MIB)!
    now = 1
    first.receive(FRAGMENT)
    const next = Array.from({ length: 7 }, () => window.tryAcquire(MIB))
    expect(next.every(permit => permit !== undefined)).toBe(true)
    first.close()
    for (const permit of next) permit!.close()
  })

  it('separates response delay from streaming capacity', () => {
    let now = 0
    const window = new BlockReceiveWindow(() => now)
    const first = window.tryAcquire(MIB)!
    now = 200
    first.receive(FRAGMENT)
    expect(window.tryAcquire(MIB)).toBeUndefined()
    now++
    first.receive(FRAGMENT)
    const next = window.tryAcquire(MIB, 0)!
    expect(next.admission.waitedMilliseconds).toBe(201)
    first.close(); next.close()
  })

  it('notifies allocation on receipts and releases without owning a task queue', () => {
    const window = new BlockReceiveWindow(() => 0)
    let wakes = 0
    const unsubscribe = window.subscribe(() => { wakes++ })
    const first = window.tryAcquire(MIB)!
    first.receive(FRAGMENT)
    first.close()
    first.close()
    first.receive(FRAGMENT)
    expect(wakes).toBe(2)
    unsubscribe()
    window.tryAcquire(MIB)!.close()
    expect(wakes).toBe(2)
    expect(() => window.tryAcquire(0)).toThrow(RangeError)
  })

  it('does not reuse stale fast capacity after an idle period', () => {
    let now = 0
    const window = new BlockReceiveWindow(() => now)
    const first = window.tryAcquire(MIB)!
    now = 1
    first.receive(FRAGMENT)
    first.close()
    now = 5000
    const next = window.tryAcquire(MIB)!
    expect(window.tryAcquire(MIB)).toBeUndefined()
    next.close()
    window.tryAcquire(MIB)!.close()
  })
})
