import { describe, expect, it } from 'vitest'

import {
  ALL_SELECTION,
  MAX_CHUNK_COUNT,
  chunkSetHas,
  createChunkCount,
  createChunkIndex,
  createChunkSet,
  createPathSelection,
  fullChunkSet,
  type CanonicalPath,
} from '../../src/contracts'

describe('compact chunk selection', () => {
  it('normalizes, merges, snapshots, and freezes half-open ranges', () => {
    const inputs = [
      { first: 9, end: 12 },
      { first: 1, end: 4 },
      { first: 4, end: 7 },
      { first: 2, end: 3 },
      { first: 8, end: 8 },
    ]

    const set = createChunkSet(inputs)

    expect(set.ranges).toEqual([
      { first: 1, end: 7 },
      { first: 9, end: 12 },
    ])
    expect(set.count).toBe(9)
    expect(Object.isFrozen(set)).toBe(true)
    expect(Object.isFrozen(set.ranges)).toBe(true)
    expect(Object.isFrozen(set.ranges[0])).toBe(true)

    inputs[0]!.first = 0
    expect(set.ranges[1]?.first).toBe(9)
    expect(() => (set.ranges as unknown as unknown[]).push({})).toThrow(TypeError)
  })

  it('keeps full selection compact at the exact protocol boundary', () => {
    const set = fullChunkSet(MAX_CHUNK_COUNT)

    expect(set.ranges).toEqual([{ first: 0, end: MAX_CHUNK_COUNT }])
    expect(set.count).toBe(MAX_CHUNK_COUNT)
    expect(chunkSetHas(set, createChunkIndex(0))).toBe(true)
    expect(chunkSetHas(set, createChunkIndex(MAX_CHUNK_COUNT - 1))).toBe(true)
  })

  it('distinguishes an empty path selection from all entries', () => {
    const firstPath = 'tree/a.txt' as CanonicalPath
    const source = [firstPath]
    const selected = createPathSelection(source)
    source.push('tree/b.txt' as CanonicalPath)

    expect(ALL_SELECTION).toEqual({ kind: 'all' })
    expect(createPathSelection([])).toEqual({ kind: 'paths', paths: [] })
    expect(selected).toEqual({ kind: 'paths', paths: [firstPath] })
    expect(Object.isFrozen(selected.paths)).toBe(true)
  })

  it('rejects invalid indices, counts, and ranges before allocation', () => {
    expect(createChunkIndex(MAX_CHUNK_COUNT - 1)).toBe(MAX_CHUNK_COUNT - 1)
    expect(createChunkCount(MAX_CHUNK_COUNT)).toBe(MAX_CHUNK_COUNT)

    for (const value of [-1, 0.5, Number.NaN, Number.MAX_SAFE_INTEGER]) {
      expect(() => createChunkIndex(value)).toThrow(RangeError)
      expect(() => createChunkCount(value)).toThrow(RangeError)
    }
    expect(() => createChunkIndex(MAX_CHUNK_COUNT)).toThrow(RangeError)
    expect(() => createChunkSet([{ first: 2, end: 1 }])).toThrow(RangeError)
    expect(() => createChunkSet([{ first: 0, end: MAX_CHUNK_COUNT + 1 }])).toThrow(
      RangeError,
    )
  })

  it('uses interval membership without expanding ranges', () => {
    const set = createChunkSet([
      { first: 2, end: 5 },
      { first: 8, end: 10 },
    ])

    expect(chunkSetHas(set, createChunkIndex(1))).toBe(false)
    expect(chunkSetHas(set, createChunkIndex(2))).toBe(true)
    expect(chunkSetHas(set, createChunkIndex(4))).toBe(true)
    expect(chunkSetHas(set, createChunkIndex(5))).toBe(false)
    expect(chunkSetHas(set, createChunkIndex(9))).toBe(true)
    expect(chunkSetHas(set, createChunkIndex(10))).toBe(false)
  })
})
