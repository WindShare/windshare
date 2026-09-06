import { ZipCrc32 } from '../zip-layout/policy'

export interface ZipCrcRange {
  readonly start: bigint
  readonly end: bigint
  readonly crc32: number
}

const CRC32_POLYNOMIAL = 0xedb88320
const CRC32_BITS = 32

/** Applies the zero-byte operator with bigint lengths, including ZIP64 ranges. */
export function combineZipCrc32(left: number, right: number, rightLength: bigint): number {
  requireCrc(left)
  requireCrc(right)
  if (rightLength < 0n) throw new RangeError('CRC length must be nonnegative')
  if (rightLength === 0n) return left >>> 0
  let odd = new Uint32Array(CRC32_BITS)
  odd[0] = CRC32_POLYNOMIAL
  let row = 1
  for (let index = 1; index < CRC32_BITS; index++) {
    odd[index] = row
    row = (row << 1) >>> 0
  }
  let even = square(odd)
  odd = square(even)
  let result = left >>> 0
  let length = rightLength
  do {
    even = square(odd)
    if ((length & 1n) !== 0n) result = times(even, result)
    length >>= 1n
    if (length === 0n) break
    odd = square(even)
    if ((length & 1n) !== 0n) result = times(odd, result)
    length >>= 1n
  } while (length !== 0n)
  return (result ^ right) >>> 0
}

export function zipBytesCrc32(bytes: Uint8Array): number {
  const crc = new ZipCrc32()
  crc.update(bytes)
  return crc.digest()
}

/** Already durable coverage is immutable; a retry may skip it but never replace it. */
export function zipRangeDisposition(
  ranges: readonly ZipCrcRange[],
  start: bigint,
  end: bigint,
): 'new' | 'covered' {
  if (start < 0n || end <= start) throw new RangeError('ZIP payload range is invalid')
  for (const range of ranges) {
    if (start >= range.start && end <= range.end) return 'covered'
    if (start < range.end && end > range.start) {
      throw new RangeError('ZIP payload write overlaps accepted coverage')
    }
  }
  return 'new'
}

export function insertZipCrcRange(
  ranges: readonly ZipCrcRange[],
  incoming: ZipCrcRange,
): readonly ZipCrcRange[] {
  requireCrc(incoming.crc32)
  if (zipRangeDisposition(ranges, incoming.start, incoming.end) === 'covered') return ranges
  const ordered = [...ranges, Object.freeze({ ...incoming })].sort(
    (left, right) => {
      if (left.start === right.start) return 0
      return left.start < right.start ? -1 : 1
    },
  )
  const result: ZipCrcRange[] = []
  for (const range of ordered) {
    const previous = result.at(-1)
    if (previous?.end === range.start) {
      result[result.length - 1] = Object.freeze({
        start: previous.start,
        end: range.end,
        crc32: combineZipCrc32(previous.crc32, range.crc32, range.end - range.start),
      })
    } else {
      result.push(range)
    }
  }
  return Object.freeze(result)
}

export function completeZipEntryCrc(
  ranges: readonly ZipCrcRange[],
  exactSize: bigint,
): number | undefined {
  if (exactSize === 0n) return ranges.length === 0 ? 0 : undefined
  const only = ranges[0]
  return ranges.length === 1 && only?.start === 0n && only.end === exactSize
    ? only.crc32 : undefined
}

function requireCrc(value: number): void {
  if (!Number.isInteger(value) || value < 0 || value > 0xffffffff) {
    throw new RangeError('CRC must be uint32')
  }
}

function times(matrix: Uint32Array, vector: number): number {
  let sum = 0
  let index = 0
  let remaining = vector >>> 0
  while (remaining !== 0) {
    if ((remaining & 1) !== 0) sum ^= matrix[index]!
    remaining >>>= 1
    index++
  }
  return sum >>> 0
}

function square(matrix: Uint32Array): Uint32Array<ArrayBuffer> {
  const result = new Uint32Array(CRC32_BITS)
  for (let index = 0; index < CRC32_BITS; index++) {
    result[index] = times(matrix, matrix[index]!)
  }
  return result
}
