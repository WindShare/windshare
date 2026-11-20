import type { Language } from '../preferences/model'
import { receiverCopy } from '../locales/receiver'
import { formatBytes } from '../progress-presentation'
import type { TaskFacts, TaskProgressPresentation } from './model'
import { isTerminalLifecycleState } from '../../transfer/lifecycle/state'

const PERCENT_SCALE = 100n
const UNSETTLED_PERCENT_LIMIT = 99n

export function presentTaskProgress(facts: TaskFacts, language: Language = 'en'): TaskProgressPresentation | null {
  const t = receiverCopy[language].progress
  const progress = facts.progress
  const direct = facts.directZipProgress
  if (progress === null && direct === null) return retainedProgress(facts, language)
  // Direct ZIP receipt advances while output writes and durable checkpoints wait.
  // Its logical payload already includes the retained prefix after reopening.
  const materialized = materializedTaskBytes(facts)
  const files = completedFileLabel(facts, language)
  const details = progressDetails(facts, language)
  const percentage = exactPercentage(facts, materialized)
  const exact = progress?.discovery === 'complete'
  const total = exact ? ` / ${formatBytes(progress.discoveredBytes)}` : ''
  const ratio = percentage === null ? '' : ` · ${percentage}%`
  return Object.freeze({
    mode: percentage === null ? 'indeterminate' : 'determinate',
    percentage,
    sampleIdentity: progress?.transferJobId ?? '',
    receivedObjectBytes: progress?.receivedObjectBytes ?? 0n,
    writtenBytes: progress?.writtenBytes ?? 0n,
    remainingBytes: exact && facts.completeness !== 'partial'
      ? maximum(0n, progress.discoveredBytes - materialized) : null,
    status: discoveryStatus(facts, language),
    label: `${t.amount(formatBytes(materialized) + total, direct !== null)}${ratio}${files}`,
    details: Object.freeze(details),
  })
}

function materializedTaskBytes(facts: TaskFacts): bigint {
  const state = facts.lifecycle
  // A settled checkpoint supersedes the attempt's last observation, including
  // interruptions before a reopened file could report its initial coverage.
  if (state.kind === 'resumable-receive' && state.payloadKind === 'file-set') return state.retainedBytes
  return facts.directZipProgress?.receivedSelectedBytes ?? facts.progress?.materializedBytes ?? 0n
}

function completedFileLabel(facts: TaskFacts, language: Language): string {
  const t = receiverCopy[language].progress
  if (facts.browserDelivery != null) return ` · ${t.savedFiles(facts.browserDelivery.targetSavedFiles)}`
  return facts.progress === null ? '' : ` · ${t.completedFiles(facts.progress.completedFiles)}`
}

function retainedProgress(facts: TaskFacts, language: Language): TaskProgressPresentation | null {
  const t = receiverCopy[language].progress
  const state = facts.lifecycle
  if (facts.browserDelivery != null) return Object.freeze({
    mode: 'indeterminate', percentage: null,
    sampleIdentity: '', receivedObjectBytes: 0n, writtenBytes: 0n, remainingBytes: null, status: null,
    label: t.savedBytes(formatBytes(facts.browserDelivery.targetSavedBytes)),
    details: Object.freeze(browserDeliveryDetails(facts.browserDelivery, facts.lifecycle, language)),
  })
  if (state.kind !== 'resumable-receive') return null
  const retained = retainedReceiveBytes(state)
  return Object.freeze({
    mode: 'indeterminate', percentage: null,
    sampleIdentity: '', receivedObjectBytes: 0n, writtenBytes: 0n, remainingBytes: null, status: null,
    label: t.retainedBytes(formatBytes(retained)),
    details: Object.freeze(state.payloadKind === 'direct-zip'
      ? [t.continueSpace(formatBytes(state.committedArchiveLength))]
      : [t.completedRetained(state.completedFileCount)]),
  })
}

function retainedReceiveBytes(state: Extract<TaskFacts['lifecycle'], { kind: 'resumable-receive' }>): bigint {
  switch (state.payloadKind) {
    case 'file-set': return state.retainedBytes
    case 'direct-zip': return state.safeSelectedPayloadBytes
    case 'workspace-archive': return state.completedBytes
  }
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

function discoveryStatus(facts: TaskFacts, language: Language): string | null {
  const t = receiverCopy[language].progress
  const progress = facts.progress
  if (progress?.discovery === 'open') return t.calculating(progress.discoveredFiles)
  return progress?.discovery === 'failed' ? t.countFailed : null
}

function browserDeliveryDetails(summary: NonNullable<TaskFacts['browserDelivery']>, lifecycle: TaskFacts['lifecycle'], language: Language): string[] {
  const t = receiverCopy[language].progress
  const terminal = isTerminalLifecycleState(lifecycle)
  return [
    ...(terminal ? [] : [t.verified(formatBytes(summary.recoverableBytes))]),
    t.savedDestination(formatBytes(summary.targetSavedBytes)),
    t.staged(formatBytes(summary.stagedBytes)),
    ...((summary.stagedCompleteFiles + summary.copyingFiles) > 0
      ? [t.awaitingSave(summary.stagedCompleteFiles + summary.copyingFiles)] : []),
    ...(summary.cleanupPendingFiles > 0
      ? [t.awaitingCleanup(summary.cleanupPendingFiles)] : []),
    ...(terminal && summary.incompleteStagedFiles > 0
      ? [t.discardable(summary.incompleteStagedFiles)] : []),
  ]
}

function maximum(left: bigint, right: bigint): bigint { return left > right ? left : right }

function progressDetails(facts: TaskFacts, language: Language): string[] {
  const t = receiverCopy[language].progress
  const progress = facts.progress
  const direct = facts.directZipProgress
  const details: string[] = []
  if (progress !== null) {
    details.push(progress.discovery === 'complete'
      ? t.total(progress.discoveredFiles, formatBytes(progress.discoveredBytes))
      : t.atLeast(progress.discoveredFiles, formatBytes(progress.discoveredBytes)))
    details.push(t.completeBytes(formatBytes(progress.completedBytes)))
    details.push(t.newBytes(formatBytes(progress.writtenBytes)))
    if (progress.discovery === 'failed') details.push(t.failedDirectories(progress.failedDirectories))
  }
  if (direct !== null) {
    details.push(t.zipWritten(formatBytes(direct.writtenSelectedBytes)))
    details.push(t.resumeBytes(formatBytes(direct.safeResumeBytes)))
    if (direct.resumeTemporarySpaceUpperBound !== undefined) {
      details.push(t.resumeSpace(formatBytes(direct.resumeTemporarySpaceUpperBound)))
    }
  } else if (facts.browserDelivery != null) {
    details.push(...browserDeliveryDetails(facts.browserDelivery, facts.lifecycle, language))
  } else {
    details.push(t.receivedHint)
  }
  return details
}
