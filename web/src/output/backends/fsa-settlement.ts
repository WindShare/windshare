import { catalogCapacityReason, transferFailureReason } from '../../transfer/fault'
import type { SettledTransferJobResult } from '../../transfer/job'
import { transitionReceiveState, type ReceiveContentWarning, type ReceivePartialReason } from '../../transfer/state'
import type { ReceiveTaskRecord, StoredTaskFileSummary } from '../store/types'

export interface FSALifecycleSettlementInput {
  readonly singleFile: boolean
  readonly stored: StoredTaskFileSummary
}

type TerminalIncompleteFSAResult = Exclude<SettledTransferJobResult, { status: 'completed' | 'paused' }>

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
      contentWarning: warning,
    })
  }

  const failedCount = result.failedFiles + result.failedDirectories
  if (stored.completedFiles > 0n) {
    // Terminal cleanup never deletes completed FSA files; promote any incomplete terminal
    // outcome to `partial` so saved counts, safe-name restoration, and replacement recovery survive.
    return transitionReceiveState(current, {
      kind: 'partial',
      reason: resolvePartialReason(result),
      savedFiles: stored.completedFiles,
      failedFiles: failedCount,
      ...(stored.hasUnfinished ? { cleanupPending: true as const } : {}),
      contentWarning: warning,
    })
  }

  return failedFSALifecycle(current, result, stored, warning)
}

function resolvePartialReason(result: TerminalIncompleteFSAResult): ReceivePartialReason {
  if (result.status === 'partial') return result.partialReason
  if (result.status === 'stopped') return 'stopped'
  return catalogCapacityReason(result.error) ?? 'failures'
}

function failedFSALifecycle(
  current: ReceiveTaskRecord['lifecycle'],
  result: TerminalIncompleteFSAResult,
  stored: StoredTaskFileSummary,
  warning: ReceiveContentWarning | null,
): ReceiveTaskRecord['lifecycle'] {
  const failedCleanupPending = stored.hasUnfinished || stored.fileCount === 0n
    ? { cleanupPending: true as const }
    : {}
  if (result.status === 'stopped' || (result.status === 'partial' && result.partialReason === 'stopped')) {
    return transitionReceiveState(current, { kind: 'failed', reason: 'cancelled', contentWarning: null, ...failedCleanupPending })
  }
  switch (result.status) {
    case 'source-changed':
      return transitionReceiveState(current, { kind: 'failed', reason: 'source-changed', contentWarning: warning, ...failedCleanupPending })
    case 'source-busy':
      return transitionReceiveState(current, { kind: 'failed', reason: 'source-busy', contentWarning: warning, ...failedCleanupPending })
    case 'partial':
    case 'failed': {
      const reason = transferFailureReason(result.error)
      return transitionReceiveState(current, {
        kind: 'failed',
        reason,
        ...(reason === 'error' && result.error instanceof Error ? { detail: result.error.message } : {}),
        contentWarning: warning,
        ...failedCleanupPending,
      })
    }
  }
}
