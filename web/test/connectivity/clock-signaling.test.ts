import { afterEach, describe, expect, it, vi } from 'vitest'

import { BrowserConnectivityClock } from '../../src/connectivity/clock'

afterEach(() => {
  vi.useRealTimers()
})

describe('connectivity clock', () => {
  it('resolves only at the injected deadline and removes its timer', async () => {
    vi.useFakeTimers()
    const clock = new BrowserConnectivityClock()
    let settled = false
    const sleeping = clock.sleep(10_000).then(() => {
      settled = true
    })

    await vi.advanceTimersByTimeAsync(9_999)
    expect(settled).toBe(false)
    await vi.advanceTimersByTimeAsync(1)
    await sleeping
    expect(vi.getTimerCount()).toBe(0)
  })

  it('rejects immediately or while waiting with the caller abort reason', async () => {
    vi.useFakeTimers()
    const clock = new BrowserConnectivityClock()
    const before = new AbortController()
    before.abort(new DOMException('before', 'AbortError'))
    await expect(clock.sleep(1, before.signal)).rejects.toMatchObject({ name: 'AbortError' })

    const during = new AbortController()
    const sleeping = clock.sleep(10_000, during.signal)
    during.abort(new DOMException('during', 'AbortError'))
    await expect(sleeping).rejects.toMatchObject({ name: 'AbortError' })
    expect(vi.getTimerCount()).toBe(0)
  })
})
