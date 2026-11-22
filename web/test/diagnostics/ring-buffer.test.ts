import { describe, expect, it } from 'vitest'
import { DiagnosticRingBuffer } from '../../src/diagnostics/ring-buffer'

describe('DiagnosticRingBuffer', () => {
  it('retains items in FIFO order and overwrites oldest entries in O(1) when count or bytes exceed bounds', () => {
    const ring = new DiagnosticRingBuffer<string>({
      maxCount: 3,
      maxEventBytes: 10,
      maxTotalBytes: 16,
    })

    expect(ring.push('a', 6)).toBe(true)
    expect(ring.push('b', 6)).toBe(true)
    expect(ring.push('c', 6)).toBe(true) // exceeds 16 bytes -> evicts 'a'
    expect(ring.entries()).toEqual(['b', 'c'])
    expect(ring.size).toBe(2)
    expect(ring.bytes).toBe(12)
    expect(ring.overwritten).toBe(1)

    expect(ring.push('d', 4)).toBe(true) // count hits 3, bytes 16
    expect(ring.push('e', 4)).toBe(true) // count exceeds 3 -> evicts 'b'
    expect(ring.entries()).toEqual(['c', 'd', 'e'])
    expect(ring.last()).toBe('e')
    expect(ring.overwritten).toBe(2)

    expect(ring.push('too-big', 11)).toBe(false)
    expect(ring.dropped).toBe(1)
    expect(ring.entries()).toEqual(['c', 'd', 'e'])

    ring.clear()
    expect(ring.size).toBe(0)
    expect(ring.bytes).toBe(0)
    expect(ring.last()).toBeUndefined()
    expect(ring.entries()).toEqual([])
  })
})
