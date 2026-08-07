import type { V2ReceiverProgress } from './v2-model'

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'] as const
const PERCENT_SCALE = 100n

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

export function discoveryProgressDescription(progress: V2ReceiverProgress): string {
  if (progress.discovery === 'complete') {
    return `${progress.discoveredFiles} file(s), ${formatBytes(progress.discoveredBytes)} total`
  }
  const lowerBound = `At least ${progress.discoveredFiles} file(s), ${formatBytes(progress.discoveredBytes)} discovered`
  return progress.discovery === 'failed'
    ? `${lowerBound}; ${progress.failedDirectories} branch(es) failed`
    : `${lowerBound} so far; final total unknown`
}

export function completionProgressDescription(progress: V2ReceiverProgress): string {
  const received = `${formatBytes(progress.writtenBytes)} received`
  if (progress.discovery !== 'complete') {
    return `${received} · ${progress.completedFiles} file(s) completed ` +
      `(${formatBytes(progress.completedBytes)} committed; final total unknown)`
  }
  const percentage = progress.discoveredBytes === 0n
    ? PERCENT_SCALE
    : minimum(PERCENT_SCALE, progress.completedBytes * PERCENT_SCALE / progress.discoveredBytes)
  return `${received} · ${progress.completedFiles}/${progress.discoveredFiles} file(s) completed · ` +
    `${formatBytes(progress.completedBytes)}/${formatBytes(progress.discoveredBytes)} committed (${percentage}%)`
}

function minimum(left: bigint, right: bigint): bigint {
  return left < right ? left : right
}
