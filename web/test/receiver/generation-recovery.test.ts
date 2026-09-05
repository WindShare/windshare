import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  GenerationRecoveryBudget, GenerationRecoveryExhaustedError, runGenerationRecovery,
} from '../../src/receiver/generation-recovery'

afterEach(() => vi.useRealTimers())

describe('session generation recovery authority', () => {
  it('bounds each wave and carries replenishing attempt capacity across installed generations', () => {
    const ledger = new GenerationRecoveryBudget()
    const first = ledger.openWave(0)
    for (let attempt = 0; attempt < 4; attempt += 1) first.reserve(0).finish(0)
    expect(() => first.reserve(0)).toThrow(GenerationRecoveryExhaustedError)
    const second = ledger.openWave(0)
    for (let attempt = 0; attempt < 4; attempt += 1) second.reserve(0).finish(0)
    expect(() => ledger.openWave(0).reserve(0)).toThrow(GenerationRecoveryExhaustedError)
    expect(ledger.openWave(600_000).reserve(600_000).milliseconds).toBe(45_000)
    expect(() => ledger.openWave(0).reserve(0)).toThrow('monotonic')
  })

  it('does not restart the wave deadline at a new handshake', () => {
    const wave = new GenerationRecoveryBudget().openWave(0)
    const first = wave.reserve(0)
    first.finish(45_000)
    const second = wave.reserve(110_000)
    expect(second.milliseconds).toBe(10_000)
    second.finish(120_000)
    expect(() => wave.reserve(120_000)).toThrow(GenerationRecoveryExhaustedError)
  })

  it('ends an unresponsive handshake at the reserved deadline and closes a late authenticated result', async () => {
    vi.useFakeTimers()
    const ledger = new GenerationRecoveryBudget()
    let complete!: (value: string) => void
    const work = new Promise<string>((resolve) => { complete = resolve })
    const close = vi.fn(async () => undefined)
    const task = runGenerationRecovery({
      reservation: ledger.openWave(0).reserve(0), parent: new AbortController().signal,
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
