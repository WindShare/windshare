import { describe, expect, it } from 'vitest'
import { PeerTabAdmission, type PeerTabAdmissionClock, type PeerTabAdmissionPermit } from '../../src/connectivity/peer-set/tab-admission'

class Clock implements PeerTabAdmissionClock {
  time = 0
  readonly waits = new Set<{ at: number; signal: AbortSignal; resolve(): void }>()
  now(): number { return this.time }
  sleep(ms: number, signal: AbortSignal): Promise<void> {
    return new Promise((resolve, reject) => {
      const wait = { at: this.time + ms, signal, resolve: () => { signal.removeEventListener('abort', abort); resolve() } }
      const abort = () => { this.waits.delete(wait); reject(signal.reason) }
      signal.addEventListener('abort', abort, { once: true })
      this.waits.add(wait)
    })
  }
  async advance(ms: number): Promise<void> {
    this.time += ms
    for (const wait of [...this.waits]) if (wait.at <= this.time) {
      this.waits.delete(wait)
      wait.resolve()
    }
    for (let index = 0; index < 8; index += 1) await Promise.resolve()
  }
}
const signal = () => new AbortController().signal

describe('tab-wide fair peer admission', () => {
  it('admits an available zero-wait opportunity despite clock sampling overhead', async () => {
    let now = 0
    const clock = { now: () => ++now, sleep: () => new Promise<void>(() => undefined) }
    const admission = new PeerTabAdmission({}, clock)
    const permit = await admission.acquire({}, 1, 0, signal())
    expect(permit.release).toBeTypeOf('function')
    permit.release()
  })

  it('rotates independent receivers before another path from the last receiver', async () => {
    const clock = new Clock()
    const admission = new PeerTabAdmission({ concurrentAttempts: 1 }, clock)
    const first = {}
    const second = {}
    const active = await admission.acquire(first, 2, 1_000, signal())
    const granted: string[] = []
    const nextFirst = admission.acquire(first, 2, 1_000, signal()).then((permit) => { granted.push('first'); return permit })
    const nextSecond = admission.acquire(second, 2, 1_000, signal()).then((permit) => { granted.push('second'); return permit })
    active.release()
    const secondPermit = await nextSecond
    expect(granted).toEqual(['second'])
    secondPermit.release()
    const firstPermit = await nextFirst
    expect(granted).toEqual(['second', 'first'])
    firstPermit.release()
    expect(clock.waits.size).toBe(0)
  })

  it('bounds endpoint starts independently of attempt starts and replenishes without a generation reset', async () => {
    const clock = new Clock()
    const admission = new PeerTabAdmission({ startsPerInterval: 4, stunEndpointsPerInterval: 2, intervalMilliseconds: 100 }, clock)
    const oldGeneration = await admission.acquire({}, 2, 1_000, signal())
    oldGeneration.release()
    let renewed: PeerTabAdmissionPermit | undefined
    const replacement = admission.acquire({}, 2, 1_000, signal()).then((permit) => { renewed = permit })
    await clock.advance(99)
    expect(renewed).toBeUndefined()
    await clock.advance(1)
    await replacement
    expect(renewed).toBeDefined()
    renewed!.release()
    expect(clock.waits.size).toBe(0)
  })

  it('cancels only its queued receiver and refuses a late start without spending network work', async () => {
    const clock = new Clock()
    const admission = new PeerTabAdmission({ concurrentAttempts: 1 }, clock)
    const active = await admission.acquire({}, 2, 1_000, signal())
    const cancelled = new AbortController()
    const waiting = admission.acquire({}, 2, 1_000, cancelled.signal)
    const cause = new Error('receiver closed')
    cancelled.abort(cause)
    await expect(waiting).rejects.toBe(cause)
    const expired = admission.acquire({}, 2, 10, signal())
    const rejection = expect(expired).rejects.toMatchObject({ name: 'TimeoutError' })
    await clock.advance(11)
    await rejection
    const survivor = admission.acquire({}, 0, 1_000, signal())
    active.release()
    const permit = await survivor
    permit.release()
    permit.release()
    expect(clock.waits.size).toBe(0)
  })

  it('caps repeated immediate attempt starts even when no STUN endpoints are selected', async () => {
    const clock = new Clock()
    const admission = new PeerTabAdmission({ startsPerInterval: 1, intervalMilliseconds: 100 }, clock)
    ;(await admission.acquire({}, 0, 1_000, signal())).release()
    let permit: PeerTabAdmissionPermit | undefined
    const waiting = admission.acquire({}, 0, 1_000, signal()).then((value) => { permit = value })
    await clock.advance(99)
    expect(permit).toBeUndefined()
    await clock.advance(1)
    await waiting
    permit!.release()
  })
})
