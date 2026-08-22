export const DIRECT_ZIP_MAXIMUM_POSITIONED_FSA_OFFSET = BigInt(Number.MAX_SAFE_INTEGER)

export function directZipFsaOffset(value: bigint, label = 'direct ZIP FSA offset'): number {
  requireDirectZipFsaOffset(value, label)
  return Number(value)
}

export function directZipFsaOffsetBigInt(
  value: number,
  label = 'direct ZIP FSA offset',
): bigint {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new RangeError(`${label} exceeds the positioned File System Access API`)
  }
  return BigInt(value)
}

export function requireDirectZipFsaOffset(
  value: bigint,
  label = 'direct ZIP FSA offset',
): void {
  if (typeof value !== 'bigint' || value < 0n ||
      value > DIRECT_ZIP_MAXIMUM_POSITIONED_FSA_OFFSET) {
    throw new RangeError(`${label} exceeds the positioned File System Access API`)
  }
}
