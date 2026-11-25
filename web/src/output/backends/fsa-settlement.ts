import { catalogCapacityReason, transferFailureReason } from '../../transfer/fault'
import type { SettledTransferJobResult } from '../../transfer/job'
import { transitionReceiveState, type ReceiveContentWarning } from '../../transfer/state'
import type { ReceiveTaskRecord, StoredTaskFileSummary } from '../store/types'

export interface FSALifecycleSettlementInput {
  readonly singleFile: boolean
  readonly stored: StoredTaskFileSummary
}

type TerminalIncompleteFSAStatus = Exclude<SettledTransferJobResult['status'], 'completed' | 'paused'>

export function nextFSALifecycle(
  current: ReceiveTaskRecord['lifecycle'],
  result: SettledTransferJobResult,
  input: FSALifecycleSettlementInput,
): ReceiveTaskRecord['lifecycle'] {
  const { singleFile, stored } = input
  const warning = result.contentWarning ?? null

  // A single-file FSA task commits its sole file to disk and IndexedDB before settlement;
  // a late pause, stop, or post-commit error must not downgrade a saved file back to incomplete.
  if (singleFile && stored.completedFiles > 0n) {
    return transitionReceiveState(current, {
      kind: 'saved',
      savedFiles: stored.completedFiles,
      savedBytes: stored.completedBytes,
      contentWarning: warning,
    })
  }

  if (result.status === 'completed') {
    return transitionReceiveState(current, {
      kind: 'saved',
      savedFiles: stored.completedFiles,
      savedBytes: stored.completedBytes,
      contentWarning: warning,
    })
  }

  if (result.status === 'paused') {
    return transitionReceiveState(current, {
      kind: 'paused',
      reason: result.pauseReason ?? 'user',
      completedFiles: stored.completedFiles,
      completedBytes: stored.completedBytes,
      retainedBytes: stored.retainedBytes,
      discoveredFiles: result.discoveredFiles > stored.fileCount ? result.discoveredFiles : stored.fileCount,
      discoveredBytes: result.discoveredBytes > stored.totalBytes ? result.discoveredBytes : stored.totalBytes,
      discoveryComplete: result.discoveryComplete,
      contentWarning: warning,
    })
  }

  const failedCount = result.failedFiles + result.failedDirectories
  if (stored.completedFiles > 0n) {
    // Terminal cleanup never deletes completed FSA files; promote any incomplete terminal
    // outcome to `partial` so saved counts, safe-name restoration, and replacement recovery survive.
    const reason = catalogCapacityReason(result.error) ?? (
      result.status === 'stopped' || (result.status === 'partial' && failedCount === 0n)
        ? 'stopped'
        : 'failures'
    )
    return transitionReceiveState(current, {
      kind: 'partial',
      reason,
      savedFiles: stored.completedFiles,
      failedFiles: failedCount,
      ...(stored.hasUnfinished ? { cleanupPending: true as const } : {}),
      contentWarning: warning,
    })
  }

  return failedFSALifecycle(current, result.status, result.error, stored, warning)
}

function failedFSALifecycle(
  current: ReceiveTaskRecord['lifecycle'],
  status: TerminalIncompleteFSAStatus,
  error: unknown,
  stored: StoredTaskFileSummary,
  warning: ReceiveContentWarning | null,
): ReceiveTaskRecord['lifecycle'] {
  const failedCleanupPending = stored.hasUnfinished || stored.fileCount === 0n
    ? { cleanupPending: true as const }
    : {}
  switch (status) {
    case 'stopped':
      return transitionReceiveState(current, { kind: 'failed', reason: 'cancelled', contentWarning: null, ...failedCleanupPending })
    case 'source-changed':
      return transitionReceiveState(current, { kind: 'failed', reason: 'source-changed', contentWarning: warning, ...failedCleanupPending })
    case 'source-busy':
      return transitionReceiveState(current, { kind: 'failed', reason: 'source-busy', contentWarning: warning, ...failedCleanupPending })
    case 'partial':
    case 'failed': {
      const reason = transferFailureReason(error)
      return transitionReceiveState(current, {
        kind: 'failed',
        reason,
        ...(reason === 'error' && error instanceof Error ? { detail: error.message } : {}),
        contentWarning: warning,
        ...failedCleanupPending,
      })
    }
  }
}
