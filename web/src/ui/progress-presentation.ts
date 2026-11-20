import { receiverCopy } from './locales/receiver'
import type { Language } from './preferences/model'
import type { ProjectedByteCount } from '../output/model/artifacts/projections'
import type { ReceiveLifecycleState } from '../transfer/lifecycle/state'
import type { DirectZipProgressSnapshot } from '../application/receive/contracts'

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'] as const
const PERCENT_SCALE = 100n
const INCOMPLETE_PERCENT_LIMIT = 99n

export interface DirectZipProgressPresentation {
  readonly primary: string
  readonly written: string
  readonly safeResume: string
  readonly temporarySpace: string | null
  readonly percentage: bigint | null
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

export function presentDirectZipProgress(input: Readonly<{
  progress: DirectZipProgressSnapshot
  selectedBytes: ProjectedByteCount
  lifecycle: ReceiveLifecycleState | null
}>, language: Language = 'en'): DirectZipProgressPresentation {
  const t = receiverCopy[language].progress
  const published = input.lifecycle?.kind === 'published'
  const total = input.selectedBytes.bytes
  const percentage = input.selectedBytes.kind === 'exact'
    ? boundedPercentage(input.progress.receivedSelectedBytes, total, published)
    : null
  const received = formatBytes(input.progress.receivedSelectedBytes)
  const amount = input.selectedBytes.kind === 'exact'
    ? t.receivedExact(received, formatBytes(total))
    : t.receivedEstimate(received, formatBytes(total))
  const phase = published ? t.zipPublished : t.zipPhases[input.progress.phase]
  const percentageCopy = percentage === null ? '' : ` (${percentage}%)`
  return Object.freeze({
    primary: `${amount}${percentageCopy} · ${phase}`,
    written: t.zipWritten(formatBytes(input.progress.writtenSelectedBytes)),
    safeResume: t.resumeFrom(formatBytes(input.progress.safeResumeBytes)),
    temporarySpace: input.progress.resumeTemporarySpaceUpperBound === undefined
      ? null : t.extraSpace(formatBytes(input.progress.resumeTemporarySpaceUpperBound)),
    percentage,
  })
}

function boundedPercentage(received: bigint, total: bigint, published: boolean): bigint {
  if (published) return PERCENT_SCALE
  if (total === 0n) return 0n
  return minimum(INCOMPLETE_PERCENT_LIMIT, received * PERCENT_SCALE / total)
}

function minimum(left: bigint, right: bigint): bigint {
  return left < right ? left : right
}
