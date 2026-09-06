const MAX_PORTABLE_MODIFIED_SECONDS = 9_007_199_254_740_991n
const NANOSECONDS_PER_SECOND = 1_000_000_000
const NANOSECONDS_PER_MILLISECOND = 1_000_000

export interface CanonicalModifiedTime {
  readonly seconds: bigint
  readonly nanoseconds: number
  readonly precision: 1 | 2 | 3
}

export function snapshotCanonicalModifiedTime(
  input: CanonicalModifiedTime,
): CanonicalModifiedTime {
  if (typeof input.seconds !== 'bigint' ||
      input.seconds < -MAX_PORTABLE_MODIFIED_SECONDS ||
      input.seconds > MAX_PORTABLE_MODIFIED_SECONDS ||
      !Number.isInteger(input.nanoseconds) ||
      input.nanoseconds < 0 ||
      input.nanoseconds >= NANOSECONDS_PER_SECOND ||
      (input.precision !== 1 && input.precision !== 2 && input.precision !== 3) ||
      (input.precision === 1 && input.nanoseconds !== 0) ||
      (input.precision === 2 && input.nanoseconds % NANOSECONDS_PER_MILLISECOND !== 0)) {
    throw new TypeError('modified time violates the canonical portable representation')
  }
  return Object.freeze({
    seconds: input.seconds,
    nanoseconds: input.nanoseconds,
    precision: input.precision,
  })
}

export function sameModifiedTime(
  left: { readonly modifiedTime?: CanonicalModifiedTime },
  right: { readonly modifiedTime?: CanonicalModifiedTime },
): boolean {
  if (left.modifiedTime === undefined || right.modifiedTime === undefined) {
    return left.modifiedTime === right.modifiedTime
  }
  return left.modifiedTime.seconds === right.modifiedTime.seconds &&
    left.modifiedTime.nanoseconds === right.modifiedTime.nanoseconds &&
    left.modifiedTime.precision === right.modifiedTime.precision
}
