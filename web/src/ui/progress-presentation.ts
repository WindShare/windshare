const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'] as const

export function formatBytes(bytes: bigint): string {
  let value = bytes
  let unit = 0
  let divisor = 1n
  while (value >= 1024n && unit < BYTE_UNITS.length - 1) {
    value /= 1024n
    divisor *= 1024n
    unit += 1
  }
  if (unit === 0) return `${bytes} B`
  const tenths = (bytes * 10n) / divisor
  return `${tenths / 10n}.${tenths % 10n} ${BYTE_UNITS[unit]}`
}
