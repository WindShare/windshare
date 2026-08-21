import {
  compatibleNameRepairSummary,
  type CompatibleNameRepairSummary,
} from '../output/file-system-access/compatible-name/model'
import type { TransferWorkerSettlement } from '../transfer/outcome'
import type { ReceiveLifecycleState } from '../output/workspace/state'
import { hasValidatedTerminalCompatibleNameRepair } from './v2-lifecycle-presentation'

export type TransferResultTone = 'success' | 'warning'

export interface TransferResultPresentation {
  readonly title: string
  readonly tone: TransferResultTone
  readonly lines: readonly string[]
}

export function presentTransferResult(
  worker: TransferWorkerSettlement,
  repairSummary?: CompatibleNameRepairSummary | null,
  lifecycle?: ReceiveLifecycleState,
): TransferResultPresentation {
  const ordinary = presentOrdinaryTransferResult(worker, lifecycle)
  if (repairSummary === undefined || repairSummary === null) return ordinary

  const repair = compatibleNameRepairSummary(repairSummary)
  const lines = [...ordinary.lines]
  if (worker.status === 'CompletedWithErrors') {
    lines.unshift(`${ordinary.title}.`)
  }
  lines.push(
    `Saved names remain compatible until the restoration script "${repair.pairDisplayNames.script}" runs.`,
  )

  if (worker.status === 'Paused' &&
      !(lifecycle?.kind === 'partial-directory' && lifecycle.reason === 'stopped')) {
    return result('Paused with compatible names', 'warning', lines)
  }
  if (!hasValidatedTerminalCompatibleNameRepair(repair)) {
    return result('Compatible-name restoration catch-up required', 'warning', lines)
  }
  return worker.status === 'Succeeded'
    ? result('Completed with compatible names', 'success', lines)
    : result('Partial with compatible names', 'warning', lines)
}

function presentOrdinaryTransferResult(
  worker: TransferWorkerSettlement,
  lifecycle?: ReceiveLifecycleState,
): TransferResultPresentation {
  if (worker.status === 'Succeeded') {
    return result('Transfer completed', 'success', [])
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
  appendCount(lines, counts.failedFiles,
    'file failed for another reason',
    'files failed for another reason')

  const directoryFailures = worker.failureCount - worker.fileFailureCount
  appendCount(lines, directoryFailures, 'directory did not finish', 'directories did not finish')
  if (lines.length === 0 && worker.status === 'Paused') {
    lines.push('The transfer paused before completion.')
  }

  return result(
    lifecycle?.kind === 'partial-directory' && lifecycle.reason === 'stopped'
      ? 'Transfer stopped'
      : resultTitle(worker),
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
