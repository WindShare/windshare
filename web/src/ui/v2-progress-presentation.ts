import type { V2ReceiverProgress } from './v2-model'
import type { ProjectedByteCount } from '../output/planning'
import type { ReceiveLifecycleState } from '../output/workspace'
import type { V2DirectZipProgressSnapshot } from './v2-receive-runtime'

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'] as const
const PERCENT_SCALE = 100n
const INCOMPLETE_PERCENT_LIMIT = 99n

export interface DirectZipProgressPresentation {
  readonly primary: string
  readonly safeResume: string
  readonly temporarySpace: string | null
  readonly percentage: bigint | null
}

export function capacityWaitDescription(progress: V2ReceiverProgress): string | null {
  return progress.capacityWaitVisible && progress.capacityWaitingFiles > 0
    ? 'Waiting for sender capacity'
    : null
}

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

export function presentDirectZipProgress(input: Readonly<{
  progress: V2DirectZipProgressSnapshot
  selectedBytes: ProjectedByteCount
  lifecycle: ReceiveLifecycleState | null
}>): DirectZipProgressPresentation {
  const published = input.lifecycle?.kind === 'published'
  const total = input.selectedBytes.bytes
  const percentage = input.selectedBytes.kind === 'exact'
    ? boundedPercentage(input.progress.receivedSelectedBytes, total, published)
    : null
  const totalCopy = input.selectedBytes.kind === 'exact'
    ? ` of ${formatBytes(total)}`
    : `; estimated selection is at least ${formatBytes(total)}`
  const phaseCopy = published ? 'saved and verified' : directZipPhaseCopy(input.progress.phase)
  const percentageCopy = percentage === null ? '' : ` (${percentage}%)`
  return Object.freeze({
    primary: `${formatBytes(input.progress.receivedSelectedBytes)} received${totalCopy}` +
      `${percentageCopy} · ${phaseCopy}`,
    safeResume: `If interrupted, resume from ${formatBytes(input.progress.safeResumeBytes)}.`,
    temporarySpace: input.progress.resumeTemporarySpaceUpperBound === undefined
      ? null
      : `Continuing may require up to ${formatBytes(input.progress.resumeTemporarySpaceUpperBound)} of additional temporary space.`,
    percentage,
  })
}

function directZipPhaseCopy(phase: V2DirectZipProgressSnapshot['phase']): string {
  switch (phase) {
    case 'receiving': return 'receiving selected content'
    case 'saving-resume-position': return 'saving a safe resume position'
    case 'closing': return 'closing the ZIP'
    case 'verifying': return 'confirming saved content'
  }
}

function boundedPercentage(received: bigint, total: bigint, published: boolean): bigint {
  if (published) return PERCENT_SCALE
  if (total === 0n) return 0n
  return minimum(INCOMPLETE_PERCENT_LIMIT, received * PERCENT_SCALE / total)
}

function minimum(left: bigint, right: bigint): bigint {
  return left < right ? left : right
}
