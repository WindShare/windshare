import type { Language } from '../preferences/model'
import { receiverCopy } from '../locales/receiver'
import { formatBytes } from '../progress-presentation'
import type { TaskFacts, TaskProgressPresentation } from './model'

const PERCENT_SCALE = 100n
const UNSETTLED_PERCENT_LIMIT = 99n

export function presentTaskProgress(facts: TaskFacts, language: Language = 'en'): TaskProgressPresentation | null {
  const t = receiverCopy[language].progress
  const progress = facts.progress
  const paused = facts.lifecycle.kind === 'paused'
  if (progress === null || (paused && progress.transferJobId === '')) return retainedProgress(facts, language)
  const materialized = paused ? facts.lifecycle.retainedBytes : progress.materializedBytes
  const completedFiles = paused ? Number(facts.lifecycle.completedFiles) : progress.completedFiles
  // A resumed run restarts progressive discovery from zero; suppress its partial
  // discovery total when re-pausing before it catches up to durable local progress.
  const coversPaused = !paused || progressCoversPausedState(progress, facts.lifecycle)
  const exact = progress.discovery === 'complete' && coversPaused
  const files = ` · ${t.completedFiles(completedFiles)}`
  const details = progressDetails(facts, coversPaused, language)
  const percentage = exact ? exactPercentage(facts, materialized) : null
  const total = exact ? ` / ${formatBytes(progress.discoveredBytes)}` : ''
  const amount = formatBytes(materialized) + total
  const coverage = paused ? t.retainedBytes(amount) : t.receivedBytes(amount)
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
    label: `${coverage}${ratio}${files}`,
    details: Object.freeze(details),
  })
}

function progressCoversPausedState(
  progress: NonNullable<TaskFacts['progress']>,
  state: Extract<TaskFacts['lifecycle'], { kind: 'paused' }>,
): boolean {
  return BigInt(progress.discoveredFiles) >= state.completedFiles &&
    progress.discoveredBytes >= state.completedBytes &&
    progress.discoveredBytes >= state.retainedBytes
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
  if (progress?.discovery === 'open') {
    return facts.lifecycle.kind === 'paused' ? null : t.calculating(progress.discoveredFiles)
  }
  return progress?.discovery === 'failed' ? t.countFailed : null
}

function maximum(left: bigint, right: bigint): bigint { return left > right ? left : right }

function progressDetails(facts: TaskFacts, coversPaused: boolean, language: Language): string[] {
  const t = receiverCopy[language].progress
  const progress = facts.progress
  const details: string[] = []
  if (progress !== null) {
    if (coversPaused) {
      details.push(progress.discovery === 'complete'
        ? t.total(progress.discoveredFiles, formatBytes(progress.discoveredBytes))
        : t.atLeast(progress.discoveredFiles, formatBytes(progress.discoveredBytes)))
    }
    const completedBytes = facts.lifecycle.kind === 'paused'
      ? facts.lifecycle.completedBytes
      : progress.completedBytes
    details.push(t.completeBytes(formatBytes(completedBytes)))
    details.push(t.newBytes(formatBytes(progress.writtenBytes)))
    if (progress.discovery === 'failed') details.push(t.failedDirectories(progress.failedDirectories))
  }
  if (facts.lifecycle.kind === 'downloading' && facts.backend !== 'portable') details.push(t.receivedHint)
  return details
}
