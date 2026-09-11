import { describe, expect, it } from 'vitest'
import { decideFileReceivingPlacement, UNKNOWN_SPEED_STAGING_THRESHOLD_BYTES,
  SMALL_FILE_DIRECT_MAXIMUM_BYTES } from '../../../src/output/planning/file-receiving-placement'
import { browserStagingQuota, type BrowserStagingStorageFacts } from '../../../src/output/planning/staging-storage'
import { RecoveryCostObserver } from '../../../src/output/planning/recovery-cost'

const MIB = 1024n ** 2n
const storage: BrowserStagingStorageFacts = { opfs: 'usable', persistence: 'not-persisted',
  quota: { kind: 'unknown' }, pressure: 'normal' }

describe('authenticated per-file receiving placement', () => {
  it('uses its named unknown-speed boundary independently of folder totals or persistence permission', () => {
    const decide = (exactSize: bigint) => decideFileReceivingPlacement({ exactSize, preference: 'automatic', storage })
    expect(decide(UNKNOWN_SPEED_STAGING_THRESHOLD_BYTES - 1n).placement).toBe('direct')
    expect(decide(UNKNOWN_SPEED_STAGING_THRESHOLD_BYTES)).toMatchObject({
      placement: 'staged', reason: 'unknown-speed-large-file',
    })
    for (let index = 0; index < 100; index++) {
      expect(decide(SMALL_FILE_DIRECT_MAXIMUM_BYTES).placement).toBe('direct')
    }
  })

  it('refines unopened files using reliable receive, local copy, and flush costs', () => {
    const input = { exactSize: 512n * MIB, preference: 'automatic' as const, storage }
    expect(decideFileReceivingPlacement({ ...input, costs: {
      receivedBytesPerSecond: 100n * MIB, copiedBytesPerSecond: 100n * MIB, flushMilliseconds: 1,
    } }).reason).toBe('short-receive')
    expect(decideFileReceivingPlacement({ ...input, costs: {
      receivedBytesPerSecond: MIB, copiedBytesPerSecond: 100n * MIB, flushMilliseconds: 1,
    } }).placement).toBe('staged')
    expect(decideFileReceivingPlacement({ ...input, costs: {
      receivedBytesPerSecond: MIB, copiedBytesPerSecond: MIB, flushMilliseconds: 1,
    } }).reason).toBe('local-recovery-cost-exceeds-receive-benefit')
    expect(decideFileReceivingPlacement({ ...input, costs: {
      receivedBytesPerSecond: MIB, copiedBytesPerSecond: 100n * MIB, flushMilliseconds: 1_000_000,
    } }).placement).toBe('direct')
  })

  it('keeps explicit direct and persisted file placement immutable through capability changes', () => {
    const exactSize = UNKNOWN_SPEED_STAGING_THRESHOLD_BYTES
    expect(decideFileReceivingPlacement({ exactSize, preference: 'direct', storage }).reason).toBe('explicit-direct')
    expect(decideFileReceivingPlacement({ exactSize, preference: 'automatic', storage,
      retainedPlacement: 'direct' })).toMatchObject({ placement: 'direct', reason: 'retained-placement' })
    expect(decideFileReceivingPlacement({ exactSize, preference: 'automatic',
      storage: { ...storage, opfs: 'unavailable', pressure: 'drain-first' },
      retainedPlacement: 'staged' })).toMatchObject({ placement: 'staged', reason: 'retained-placement' })
    expect(decideFileReceivingPlacement({ exactSize, preference: 'automatic',
      storage: { ...storage, pressure: 'drain-first' } }).reason).toBe('storage-pressure')
    expect(decideFileReceivingPlacement({ exactSize, preference: 'automatic',
      storage: { ...storage, opfs: 'unavailable' } }).reason).toBe('opfs-unavailable')
    expect(() => decideFileReceivingPlacement({ exactSize: -1n, preference: 'automatic', storage })).toThrow()
  })

  it('keeps advisory quota unknown when usage is absent or invalid', () => {
    expect(browserStagingQuota({ quota: 1000 })).toEqual({ kind: 'unknown' })
    expect(browserStagingQuota({ quota: Infinity, usage: 0 })).toEqual({ kind: 'unknown' })
    expect(browserStagingQuota({ quota: 1000, usage: 100 })).toEqual({
      kind: 'estimated', usageBytes: 100n, quotaBytes: 1000n,
    })
  })
})

