import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { BlockResponseQueue } from '../../src/content/scheduling/block-response-wait'

beforeEach(() => vi.useFakeTimers())
afterEach(() => vi.useRealTimers())

describe('content lane response waits', () => {
  it('allows productive queueing and assemblies longer than the inactivity window', async () => {
    const queue = new BlockResponseQueue()
    const earlier = queue.begin(new AbortController().signal)
    const waiting = queue.begin(new AbortController().signal)
    for (let index = 0; index < 4; index += 1) {
      await vi.advanceTimersByTimeAsync(10_000)
      earlier.progress()
      expect(waiting.signal.aborted).toBe(false)
    }
    earlier.close()
    waiting.progress()
    for (let index = 0; index < 4; index += 1) {
      await vi.advanceTimersByTimeAsync(10_000)
      waiting.progress()
      expect(waiting.signal.aborted).toBe(false)
    }
    waiting.close()
    waiting.progress()
    expect(vi.getTimerCount()).toBe(0)
  })

  it.each(['later request', 'another lane', 'active assembly'])('does not borrow progress from %s', async scenario => {
    const queue = new BlockResponseQueue()
    const otherQueue = scenario === 'another lane' ? new BlockResponseQueue() : queue
    const earlier = otherQueue.begin(new AbortController().signal)
    const later = queue.begin(new AbortController().signal)
    const waiting = scenario === 'later request' ? earlier : later
    const productive = scenario === 'later request' ? later : earlier
    if (scenario === 'active assembly') waiting.progress()
    await vi.advanceTimersByTimeAsync(10_000)
    productive.progress()
    await vi.advanceTimersByTimeAsync(5_000)
    expect(waiting.signal.reason).toMatchObject({
      name: 'V2BlockInactivityTimeoutError',
      phase: scenario === 'active assembly' ? 'receiving_fragments' : 'awaiting_first_fragment',
      waitedMilliseconds: 15_000,
      queueProgress: 0,
    })
    earlier.close()
    later.close()
  })

  it('expires a queued request when its finite predecessor set stops progressing', async () => {
    const queue = new BlockResponseQueue()
    const earlier = queue.begin(new AbortController().signal)
    const waiting = queue.begin(new AbortController().signal)
    await vi.advanceTimersByTimeAsync(10_000)
    earlier.progress()
    earlier.close()
    await vi.advanceTimersByTimeAsync(15_000)
    expect(waiting.signal.reason).toMatchObject({
      phase: 'awaiting_first_fragment', waitedMilliseconds: 25_000, queueProgress: 1,
    })
    earlier.progress()
    waiting.close()
  })

  it('excludes local validation while retaining inactivity and cancellation boundaries', async () => {
    const queue = new BlockResponseQueue()
    const waiting = queue.begin(new AbortController().signal)
    waiting.suspend()
    await vi.advanceTimersByTimeAsync(20_000)
    expect(waiting.signal.aborted).toBe(false)
    waiting.progress()
    waiting.resume()
    await vi.advanceTimersByTimeAsync(15_000)
    expect(waiting.signal.reason).toMatchObject({ phase: 'receiving_fragments' })
    waiting.close()
    waiting.resume()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('preserves immediate caller cancellation and never revives expired requests', async () => {
    const queue = new BlockResponseQueue()
    const controller = new AbortController()
    const waiting = queue.begin(controller.signal)
    const reason = new Error('user stopped receiving')
    controller.abort(reason)
    waiting.progress()
    await vi.advanceTimersByTimeAsync(15_000)
    expect(waiting.signal.reason).toBe(reason)
    waiting.close()
    waiting.close()
    const expired = queue.begin(new AbortController().signal)
    await vi.advanceTimersByTimeAsync(15_000)
    expired.progress()
    expect(expired.signal.reason).toMatchObject({ phase: 'awaiting_first_fragment' })
    expired.close()
    expect(vi.getTimerCount()).toBe(0)
  })
})
