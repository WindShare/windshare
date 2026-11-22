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
  const materialized = facts.lifecycle.kind === 'paused'
    ? facts.lifecycle.retainedBytes
    : progress.materializedBytes
  const files = ` · ${t.completedFiles(progress.completedFiles)}`
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
    label: `${t.amount(formatBytes(materialized) + total)}${ratio}${files}`,
    details: Object.freeze(details),
  })
}

function retainedProgress(facts: TaskFacts, language: Language): TaskProgressPresentation | null {
  const t = receiverCopy[language].progress
  const state = facts.lifecycle
  if (state.kind !== 'paused') return null
  return Object.freeze({
    mode: 'indeterminate',
    percentage: null,
    sampleIdentity: '',
    receivedObjectBytes: 0n,
    writtenBytes: 0n,
    remainingBytes: null,
    status: null,
    label: t.retainedBytes(formatBytes(state.retainedBytes)),
    details: Object.freeze([t.completedRetained(state.completedFiles)]),
  })
}

function exactPercentage(facts: TaskFacts, materialized: bigint): number | null {
  const progress = facts.progress
  if (progress?.discovery !== 'complete' || progress.discoveredBytes <= 0n) return null
  const raw = materialized * PERCENT_SCALE / progress.discoveredBytes
  const limit = facts.completeness === 'complete' && facts.publication !== 'unpublished'
    ? PERCENT_SCALE
    : UNSETTLED_PERCENT_LIMIT
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
