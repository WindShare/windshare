export interface ByteRange {
  readonly start: bigint
  readonly end: bigint
}

export interface LocalBlockRange {
  readonly first: bigint
  readonly end: bigint
}

export interface PlannedBlockSlice {
  readonly blockIndex: bigint
  readonly blockPlaintext: ByteRange
  readonly requestedBytes: ByteRange
  readonly offsetWithinBlock: bigint
}

export class ContentGeometryError extends RangeError {
  constructor(message: string) {
    super(message)
    this.name = 'ContentGeometryError'
  }
}

export function byteRange(start: bigint, end: bigint): ByteRange {
  if (start < 0n || end < start) {
    throw new ContentGeometryError('byte range must satisfy 0 <= start <= end')
  }
  return Object.freeze({ start, end })
}

/** Canonical sparse ranges for exactly one file identity. */
export class ByteRangeSet {
  readonly fileSize: bigint
  readonly ranges: readonly ByteRange[]

  constructor(fileSize: bigint, ranges: readonly ByteRange[]) {
    if (fileSize < 0n) {
      throw new ContentGeometryError('file size must not be negative')
    }
    this.fileSize = fileSize
    this.ranges = normalizeRanges(fileSize, ranges)
  }

  get empty(): boolean {
    return this.ranges.length === 0
  }

  covers(range: ByteRange): boolean {
    this.#requireWithinFile(range)
    if (range.start === range.end) {
      return true
    }
    const candidate = this.#firstEndingAfter(range.start)
    return candidate !== undefined &&
      candidate.start <= range.start &&
      candidate.end >= range.end
  }

  containsOffset(offset: bigint): boolean {
    if (offset < 0n || offset >= this.fileSize) {
      return false
    }
    const candidate = this.#firstEndingAfter(offset)
    return candidate !== undefined && candidate.start <= offset
  }

  missingFrom(wanted: ByteRangeSet): ByteRangeSet {
    if (wanted.fileSize !== this.fileSize) {
      throw new ContentGeometryError('range sets must belong to the same file size')
    }
    const missing: ByteRange[] = []
    let haveIndex = 0
    for (const target of wanted.ranges) {
      while (
        haveIndex < this.ranges.length &&
        (this.ranges[haveIndex]?.end ?? 0n) <= target.start
      ) {
        haveIndex += 1
      }
      const result = subtractAvailable(target, this.ranges, haveIndex)
      missing.push(...result.missing)
      haveIndex = result.nextAvailable
    }
    return new ByteRangeSet(this.fileSize, missing)
  }

  union(other: ByteRangeSet): ByteRangeSet {
    if (other.fileSize !== this.fileSize) {
      throw new ContentGeometryError('range sets must belong to the same file size')
    }
    return new ByteRangeSet(this.fileSize, [...this.ranges, ...other.ranges])
  }

  #firstEndingAfter(offset: bigint): ByteRange | undefined {
    let low = 0
    let high = this.ranges.length
    while (low < high) {
      const middle = low + Math.floor((high - low) / 2)
      const range = this.ranges[middle]
      if (range !== undefined && range.end <= offset) {
        low = middle + 1
      } else {
        high = middle
      }
    }
    return this.ranges[low]
  }

  #requireWithinFile(range: ByteRange): void {
    requireCanonicalRange(range)
    if (range.end > this.fileSize) {
      throw new ContentGeometryError('byte range exceeds its file')
    }
  }
}

/**
 * File-local geometry is compact even for very large files: planning returns a
 * half-open block span and computes individual slices on demand.
 */
export class FileGeometry {
  readonly exactSize: bigint
  readonly blockSize: bigint
  readonly blockCount: bigint

  constructor(exactSize: bigint, blockSize: bigint) {
    if (exactSize < 0n) {
      throw new ContentGeometryError('exact file size must not be negative')
    }
    if (blockSize <= 0n) {
      throw new ContentGeometryError('block size must be positive')
    }
    this.exactSize = exactSize
    this.blockSize = blockSize
    this.blockCount = exactSize === 0n
      ? 0n
      : 1n + (exactSize - 1n) / blockSize
  }

  wholeFile(): ByteRange {
    return byteRange(0n, this.exactSize)
  }

  requireRange(range: ByteRange): ByteRange {
    requireCanonicalRange(range)
    if (range.end > this.exactSize) {
      throw new ContentGeometryError('requested byte range exceeds its file')
    }
    return byteRange(range.start, range.end)
  }

