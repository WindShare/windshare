import type { Language } from './preferences/model'
import { receiverCopy } from './locales/receiver'
import type { TransferWorkerSettlement } from '../transfer/outcome'
import type { ReceiveLifecycleState } from '../transfer/lifecycle/state'

export type TransferResultTone = 'success' | 'warning'

export interface TransferResultPresentation {
  readonly title: string
  readonly tone: TransferResultTone
  readonly lines: readonly string[]
}

export function presentTransferResult(
  worker: TransferWorkerSettlement,
  lifecycle?: ReceiveLifecycleState,
  language: Language = 'en',
): TransferResultPresentation {
  const t = receiverCopy[language].result
  if (worker.status === 'Succeeded') {
    return result(t.completed, 'success', [])
  }

  const counts = worker.fileOutcomes
  const lines: string[] = []
  appendCount(lines, counts.sourceDriftFiles, t.drift)
  appendCount(lines, counts.revisionConflictFiles, t.revision)
  appendCount(lines, counts.checkpointInvalidFiles, t.checkpoint)
  appendCount(lines, counts.ownedObjectUnknownFiles, t.ownership)
  appendCount(lines, counts.collisionFiles, t.exists)
  appendCount(lines, counts.failedFiles, t.failed)

  const directoryFailures = worker.failureCount - worker.fileFailureCount
  appendCount(lines, directoryFailures, t.directory)
  if (lines.length === 0 && worker.status === 'Paused') {
    lines.push(t.pausedHint)
  }

  return result(
    lifecycle?.kind === 'partial-directory' && lifecycle.reason === 'stopped'
      ? t.stopped
      : resultTitle(worker, language),
    'warning',
    lines,
  )
}

function result(
  title: string,
  tone: TransferResultTone,
  lines: readonly string[],
): TransferResultPresentation {
  return Object.freeze({
    title,
    tone,
    lines: Object.freeze([...lines]),
  })
}

function resultTitle(worker: TransferWorkerSettlement, language: Language): string {
  const t = receiverCopy[language].result
  const counts = worker.fileOutcomes
  if (counts.sourceDriftFiles > 0) return t.sourceChanged
  if (counts.revisionConflictFiles > 0) return t.revisionConflict
  if (counts.checkpointInvalidFiles > 0) return t.invalidCheckpoint
  if (counts.ownedObjectUnknownFiles > 0) return t.ownershipConflict
  if (counts.collisionFiles > 0) return t.collision
  return worker.status === 'Paused' ? t.paused : t.partial
}

function appendCount(lines: string[], count: number, format: (count: number) => string): void {
  if (count === 0) return
  lines.push(format(count))
}
