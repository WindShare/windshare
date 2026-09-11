import { formatBytes } from '../v2-progress-presentation'
import type { TaskFacts, TaskProgressPresentation } from './model'
import { isTerminalLifecycleState } from '../../output/workspace/state'

const PERCENT_SCALE = 100n
const UNSETTLED_PERCENT_LIMIT = 99n

export function presentTaskProgress(facts: TaskFacts): TaskProgressPresentation | null {
  const progress = facts.progress
  const direct = facts.directZipProgress
  if (progress === null && direct === null) return retainedProgress(facts)
  // Direct ZIP receipt advances while output writes and durable checkpoints wait.
  // Its logical payload already includes the retained prefix after reopening.
  const materialized = direct?.receivedSelectedBytes ?? progress?.materializedBytes ?? 0n
  const files = completedFileLabel(facts)
  const details = progressDetails(facts)
  const percentage = exactPercentage(facts, materialized)
  const exact = progress?.discovery === 'complete'
  const total = exact ? ` / ${formatBytes(progress.discoveredBytes)}` : ''
  const ratio = percentage === null ? '' : ` · ${percentage}%`
  return Object.freeze({
    mode: percentage === null ? 'indeterminate' : 'determinate',
    percentage,
    sampleIdentity: progress?.transferJobId ?? '',
    receivedBytes: progress?.writtenBytes ?? 0n,
    remainingBytes: exact && facts.completeness !== 'partial'
      ? maximum(0n, progress.discoveredBytes - materialized) : null,
    status: discoveryStatus(facts),
    label: `${formatBytes(materialized)}${total} ${direct === null ? 'written or reused' : 'received'}${ratio}${files}`,
    details: Object.freeze(details),
  })
}

function completedFileLabel(facts: TaskFacts): string {
  if (facts.browserDelivery != null) return ` · ${facts.browserDelivery.targetSavedFiles} files saved to folder`
  return facts.progress === null ? '' : ` · ${facts.progress.completedFiles} files completed`
}

function retainedProgress(facts: TaskFacts): TaskProgressPresentation | null {
  const state = facts.lifecycle
  if (facts.browserDelivery != null) return Object.freeze({
    mode: 'indeterminate', percentage: null,
    sampleIdentity: '', receivedBytes: 0n, remainingBytes: null, status: null,
    label: `${formatBytes(facts.browserDelivery.targetSavedBytes)} saved to folder`,
    details: Object.freeze(browserDeliveryDetails(facts.browserDelivery, facts.lifecycle)),
  })
  if (state.kind !== 'resumable-receive') return null
  const retained = state.payloadKind === 'direct-zip' ? state.safeSelectedPayloadBytes : state.completedBytes
  return Object.freeze({
    mode: 'indeterminate', percentage: null,
    sampleIdentity: '', receivedBytes: 0n, remainingBytes: null, status: null,
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
  const limit = facts.completeness === 'complete' && facts.publication !== 'unpublished'
    ? PERCENT_SCALE : UNSETTLED_PERCENT_LIMIT
  if (raw < 0n) return 0
  return Number(raw > limit ? limit : raw)
}

function discoveryStatus(facts: TaskFacts): string | null {
  const progress = facts.progress
  if (progress?.discovery === 'open') return `Calculating total · ${progress.discoveredFiles} files found so far`
  return progress?.discovery === 'failed' ? 'Counting did not finish; final total unknown.' : null
}

function browserDeliveryDetails(summary: NonNullable<TaskFacts['browserDelivery']>, lifecycle: TaskFacts['lifecycle']): string[] {
  const terminal = isTerminalLifecycleState(lifecycle)
  return [
    ...(terminal ? [] : [`${formatBytes(summary.recoverableBytes)} verified and recoverable after restart.`]),
    `${formatBytes(summary.targetSavedBytes)} saved to the chosen folder.`,
    `${formatBytes(summary.stagedBytes)} retained in browser staging.`,
    ...((summary.stagedCompleteFiles + summary.copyingFiles) > 0
      ? [`${summary.stagedCompleteFiles + summary.copyingFiles} received files still need local saving.`] : []),
    ...(summary.cleanupPendingFiles > 0
      ? [`${summary.cleanupPendingFiles} files await staging cleanup.`] : []),
    ...(terminal && summary.incompleteStagedFiles > 0
      ? [`${summary.incompleteStagedFiles} incomplete staged files cannot continue and can be discarded.`] : []),
  ]
}

function maximum(left: bigint, right: bigint): bigint { return left > right ? left : right }

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
    details.push(`${formatBytes(direct.writtenSelectedBytes)} written or reused in the ZIP.`)
    details.push(`${formatBytes(direct.safeResumeBytes)} safe to resume after restart.`)
    if (direct.resumeTemporarySpaceUpperBound !== undefined) {
      details.push(`Resuming may need up to ${formatBytes(direct.resumeTemporarySpaceUpperBound)} of temporary destination space.`)
    }
  } else if (facts.browserDelivery != null) {
    details.push(...browserDeliveryDetails(facts.browserDelivery, facts.lifecycle))
  } else {
    details.push('Received bytes are not a measurement of progress recoverable after restart.')
  }
  return details
}
