import { formatBytes } from '../v2-progress-presentation'
import type { TaskFacts, TaskProgressPresentation } from './model'

const PERCENT_SCALE = 100n
const UNSETTLED_PERCENT_LIMIT = 99n

export function presentTaskProgress(facts: TaskFacts): TaskProgressPresentation | null {
  const progress = facts.progress
  const direct = facts.directZipProgress
  if (progress === null && direct === null) return retainedProgress(facts)
  const materialized = direct?.receivedSelectedBytes ?? progress?.materializedBytes ?? 0n
  const files = progress === null ? '' : ` · ${progress.completedFiles} files completed`
  const details = progressDetails(facts)
  const percentage = exactPercentage(facts, materialized)
  return Object.freeze({
    mode: percentage === null ? 'indeterminate' : 'determinate',
    percentage, label: `${formatBytes(materialized)} ${direct === null ? 'written or reused' : 'received'}${files}`,
    details: Object.freeze(details),
  })
}

function retainedProgress(facts: TaskFacts): TaskProgressPresentation | null {
  const state = facts.lifecycle
  if (state.kind !== 'resumable-receive') return null
  const retained = state.payloadKind === 'direct-zip' ? state.safeSelectedPayloadBytes : state.completedBytes
  return Object.freeze({
    mode: 'indeterminate', percentage: null,
    label: `${formatBytes(retained)} retained for continuation`,
    details: Object.freeze(state.payloadKind === 'direct-zip'
      ? [`Continuing may need up to ${formatBytes(state.committedArchiveLength)} of temporary destination space.`]
      : [`${state.completedFileCount} completed files retained.`]),
  })
}

function exactPercentage(facts: TaskFacts, materialized: bigint): number | null {
  const progress = facts.progress
  // Receipt progress never proves publication. A closed, exact discovery set is
  // required even when a single active file happens to have a known size.
  if (progress?.discovery !== 'complete' || progress.discoveredBytes <= 0n) return null
  const raw = materialized * PERCENT_SCALE / progress.discoveredBytes
  const limit = facts.completeness === 'complete' ? PERCENT_SCALE : UNSETTLED_PERCENT_LIMIT
  if (raw < 0n) return 0
  return Number(raw > limit ? limit : raw)
}

function progressDetails(facts: TaskFacts): string[] {
  const progress = facts.progress
  const direct = facts.directZipProgress
  const details: string[] = []
  if (progress !== null) {
    details.push(progress.discovery === 'complete'
      ? `${progress.discoveredFiles} files · ${formatBytes(progress.discoveredBytes)} total`
      : `At least ${progress.discoveredFiles} files · ${formatBytes(progress.discoveredBytes)} found; final total unknown.`)
    details.push(`${formatBytes(progress.completedBytes)} in completed files.`)
    details.push(`${formatBytes(progress.writtenBytes)} newly received during this attempt.`)
    if (progress.discovery === 'failed') details.push(`${progress.failedDirectories} folders could not be fully discovered.`)
  }
  if (direct !== null) {
    details.push(`${formatBytes(direct.safeResumeBytes)} safe to resume after restart.`)
    if (direct.resumeTemporarySpaceUpperBound !== undefined) {
      details.push(`Resuming may need up to ${formatBytes(direct.resumeTemporarySpaceUpperBound)} of temporary destination space.`)
    }
  } else {
    details.push('Received bytes are not a measurement of progress recoverable after restart.')
  }
  return details
}
