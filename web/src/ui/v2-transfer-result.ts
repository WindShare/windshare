import type { TransferWorkerSettlement } from '../transfer/outcome'

export type TransferResultTone = 'success' | 'warning'

export interface TransferResultPresentation {
  readonly title: string
  readonly tone: TransferResultTone
  readonly lines: readonly string[]
}

export function presentTransferResult(
  worker: TransferWorkerSettlement,
): TransferResultPresentation {
  if (worker.status === 'Succeeded') {
    return Object.freeze({
      title: 'Transfer completed',
      tone: 'success',
      lines: Object.freeze([]),
    })
  }

  const counts = worker.fileOutcomes
  const lines: string[] = []
  appendCount(lines, counts.sourceDriftFiles,
    'file stopped because authenticated source content changed',
    'files stopped because authenticated source content changed')
  appendCount(lines, counts.revisionConflictFiles,
    'file was blocked because local resume data belongs to another source revision',
    'files were blocked because local resume data belongs to another source revision')
  appendCount(lines, counts.checkpointInvalidFiles,
    'file was blocked by an invalid local resume checkpoint binding',
    'files were blocked by invalid local resume checkpoint bindings')
  appendCount(lines, counts.ownedObjectUnknownFiles,
    'file was blocked because local resume data disagrees about the owned destination object',
    'files were blocked because local resume data disagrees about the owned destination object')
  appendCount(lines, counts.collisionFiles,
    'existing destination prevented a file from completing',
    'existing destinations prevented files from completing')
  appendCount(lines, counts.failedFiles, 'file failed for another reason', 'files failed for another reason')

  const directoryFailures = worker.failureCount - worker.fileFailureCount
  appendCount(lines, directoryFailures, 'directory did not finish', 'directories did not finish')
  if (lines.length === 0 && worker.status === 'Paused') lines.push('The transfer paused before completion.')

  return Object.freeze({
    title: resultTitle(worker),
    tone: 'warning',
    lines: Object.freeze(lines),
  })
}

function resultTitle(worker: TransferWorkerSettlement): string {
  const counts = worker.fileOutcomes
  if (counts.sourceDriftFiles > 0) return 'Source content changed'
  if (counts.revisionConflictFiles > 0) return 'Resume revision conflict'
  if (counts.checkpointInvalidFiles > 0) return 'Invalid resume checkpoint'
  if (counts.ownedObjectUnknownFiles > 0) return 'Resume ownership conflict'
  if (counts.collisionFiles > 0) return 'Existing destinations prevented completion'
  return worker.status === 'Paused' ? 'Transfer paused' : 'Some items did not finish'
}

function appendCount(lines: string[], count: number, singular: string, plural: string): void {
  if (count === 0) return
  lines.push(`${count} ${count === 1 ? singular : plural}.`)
}
