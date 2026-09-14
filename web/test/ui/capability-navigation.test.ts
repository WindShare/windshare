import { describe, expect, it, vi } from 'vitest'
import { CapabilityNavigation, type CapabilityJoinOutcome } from '../../src/ui/capability/navigation'
import { deferred } from './v2-receiver-orchestration-fixture'

describe('capability navigation ownership', () => {
  it('starts the first join immediately and reuses both pending and completed joins', async () => {
    const navigation = new CapabilityNavigation(() => true)
    const fingerprint = deferred<string>()
    const joined = deferred<CapabilityJoinOutcome>()
    const join = vi.fn(() => joined.promise)
    const first = navigation.open(fingerprint.promise, join)
    expect(join).toHaveBeenCalledTimes(1)
    const duplicate = navigation.open(Promise.resolve('same'), join)
    fingerprint.resolve('same')
    expect(await duplicate).toBe('reused')
    expect(join).toHaveBeenCalledTimes(1)
    joined.resolve('joined')
    await first
    expect(await navigation.open(Promise.resolve('same'), join)).toBe('reused')
  })

  it('lets the latest input win when fingerprint completion arrives out of order', async () => {
    const navigation = new CapabilityNavigation(() => true)
    await navigation.open(Promise.resolve('first'), async () => 'joined')
    const slow = deferred<string>()
    const staleJoin = vi.fn(async (): Promise<CapabilityJoinOutcome> => 'joined')
    const stale = navigation.open(slow.promise, staleJoin)
    const latestJoin = vi.fn(async (): Promise<CapabilityJoinOutcome> => 'joined')
    expect(await navigation.open(Promise.resolve('latest'), latestJoin)).toBe('opened')
    slow.resolve('stale')
    expect(await stale).toBe('superseded')
    expect(staleJoin).not.toHaveBeenCalled()
    expect(latestJoin).toHaveBeenCalledTimes(1)
  })

  it('preserves the current identity after a blocked replacement and retries failed joins', async () => {
    const navigation = new CapabilityNavigation(() => true)
    await navigation.open(Promise.resolve('current'), async () => 'joined')
    expect(await navigation.open(Promise.resolve('other'), async () => 'blocked')).toBe('blocked')
    const join = vi.fn(async (): Promise<CapabilityJoinOutcome> => 'joined')
    expect(await navigation.open(Promise.resolve('current'), join)).toBe('reused')
    await navigation.open(Promise.resolve('other'), async () => 'failed')
    expect(await navigation.open(Promise.resolve('other'), join)).toBe('opened')
    expect(join).toHaveBeenCalledTimes(1)
  })

  it('does not let a late failed join clear the newer authenticated identity', async () => {
    const navigation = new CapabilityNavigation(() => true)
    const old = deferred<CapabilityJoinOutcome>()
    const first = navigation.open(Promise.resolve('first'), () => old.promise)
    await navigation.open(Promise.resolve('new'), async () => 'joined')
    old.resolve('failed')
    await first
    expect(await navigation.open(Promise.resolve('new'), async () => 'failed')).toBe('reused')
  })

  it('invalidates comparisons on cancellation without resurrecting an old join', async () => {
    const navigation = new CapabilityNavigation(() => true)
    const identity = deferred<string>()
    const join = vi.fn(async (): Promise<CapabilityJoinOutcome> => 'joined')
    await navigation.open(identity.promise, join)
    const queued = navigation.open(Promise.resolve('same'), join)
    navigation.clear()
    identity.resolve('same')
    expect(await queued).toBe('superseded')
    expect(await navigation.open(Promise.resolve('same'), join)).toBe('opened')
    expect(join).toHaveBeenCalledTimes(2)
  })

  it('keeps malformed inputs retryable and releases identity after unexpected rejection', async () => {
    const navigation = new CapabilityNavigation(() => true)
    const join = vi.fn(async (): Promise<CapabilityJoinOutcome> => 'failed')
    await navigation.open(Promise.resolve(undefined), join)
    await navigation.open(Promise.resolve(undefined), join)
    expect(join).toHaveBeenCalledTimes(2)
    await expect(navigation.open(Promise.resolve('bad'), async () => { throw new Error('failed') }))
      .rejects.toThrow('failed')
    expect(await navigation.open(Promise.resolve('bad'), async () => 'joined')).toBe('opened')
  })
})