describe('runtime recovery cost evidence', () => {
  it('requires a stable new-receipt window and expires idle, variable, and reset observations', () => {
    const costs = new RecoveryCostObserver()
    costs.observeReceipt({ atMilliseconds: 0, newReceivedBytes: 0n })
    costs.observeReceipt({ atMilliseconds: 1000, newReceivedBytes: MIB })
    expect(costs.snapshot(1000).receivedBytesPerSecond).toBeNull()
    costs.observeReceipt({ atMilliseconds: 3000, newReceivedBytes: 3n * MIB })
    expect(costs.snapshot(3000).receivedBytesPerSecond).toBe(MIB)
    expect(costs.snapshot(14_000).receivedBytesPerSecond).toBeNull()
    costs.observeReceipt({ atMilliseconds: 4000, newReceivedBytes: 3n * MIB })
    expect(costs.snapshot(4000).receivedBytesPerSecond).toBeNull()
    costs.observeReceipt({ atMilliseconds: 5000, newReceivedBytes: 0n })
    expect(costs.snapshot(5000).receivedBytesPerSecond).toBeNull()
    expect(() => costs.observeReceipt({ atMilliseconds: -1, newReceivedBytes: 0n })).toThrow()
  })

  it.each([40, 50, 100, 1000])('preserves a reliable horizon at %i receipt callbacks per second', (frequency) => {
    const observer = new RecoveryCostObserver()
    const bytesPerSecond = 100n * MIB
    for (let tick = 0; tick <= frequency * 20; tick++) {
      observer.observeReceipt({ atMilliseconds: tick * 1000 / frequency,
        newReceivedBytes: BigInt(tick) * bytesPerSecond / BigInt(frequency) })
    }
    const costs = observer.snapshot(20_000)
    expect(costs.receivedBytesPerSecond).toBe(bytesPerSecond)
    expect(decideFileReceivingPlacement({ exactSize: 1024n * MIB, preference: 'automatic', storage, costs }))
      .toMatchObject({ placement: 'direct', reason: 'short-receive', estimatedReceiveMilliseconds: 10_240n })
    observer.observeReceipt({ atMilliseconds: 21_000, newReceivedBytes: 20n * bytesPerSecond })
    expect(observer.snapshot(21_000).receivedBytesPerSecond).toBeNull()
    observer.observeReceipt({ atMilliseconds: 21_001, newReceivedBytes: 0n })
    expect(observer.snapshot(21_001).receivedBytesPerSecond).toBeNull()
  })

  it('ignores tiny or invalid local observations and conservatively rejects unstable copy costs', () => {
    const costs = new RecoveryCostObserver()
    costs.observeCopy({ bytes: 1n, durationMilliseconds: 1 })
    costs.observeCopy({ bytes: MIB, durationMilliseconds: 0 })
    costs.observeFlush({ bytes: 0n, durationMilliseconds: 10 })
    expect(costs.snapshot(0)).toEqual({
      receivedBytesPerSecond: null, copiedBytesPerSecond: null, flushMilliseconds: null,
    })
    costs.observeCopy({ bytes: 10n * MIB, durationMilliseconds: 1000 })
    costs.observeCopy({ bytes: 12n * MIB, durationMilliseconds: 1000 })
    costs.observeFlush({ bytes: MIB, durationMilliseconds: 10 })
    costs.observeFlush({ bytes: MIB, durationMilliseconds: 20 })
    expect(costs.snapshot(0)).toMatchObject({ copiedBytesPerSecond: 10n * MIB, flushMilliseconds: 20 })
    costs.observeCopy({ bytes: 100n * MIB, durationMilliseconds: 1000 })
    expect(costs.snapshot(0).copiedBytesPerSecond).toBeNull()
  })
})
