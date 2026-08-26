import type { ReceiveLifecycleState } from '../output/workspace'
import type { RecoverySummary } from '../output/file-system-access/recovery-summary'
import type { MaterializationPlan } from '../transfer/intent'
import { formatBytes } from './v2-progress-presentation'

type ResumableFileSetState = Extract<ReceiveLifecycleState, {
  kind: 'resumable-receive'
  payloadKind: 'file-set'
}>

export function resumableFileSetDescription(
  state: ResumableFileSetState,
  planKind: MaterializationPlan['kind'],
  recoverySummary?: RecoverySummary | null,
): string {
  const completed = `${state.completedFileCount} file(s) and ${formatBytes(state.completedBytes)} are complete.`
  if (planKind !== 'direct-tree') return `${completed} Continuing still requires the sender and save permission.`
  if (recoverySummary === undefined || recoverySummary === null) {
    return `${completed} Recovery costs are being validated before either continuation action becomes available.`
  }
  requireMatchingRecoverySummary(state, recoverySummary)
  return recoverySummaryDescription(recoverySummary)
}

export function recoverySummaryDescription(summary: RecoverySummary): string {
  const scope = summary.discovery === 'complete'
    ? 'Selection complete.'
    : 'Known so far (discovery incomplete)'
  const suffix = summary.discovery === 'complete'
    ? ''
    : ' These totals can grow if more selected files are discovered.'
  return `${scope} Completed: ${fileCount(summary.completedFileCount)}, ${formatBytes(summary.completedBytes)}. ` +
    `Verified partial data: ${fileCount(summary.verifiedPartialFileCount)}, ${formatBytes(summary.verifiedPartialBytes)}. ` +
    `Preserve partial files: ${formatBytes(summary.preservingRemainingBytes)} remaining; up to ` +
    `${formatBytes(summary.maximumPreservingTemporaryBytes)} of temporary destination space. ` +
    `Restart incomplete files: ${formatBytes(summary.restartRemainingBytes)} remaining, including ` +
    `${formatBytes(summary.restartRedownloadBytes)} of verified data to redownload.${suffix}`
}

function fileCount(count: bigint): string {
  return `${count} ${count === 1n ? 'file' : 'files'}`
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
