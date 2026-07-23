import { describe, expect, it } from 'vitest'

import { BoundedStreamQueue } from '../../src/transport/relay/stream-queue'

describe('bounded relay stream queue', () => {
  it('releases buffered ownership when its only consumer cancels', async () => {
    const queue = new BoundedStreamQueue<Uint8Array>(2)
    expect(queue.push(Uint8Array.of(1))).toBe('accepted')
    expect(queue.push(Uint8Array.of(2))).toBe('accepted')
    expect(queue.bufferedCount).toBe(2)

    await queue.stream.getReader().cancel()

    expect(queue.bufferedCount).toBe(0)
  })

  it('reports overflow without accepting bytes beyond its fixed capacity', () => {
    const queue = new BoundedStreamQueue<Uint8Array>(1)
    expect(queue.push(Uint8Array.of(1))).toBe('accepted')
    expect(queue.push(Uint8Array.of(2))).toBe('overflow')
    expect(queue.bufferedCount).toBe(1)
  })
})
