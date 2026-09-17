import { describe, expect, it } from 'vitest'
import { ReceiveRateSampler, TransferRateSampler } from '../../src/ui/tasks/receive-rate'

describe('receive rate and remaining time', () => {
  it('shows real traffic before the first write without treating it as completed work or an ETA', () => {
    const sampler = new TransferRateSampler(0, { receivedObjectBytes: 0n, writtenBytes: 0n })
    for (let second = 1; second <= 8; second++) {
      expect(sampler.sample(second * 1000, {
        receivedObjectBytes: BigInt(second * 128 * 1024), writtenBytes: 0n,
      }, 8n * 1024n * 1024n)).toEqual({ bytesPerSecond: 128n * 1024n, remainingSeconds: null })
    }
    // Replayed traffic remains activity but cannot establish useful completion speed.
    for (let second = 9; second <= 15; second++) {
      expect(sampler.sample(second * 1000, {
        receivedObjectBytes: BigInt(second * 128 * 1024), writtenBytes: 0n,
      }, 8n * 1024n * 1024n)?.remainingSeconds).toBeNull()
    }
    for (let second = 16; second <= 22; second++) {
      const sample = sampler.sample(second * 1000, {
        receivedObjectBytes: 15n * 128n * 1024n, writtenBytes: 0n,
      }, null)
      if (second === 22) expect(sample?.bytesPerSecond).toBe(0n)
    }
  })


  it('shows throughput while totals are unknown and estimates only after stable samples', () => {
    const sampler = new ReceiveRateSampler(0, 0n)
    expect(sampler.sample(1000, 100n, null)).toEqual({ bytesPerSecond: 100n, remainingSeconds: null })
    expect(sampler.sample(2000, 200n, 800n)?.remainingSeconds).toBeNull()
    expect(sampler.sample(3000, 300n, 700n)).toEqual({ bytesPerSecond: 100n, remainingSeconds: 7 })
  })

  it('drops the estimate on a stall and lets throughput decay to zero', () => {
    const sampler = new ReceiveRateSampler(0, 0n)
    for (let second = 1; second <= 3; second++) sampler.sample(second * 1000, BigInt(second * 100), 1000n)
    expect(sampler.sample(4000, 300n, 1000n)?.remainingSeconds).toBeNull()
    let sample
    for (let second = 5; second <= 9; second++) sample = sampler.sample(second * 1000, 300n, 1000n)
    expect(sample).toEqual({ bytesPerSecond: 0n, remainingSeconds: null })
  })

  it('does not count a retained starting position or a reset counter as new receipt', () => {
    const sampler = new ReceiveRateSampler(0, 900n)
    expect(sampler.sample(1000, 1000n, 100n)?.bytesPerSecond).toBe(100n)
    expect(sampler.sample(2000, 0n, 100n)).toBeNull()
    expect(sampler.sample(3000, 10n, 90n)?.bytesPerSecond).toBe(10n)
    expect(sampler.sample(3000, 10n, 90n)).toBeNull()
  })

  it('withholds whole-task estimates for unstable rates, unknown totals, and completed payload', () => {
    const sampler = new ReceiveRateSampler(0, 0n)
    sampler.sample(1000, 100n, 1000n)
    sampler.sample(2000, 200n, 1000n)
    expect(sampler.sample(3000, 1200n, 1000n)?.remainingSeconds).toBeNull()
    expect(sampler.sample(4000, 1300n, null)?.remainingSeconds).toBeNull()
    expect(sampler.sample(5000, 1400n, 0n)?.remainingSeconds).toBeNull()
  })
})
