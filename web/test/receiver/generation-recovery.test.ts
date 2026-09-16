import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  GenerationRecoveryBudget, GenerationRecoveryExhaustedError, runGenerationRecovery,
} from '../../src/receiver/generation-recovery'

afterEach(() => vi.useRealTimers())

describe('session generation recovery authority', () => {
  it('charges fast failures and carries replenishing attempt capacity across installed generations', () => {
    const ledger = new GenerationRecoveryBudget()
    for (let attempt = 0; attempt < 8; attempt += 1) {
      const reservation = ledger.reserve(0)
      reservation.finish(0)
      reservation.finish(0)
    }
    expect(() => ledger.reserve(0)).toThrow(GenerationRecoveryExhaustedError)
    expect(ledger.nextCapacityMilliseconds(0)).toBe(75_000)
    expect(() => ledger.reserve(74_999)).toThrow(GenerationRecoveryExhaustedError)
    expect(ledger.reserve(75_000).milliseconds).toBe(45_000)
    expect(() => ledger.reserve(0)).toThrow('monotonic')
  })

  it('waits for a full handshake allowance when the time budget is depleted', () => {
    const ledger = new GenerationRecoveryBudget()
    const reservations = Array.from({ length: 4 }, () => ledger.reserve(0))
    for (const reservation of reservations) reservation.finish(45_000)
    expect(ledger.nextCapacityMilliseconds(45_000)).toBe(105_000)
    expect(() => ledger.reserve(149_999)).toThrow(GenerationRecoveryExhaustedError)
    expect(ledger.reserve(150_000).milliseconds).toBe(45_000)
  })

  it('ends an unresponsive handshake at the reserved deadline and closes a late authenticated result', async () => {
    vi.useFakeTimers()
    const ledger = new GenerationRecoveryBudget()
    let complete!: (value: string) => void
    const work = new Promise<string>((resolve) => { complete = resolve })
    const close = vi.fn(async () => undefined)
    const task = runGenerationRecovery({
      reservation: ledger.reserve(0), parent: new AbortController().signal,
      now: () => 45_000, connect: async () => work, close,
    })
    const rejected = expect(task).rejects.toThrow('timed out')
    await vi.advanceTimersByTimeAsync(45_000)
    await rejected
    complete('late-session')
    await work
    await Promise.resolve()
    await Promise.resolve()
    expect(close).toHaveBeenCalledWith('late-session')
    expect(vi.getTimerCount()).toBe(0)
  })
})
