import { describe, expect, it } from 'vitest'

import {
  ByteRangeSet,
  ContentGeometryError,
  FileGeometry,
  bigintToSafeNumber,
  byteRange,
} from '../../src/content/geometry'

describe('file-local geometry and sparse range planning', () => {
  it('derives block count and exact tail ranges without a global stream', () => {
    const geometry = new FileGeometry(2_049n, 1_024n)
    expect(geometry.blockCount).toBe(3n)
    expect(geometry.blockPlaintext(0n)).toEqual({ start: 0n, end: 1_024n })
    expect(geometry.blockPlaintext(1n)).toEqual({ start: 1_024n, end: 2_048n })
    expect(geometry.blockPlaintext(2n)).toEqual({ start: 2_048n, end: 2_049n })
  })

  it('plans only intersecting local blocks and computes slices lazily', () => {
    const geometry = new FileGeometry(10_000n, 1_024n)
    const plan = geometry.plan(byteRange(1_000n, 2_050n))
    expect(plan.blocks).toEqual({ first: 0n, end: 3n })
    expect(plan.sliceForBlock(0n)).toMatchObject({
      requestedBytes: { start: 1_000n, end: 1_024n },
      offsetWithinBlock: 1_000n,
    })
    expect(plan.sliceForBlock(1n)).toMatchObject({
      requestedBytes: { start: 1_024n, end: 2_048n },
      offsetWithinBlock: 0n,
    })
    expect(plan.sliceForBlock(2n)).toMatchObject({
      requestedBytes: { start: 2_048n, end: 2_050n },
      offsetWithinBlock: 0n,
    })
    expect(plan.sliceForBlock(3n)).toBeUndefined()
  })

  it('keeps huge range plans compact', () => {
    const size = 1n << 80n
    const geometry = new FileGeometry(size, 1n << 20n)
    const plan = geometry.plan(byteRange(1n, size - 1n))
    expect(plan.blocks.first).toBe(0n)
    expect(plan.blocks.end).toBe(1n << 60n)
    expect(Object.keys(plan)).not.toContain('indices')
  })

  it('normalizes, unions, and subtracts sparse durable ranges', () => {
    const have = new ByteRangeSet(100n, [
      byteRange(20n, 30n),
      byteRange(0n, 10n),
      byteRange(10n, 20n),
      byteRange(80n, 90n),
    ])
    expect(have.ranges).toEqual([
      { start: 0n, end: 30n },
      { start: 80n, end: 90n },
    ])
    const wanted = new ByteRangeSet(100n, [byteRange(0n, 100n)])
    expect(have.missingFrom(wanted).ranges).toEqual([
      { start: 30n, end: 80n },
      { start: 90n, end: 100n },
    ])
    expect(have.union(new ByteRangeSet(100n, [byteRange(30n, 80n)])).ranges).toEqual([
      { start: 0n, end: 90n },
    ])
  })

  it('satisfies range-to-block coverage properties at hostile boundaries', () => {
    for (let size = 0n; size < 257n; size += 17n) {
      for (let blockSize = 1n; blockSize <= 32n; blockSize *= 2n) {
        const geometry = new FileGeometry(size, blockSize)
        for (let start = 0n; start <= size; start += 7n) {
          for (let end = start; end <= size; end += 11n) {
            const requested = byteRange(start, end)
            const blocks = geometry.blocksCovering(requested)
            if (start === end) {
              expect(blocks.first).toBe(blocks.end)
              continue
            }
            const first = geometry.blockPlaintext(blocks.first)
            const last = geometry.blockPlaintext(blocks.end - 1n)
            expect(first.start).toBeLessThanOrEqual(start)
            expect(last.end).toBeGreaterThanOrEqual(end)
            expect(blocks.first).toBe(start / blockSize)
          }
        }
      }
    }
  })

  it('rejects invalid ranges, block indices, and unsafe browser-number conversion', () => {
    const geometry = new FileGeometry(10n, 4n)
    expect(() => byteRange(-1n, 0n)).toThrow(ContentGeometryError)
    expect(() => byteRange(2n, 1n)).toThrow(ContentGeometryError)
    expect(() => geometry.requireRange(byteRange(0n, 11n))).toThrow(/exceeds/u)
    expect(() => geometry.blockPlaintext(-1n)).toThrow(/outside/u)
    expect(() => geometry.blockPlaintext(3n)).toThrow(/outside/u)
    expect(() => new FileGeometry(-1n, 1n)).toThrow()
    expect(() => new FileGeometry(1n, 0n)).toThrow()
    expect(() => new ByteRangeSet(10n, [byteRange(0n, 11n)])).toThrow(/exceeds/u)
    expect(bigintToSafeNumber(10n)).toBe(10)
    expect(bigintToSafeNumber(BigInt(Number.MAX_SAFE_INTEGER))).toBe(Number.MAX_SAFE_INTEGER)
    expect(() => bigintToSafeNumber(-1n)).toThrow(/safely/u)
    expect(() => bigintToSafeNumber(BigInt(Number.MAX_SAFE_INTEGER) + 1n)).toThrow(/safely/u)
  })
})
