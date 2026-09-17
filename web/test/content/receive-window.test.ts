import { describe, expect, it } from 'vitest'
import { BlockReceiveWindow, type BlockReceivePermit } from '../../src/content/scheduling/receive-window'

const MIB = 1024 * 1024
const FRAGMENT = 64 * 1024

function fixture(bytes = MIB) {
  let now = 0
  const window = new BlockReceiveWindow(() => now)
  const permits: BlockReceivePermit[] = []
  const controllers = Array.from({ length: 8 }, () => new AbortController())
  const requests = controllers.map(controller => window.acquire(bytes, controller.signal).then(permit => {
    permits.push(permit)
    return permit
  }))
  return { window, permits, requests, controllers, advance: (milliseconds: number) => { now += milliseconds } }
}

describe('physical lane receive admission', () => {
  it('finishes a slow default block before distributing the file across eight assemblies', async () => {
    const state = fixture()
    await Promise.resolve()
    expect(state.permits).toHaveLength(1)
    for (let fragment = 0; fragment < 14; fragment++) {
      state.advance(500)
      state.permits[0]!.receive(FRAGMENT)
      await Promise.resolve()
      expect(state.permits).toHaveLength(1)
    }
    state.advance(500)
    state.permits[0]!.receive(FRAGMENT)
    await Promise.resolve()
    expect(state.permits).toHaveLength(2)
    state.advance(500)
    state.permits[0]!.receive(FRAGMENT)
    state.permits[0]!.close()
    for (let index = 1; index < 8; index++) {
      const permit = await state.requests[index]!
      state.advance(8000)
      permit.receive(MIB)
      permit.close()
    }
    await Promise.all(state.requests)
  })

  it('fills a fast lane from its first fragment without waiting for a completed block', async () => {
    const state = fixture()
    await Promise.resolve()
    state.advance(1)
    state.permits[0]!.receive(FRAGMENT)
    await Promise.resolve()
    expect(state.permits).toHaveLength(8)
    for (const permit of await Promise.all(state.requests)) permit.close()
  })

  it('separates a distant peer response delay from its fast streaming capacity', async () => {
    const state = fixture()
    await Promise.resolve()
    state.advance(200)
    state.permits[0]!.receive(FRAGMENT)
    await Promise.resolve()
    expect(state.permits).toHaveLength(1)
    state.advance(1)
    state.permits[0]!.receive(FRAGMENT)
    await Promise.resolve()
    expect(state.permits).toHaveLength(8)
    expect(state.permits[1]!.admission.waitedMilliseconds).toBe(201)
    for (const permit of await Promise.all(state.requests)) permit.close()
  })

  it('pipelines small blocks on a distant peer instead of serializing every round trip', async () => {
    const state = fixture(FRAGMENT)
    await Promise.resolve()
    state.advance(200)
    state.permits[0]!.receive(FRAGMENT)
    await Promise.resolve()
    expect(state.permits.length).toBeGreaterThanOrEqual(3)
    while (state.permits.length < 8) {
      state.advance(200)
      for (const permit of [...state.permits]) permit.receive(FRAGMENT)
      await Promise.resolve()
    }
    for (const permit of await Promise.all(state.requests)) permit.close()
  })

  it('learns capacity across concurrent single-fragment blocks', async () => {
    const state = fixture(FRAGMENT / 2)
    await Promise.resolve()
    state.advance(200)
    state.permits[0]!.receive(FRAGMENT / 2)
    await Promise.resolve()
    expect(state.permits.length).toBeGreaterThan(2)
    state.advance(200)
    state.permits[1]!.receive(FRAGMENT / 2)
    state.advance(1)
    state.permits[2]!.receive(FRAGMENT / 2)
    await Promise.resolve()
    expect(state.permits).toHaveLength(8)
    for (const permit of await Promise.all(state.requests)) permit.close()
  })

  it('removes canceled queued work and releases an abandoned active read exactly once', async () => {
    const window = new BlockReceiveWindow(() => 0)
    const first = await window.acquire(MIB, new AbortController().signal)
    const controller = new AbortController()
    const canceled = window.acquire(MIB, controller.signal).catch(error => error)
    const next = window.acquire(MIB, new AbortController().signal)
    const reason = new Error('reader left')
    controller.abort(reason)
    expect(await canceled).toBe(reason)
    first.close()
    first.close()
    first.receive(MIB)
    const last = await next
    last.close()
    expect(() => window.acquire(0, new AbortController().signal)).toThrow(RangeError)
    expect(() => window.acquire(MIB, controller.signal)).toThrow(reason)
  })

  it('does not reuse stale fast capacity after an idle period', async () => {
    let now = 0
    const window = new BlockReceiveWindow(() => now)
    const first = await window.acquire(MIB, new AbortController().signal)
    now = 1
    first.receive(FRAGMENT)
    first.close()
    now = 5000
    const next = await window.acquire(MIB, new AbortController().signal)
    let admitted = false
    const queued = window.acquire(MIB, new AbortController().signal).then(permit => { admitted = true; return permit })
    await Promise.resolve()
    expect(admitted).toBe(false)
    next.close()
    const last = await queued
    last.close()
  })
})
