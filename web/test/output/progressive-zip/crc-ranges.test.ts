import { describe, expect, it } from 'vitest'
import { combineZipCrc32, completeZipEntryCrc, insertZipCrcRange, zipBytesCrc32, zipRangeDisposition } from '../../../src/output/progressive-zip/crc-ranges'

describe('progressive ZIP CRC summaries', () => {
  it('combines out-of-order uneven authenticated ranges and skips immutable retries', () => {
    const bytes = new TextEncoder().encode('123456789')
    let ranges = insertZipCrcRange([], { start: 5n, end: 9n, crc32: zipBytesCrc32(bytes.subarray(5)) })
    ranges = insertZipCrcRange(ranges, { start: 0n, end: 2n, crc32: zipBytesCrc32(bytes.subarray(0, 2)) })
    expect(completeZipEntryCrc(ranges, 9n)).toBeUndefined()
    ranges = insertZipCrcRange(ranges, { start: 2n, end: 5n, crc32: zipBytesCrc32(bytes.subarray(2, 5)) })
    expect(ranges).toEqual([{ start: 0n, end: 9n, crc32: 0xcbf43926 }])
    expect(zipRangeDisposition(ranges, 2n, 5n)).toBe('covered')
    expect(insertZipCrcRange(ranges, { start: 2n, end: 5n, crc32: 0 })).toBe(ranges)
    expect(completeZipEntryCrc([], 0n)).toBe(0)
  })

  it('rejects partially overlapping coverage and invalid CRC lengths', () => {
    const ranges = [{ start: 2n, end: 4n, crc32: 0 }]
    expect(() => zipRangeDisposition(ranges, 1n, 3n)).toThrow('overlaps')
    expect(() => combineZipCrc32(0, 0, -1n)).toThrow('nonnegative')
    expect(() => combineZipCrc32(-1, 0, 1n)).toThrow('uint32')
    expect(() => zipRangeDisposition([], -1n, 3n)).toThrow('invalid')
  })

  it('preserves concatenation associativity across the 32-bit length boundary', () => {
    let length = 1n
    let crc = zipBytesCrc32(Uint8Array.of(7))
    while (length < (1n << 32n)) {
      crc = combineZipCrc32(crc, crc, length)
      length *= 2n
    }
    const tail = zipBytesCrc32(Uint8Array.of(3, 4))
    const joined = combineZipCrc32(crc, crc, length)
    expect(combineZipCrc32(joined, tail, 2n)).toBe(
      combineZipCrc32(crc, combineZipCrc32(crc, tail, 2n), length + 2n),
    )
    expect(combineZipCrc32(crc, 0, 0n)).toBe(crc)
  })
})