  blockPlaintext(index: bigint): ByteRange {
    this.#requireBlockIndex(index)
    const start = index * this.blockSize
    return byteRange(start, minBigInt(start + this.blockSize, this.exactSize))
  }

  blocksCovering(range: ByteRange): LocalBlockRange {
    const checked = this.requireRange(range)
    if (checked.start === checked.end) {
      const boundary = checked.start === this.exactSize
        ? this.blockCount
        : checked.start / this.blockSize
      return Object.freeze({ first: boundary, end: boundary })
    }
    return Object.freeze({
      first: checked.start / this.blockSize,
      end: 1n + (checked.end - 1n) / this.blockSize,
    })
  }

  plan(range: ByteRange): FileRangePlan {
    return new FileRangePlan(this, this.requireRange(range))
  }

  #requireBlockIndex(index: bigint): void {
    if (index < 0n || index >= this.blockCount) {
      throw new ContentGeometryError('local block index is outside its file')
    }
  }
}

export class FileRangePlan {
  readonly requested: ByteRange
  readonly blocks: LocalBlockRange

  readonly #geometry: FileGeometry

  constructor(geometry: FileGeometry, requested: ByteRange) {
    this.#geometry = geometry
    this.requested = requested
    this.blocks = geometry.blocksCovering(requested)
  }

  sliceForBlock(index: bigint): PlannedBlockSlice | undefined {
    if (index < this.blocks.first || index >= this.blocks.end) {
      return undefined
    }
    const plaintext = this.#geometry.blockPlaintext(index)
    const start = maxBigInt(plaintext.start, this.requested.start)
    const end = minBigInt(plaintext.end, this.requested.end)
    return Object.freeze({
      blockIndex: index,
      blockPlaintext: plaintext,
      requestedBytes: byteRange(start, end),
      offsetWithinBlock: start - plaintext.start,
    })
  }
}

export function bigintToSafeNumber(value: bigint, label = 'value'): number {
  if (value < 0n || value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new ContentGeometryError(`${label} cannot be represented safely by this browser API`)
  }
  return Number(value)
}

function normalizeRanges(fileSize: bigint, inputs: readonly ByteRange[]): readonly ByteRange[] {
  const ranges = inputs.map((range) => {
    requireCanonicalRange(range)
    if (range.end > fileSize) {
      throw new ContentGeometryError('byte range exceeds its file')
    }
    return byteRange(range.start, range.end)
  }).filter((range) => range.start !== range.end)
  ranges.sort(compareByteRanges)

  const normalized: Array<{ start: bigint; end: bigint }> = []
  for (const range of ranges) {
    const previous = normalized.at(-1)
    if (previous === undefined || range.start > previous.end) {
      normalized.push({ start: range.start, end: range.end })
    } else if (range.end > previous.end) {
      previous.end = range.end
    }
  }
  return Object.freeze(normalized.map((range) => byteRange(range.start, range.end)))
}

function requireCanonicalRange(range: ByteRange): void {
  if (
    typeof range.start !== 'bigint' ||
    typeof range.end !== 'bigint' ||
    range.start < 0n ||
    range.end < range.start
  ) {
    throw new ContentGeometryError('byte range must satisfy 0 <= start <= end')
  }
}

function subtractAvailable(
  target: ByteRange,
  available: readonly ByteRange[],
  firstAvailable: number,
): { readonly missing: readonly ByteRange[]; readonly nextAvailable: number } {
  const missing: ByteRange[] = []
  let cursor = target.start
  let scan = firstAvailable
  while (scan < available.length) {
    const range = available[scan]
    if (range === undefined || range.start >= target.end) {
      break
    }
    if (range.start > cursor) {
      missing.push(byteRange(cursor, minBigInt(range.start, target.end)))
    }
    cursor = maxBigInt(cursor, range.end)
    if (cursor >= target.end) {
      break
    }
    scan += 1
  }
  if (cursor < target.end) {
    missing.push(byteRange(cursor, target.end))
  }
  return { missing, nextAvailable: scan }
}

function compareByteRanges(left: ByteRange, right: ByteRange): number {
  if (left.start !== right.start) {
    return left.start < right.start ? -1 : 1
  }
  if (left.end !== right.end) {
    return left.end < right.end ? -1 : 1
  }
  return 0
}

function minBigInt(left: bigint, right: bigint): bigint {
  return left < right ? left : right
}

function maxBigInt(left: bigint, right: bigint): bigint {
  return left > right ? left : right
}
