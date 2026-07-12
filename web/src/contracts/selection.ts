import type { ByteLength, CanonicalPath, ManifestEntry } from './manifest'

declare const chunkBoundaryBrand: unique symbol
declare const chunkIndexBrand: unique symbol
declare const chunkCountBrand: unique symbol
declare const chunkSetBrand: unique symbol
declare const planIdBrand: unique symbol

export const MAX_CHUNK_COUNT = 2 ** 26
export const PLAN_ID_BYTES = 32
export const PLAN_ID_DOMAIN = 'windshare/v1 transfer-plan\0' as const

/** A half-open interval boundary in [0, MAX_CHUNK_COUNT]. */
export type ChunkBoundary = number & {
  readonly [chunkBoundaryBrand]: 'ChunkBoundary'
}

/** A valid global chunk index in [0, MAX_CHUNK_COUNT). */
export type ChunkIndex = number & { readonly [chunkIndexBrand]: 'ChunkIndex' }

/** A checked chunk count in [0, MAX_CHUNK_COUNT]. */
export type ChunkCount = number & { readonly [chunkCountBrand]: 'ChunkCount' }

export interface ChunkRange {
  readonly first: ChunkIndex
  readonly end: ChunkBoundary
}

export interface ChunkRangeInput {
  readonly first: number
  readonly end: number
}

/**
 * Ranges are sorted, disjoint, non-adjacent, non-empty, and half-open. The
 * representation is compact even when it denotes every protocol-valid chunk.
 */
export interface ChunkSet {
  readonly [chunkSetBrand]: true
  readonly ranges: readonly ChunkRange[]
  readonly count: ChunkCount
}

export interface AllSelection {
  readonly kind: 'all'
}

export interface PathSelection {
  readonly kind: 'paths'
  /** An empty list deliberately means select nothing, not select everything. */
  readonly paths: readonly CanonicalPath[]
}

export type Selection = AllSelection | PathSelection

/**
 * A SHA-256 transfer-plan identity over PLAN_ID_DOMAIN and UTF-8 byte-sorted
 * canonical paths. Producers must snapshot before branding.
 */
export type PlanId = Uint8Array & { readonly [planIdBrand]: 'PlanId' }

/** The immutable projection shared by session scheduling and download sinks. */
export interface TransferPlan {
  readonly planId: PlanId
  readonly selectedEntries: readonly ManifestEntry[]
  readonly selectedBytes: ByteLength
  readonly chunks: ChunkSet
}

export const ALL_SELECTION: AllSelection = Object.freeze({ kind: 'all' })

const EMPTY_CHUNK_SET = Object.freeze({
  ranges: Object.freeze([]),
  count: 0 as ChunkCount,
}) as unknown as ChunkSet

function checkedChunkBoundary(value: number, label: string): ChunkBoundary {
  if (!Number.isSafeInteger(value) || value < 0 || value > MAX_CHUNK_COUNT) {
    throw new RangeError(`${label} must be an integer in [0, ${MAX_CHUNK_COUNT}]`)
  }
  return value as ChunkBoundary
}

export function createChunkIndex(value: number): ChunkIndex {
  checkedChunkBoundary(value, 'chunk index')
  if (value === MAX_CHUNK_COUNT) {
    throw new RangeError(`chunk index must be less than ${MAX_CHUNK_COUNT}`)
  }
  return value as ChunkIndex
}

export function createChunkCount(value: number): ChunkCount {
  checkedChunkBoundary(value, 'chunk count')
  return value as ChunkCount
}

/**
 * Snapshots and normalizes intervals so callers cannot mutate scheduler demand
 * after a transfer has started. Empty intervals are ignored; reversed intervals
 * are rejected because silently repairing them would conceal geometry defects.
 */
export function createChunkSet(inputs: readonly ChunkRangeInput[]): ChunkSet {
  const ranges = inputs.map(({ first, end }, index) => {
    const checkedFirst = checkedChunkBoundary(first, `range ${index} first`)
    const checkedEnd = checkedChunkBoundary(end, `range ${index} end`)
    if (checkedEnd < checkedFirst) {
      throw new RangeError(`range ${index} must satisfy first <= end`)
    }
    return { first: checkedFirst, end: checkedEnd }
  })

  ranges.sort((left, right) => left.first - right.first || left.end - right.end)

  const merged: Array<{ first: ChunkIndex; end: ChunkBoundary }> = []
  for (const range of ranges) {
    if (range.first === range.end) {
      continue
    }
    const previous = merged.at(-1)
    if (previous === undefined || range.first > previous.end) {
      merged.push({ first: createChunkIndex(range.first), end: range.end })
      continue
    }
    if (range.end > previous.end) {
      previous.end = range.end
    }
  }

  if (merged.length === 0) {
    return EMPTY_CHUNK_SET
  }

  const immutableRanges = Object.freeze(
    merged.map((range) => Object.freeze({ ...range })),
  )
  const count = immutableRanges.reduce(
    (total, range) => total + range.end - range.first,
    0,
  )

  return Object.freeze({
    ranges: immutableRanges,
    count: count as ChunkCount,
  }) as unknown as ChunkSet
}

export function fullChunkSet(chunkCount: number): ChunkSet {
  const count = createChunkCount(chunkCount)
  if (count === 0) {
    return EMPTY_CHUNK_SET
  }
  return createChunkSet([{ first: 0, end: count }])
}

export function chunkSetHas(set: ChunkSet, index: ChunkIndex): boolean {
  let low = 0
  let high = set.ranges.length
  while (low < high) {
    const middle = low + Math.floor((high - low) / 2)
    const range = set.ranges[middle]
    if (range === undefined) {
      return false
    }
    if (index < range.first) {
      high = middle
    } else if (index >= range.end) {
      low = middle + 1
    } else {
      return true
    }
  }
  return false
}

/** Snapshots paths while intentionally preserving order and duplicates for C1. */
export function createPathSelection(paths: readonly CanonicalPath[]): PathSelection {
  return Object.freeze({
    kind: 'paths',
    paths: Object.freeze([...paths]),
  })
}
