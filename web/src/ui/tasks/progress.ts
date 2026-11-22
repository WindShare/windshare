import type { Language } from '../preferences/model'
import { receiverCopy } from '../locales/receiver'
import { formatBytes } from '../progress-presentation'
import type { TaskFacts, TaskProgressPresentation } from './model'

const PERCENT_SCALE = 100n
const UNSETTLED_PERCENT_LIMIT = 99n

export function presentTaskProgress(facts: TaskFacts, language: Language = 'en'): TaskProgressPresentation | null {
  const t = receiverCopy[language].progress
  const progress = facts.progress
  if (progress === null) return retainedProgress(facts, language)
  const materialized = materializedTaskBytes(facts)
  const files = completedFileLabel(facts, language)
  const details = progressDetails(facts, language)
  const percentage = exactPercentage(facts, materialized)
  const exact = progress.discovery === 'complete'
  const total = exact ? ` / ${formatBytes(progress.discoveredBytes)}` : ''
  const ratio = percentage === null ? '' : ` · ${percentage}%`
  return Object.freeze({
    mode: percentage === null ? 'indeterminate' : 'determinate',
    percentage,
    sampleIdentity: progress.transferJobId,
    receivedObjectBytes: progress.receivedObjectBytes,
    writtenBytes: progress.writtenBytes,
    remainingBytes: exact && facts.completeness !== 'partial'
      ? maximum(0n, progress.discoveredBytes - materialized) : null,
    status: discoveryStatus(facts, language),
    label: `${t.amount(formatBytes(materialized) + total, false)}${ratio}${files}`,
    details: Object.freeze(details),
  })
}

function materializedTaskBytes(facts: TaskFacts): bigint {
  const state = facts.lifecycle
  // A settled checkpoint supersedes the attempt's last observation, including
  // interruptions before a reopened file could report its initial coverage.
  if (state.kind === 'resumable-receive' && state.payloadKind === 'file-set') return state.retainedBytes
  return facts.progress?.materializedBytes ?? 0n
}

function completedFileLabel(facts: TaskFacts, language: Language): string {
  const t = receiverCopy[language].progress
  return facts.progress === null ? '' : ` · ${t.completedFiles(facts.progress.completedFiles)}`
}

function retainedProgress(facts: TaskFacts, language: Language): TaskProgressPresentation | null {
  const t = receiverCopy[language].progress
  const state = facts.lifecycle
  if (state.kind !== 'resumable-receive') return null
  const retained = retainedReceiveBytes(state)
  return Object.freeze({
    mode: 'indeterminate', percentage: null,
    sampleIdentity: '', receivedObjectBytes: 0n, writtenBytes: 0n, remainingBytes: null, status: null,
    label: t.retainedBytes(formatBytes(retained)),
    details: Object.freeze([t.completedRetained(state.completedFileCount)]),
  })
}

function retainedReceiveBytes(state: Extract<TaskFacts['lifecycle'], { kind: 'resumable-receive' }>): bigint {
  switch (state.payloadKind) {
    case 'file-set': return state.retainedBytes
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

function maximum(left: bigint, right: bigint): bigint { return left > right ? left : right }

function progressDetails(facts: TaskFacts, language: Language): string[] {
  const t = receiverCopy[language].progress
  const progress = facts.progress
  const details: string[] = []
  if (progress !== null) {
    details.push(progress.discovery === 'complete'
      ? t.total(progress.discoveredFiles, formatBytes(progress.discoveredBytes))
      : t.atLeast(progress.discoveredFiles, formatBytes(progress.discoveredBytes)))
    details.push(t.completeBytes(formatBytes(progress.completedBytes)))
    details.push(t.newBytes(formatBytes(progress.writtenBytes)))
    if (progress.discovery === 'failed') details.push(t.failedDirectories(progress.failedDirectories))
  }
  details.push(t.receivedHint)
  return details
}
