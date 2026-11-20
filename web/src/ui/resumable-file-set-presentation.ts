import type { Language } from './preferences/model'
import { receiverCopy } from './locales/receiver'
import type { ReceiveLifecycleState } from '../transfer/lifecycle/state'
import type {
  RecoverySummary,
} from '../transfer/lifecycle/recovery-summary'
import type { MaterializationPlan } from '../transfer/intent'
import { formatBytes } from './progress-presentation'

type ResumableFileSetState = Extract<ReceiveLifecycleState, {
  kind: 'resumable-receive'
  payloadKind: 'file-set'
}>

export function resumableFileSetDescription(
  state: ResumableFileSetState,
  planKind: MaterializationPlan['kind'],
  recoverySummary?: RecoverySummary | null,
  language: Language = 'en',
): string {
  const t = receiverCopy[language].progress
  const completed = t.completed(state.completedFileCount, formatBytes(state.completedBytes))
  if (planKind !== 'direct-tree') return t.needSender(completed)
  if (recoverySummary === undefined || recoverySummary === null) {
    return t.validating(completed)
  }
  requireMatchingRecoverySummary(state, recoverySummary)
  return recoverySummaryDescription(recoverySummary, language)
}

export function recoverySummaryDescription(summary: RecoverySummary, language: Language = 'en'): string {
  const t = receiverCopy[language].progress
  const scope = summary.discovery === 'complete'
    ? t.discovered
    : t.discovering
  const suffix = summary.discovery === 'complete'
    ? ''
    : t.mayGrow
  const counts = t.recoveryCounts(t.fileCount(summary.completedFileCount), formatBytes(summary.completedBytes),
    t.fileCount(summary.verifiedPartialFileCount), formatBytes(summary.verifiedPartialBytes))
  return [scope, counts, t.preserve(formatBytes(summary.preservingRemainingBytes), formatBytes(summary.maximumPreservingTemporaryBytes)),
    t.restart(formatBytes(summary.restartRemainingBytes), formatBytes(summary.restartRedownloadBytes))].join(' ') + suffix
}

function requireMatchingRecoverySummary(
  state: ResumableFileSetState,
  summary: RecoverySummary,
): void {
  if (summary.lifecycleGeneration !== state.generation ||
      summary.checkpointSetDigest !== state.checkpointSetDigest ||
      summary.completedFileCount !== state.completedFileCount ||
      summary.completedBytes !== state.completedBytes) {
    throw new TypeError('recovery summary does not match the presented lifecycle')
  }
}
