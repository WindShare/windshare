import { catalogCapacityReason } from '../../transfer/fault'
import type { SettledTransferJobResult } from '../../transfer/job'
import { transitionReceiveState } from '../../transfer/state'
import { maxRangeEnd, type OPFSTargetRecord, type ReceiveFileRecord, type ReceiveTaskRecord } from '../store/types'
import { hasZipLayout } from '../zip/stored-members'

export type IncompleteOPFSJobStatus = Exclude<SettledTransferJobResult['status'], 'completed'>

export function hasCompletedZipFile(files: readonly ReceiveFileRecord[]): boolean {
  return files.some(file => file.status === 'completed' && file.zipEntry?.kind !== 'directory')
}

export function completedZipTailOffset(files: readonly ReceiveFileRecord[]): bigint {
  let tail = 0n
  for (const file of files) {
    if (file.status !== 'completed' || !hasZipLayout(file)) continue
    // Empty files and directories regenerate headers without reading bytes from the OPFS object.
    if (file.zipEntry.kind !== 'file' || file.size === 0n) continue
    if (file.zipEntry.endOffset > tail) tail = file.zipEntry.endOffset
  }
  return tail
}

export function readyToSaveLifecycle(
  current: ReceiveTaskRecord['lifecycle'],
  sealedSize: bigint,
  fileName: string,
  contentWarning: SettledTransferJobResult['contentWarning'],
): ReceiveTaskRecord['lifecycle'] {
  if (
    current.kind === 'ready-to-save' &&
    current.size === sealedSize &&
    current.fileName === fileName &&
    contentWarning === undefined &&
    current.contentWarning === undefined
  ) {
    return current
  }
  return transitionReceiveState(current, {
    kind: 'ready-to-save',
    size: sealedSize,
    fileName,
    contentWarning: contentWarning ?? null,
  })
}

export function isPersistedCompletedRecord(
  record: ReceiveTaskRecord,
  target: OPFSTargetRecord,
  lifecycle: ReceiveTaskRecord['lifecycle'],
  sourceOmittedEntries: bigint | undefined,
): boolean {
  const current = record.target as OPFSTargetRecord
  return lifecycle === record.lifecycle &&
    current.sealedSize === target.sealedSize &&
    current.allocatedLength === target.allocatedLength &&
    sourceOmittedEntries === record.sourceOmittedEntries &&
    record.discoveryComplete
}

export function survivingFileCoverage(row: ReceiveFileRecord, physicalSize: bigint): ReceiveFileRecord {
  if (row.status === 'completed' ? physicalSize === row.size : maxRangeEnd(row.completedRanges) <= physicalSize) return row
  const completedRanges = row.completedRanges.flatMap(([start, end]) =>
    start < physicalSize ? [[start, end < physicalSize ? end : physicalSize] as const] : [])
  return { ...row, status: 'receiving', completedRanges }
}

export function nextOPFSIncompleteLifecycle(
  current: ReceiveTaskRecord['lifecycle'],
  status: IncompleteOPFSJobStatus,
  result: SettledTransferJobResult,
  input: Readonly<{
    isZip: boolean
    retainedBytes: bigint
    storedCompletedFiles: bigint
    storedCompletedBytes: bigint
  }>,
): ReceiveTaskRecord['lifecycle'] {
  const warning = result.contentWarning ?? null
  if (status === 'paused') {
    return transitionReceiveState(current, {
      kind: 'paused',
      reason: result.pauseReason ?? 'user',
      completedFiles: input.storedCompletedFiles,
      completedBytes: input.storedCompletedBytes,
      retainedBytes: input.retainedBytes,
      discoveredFiles: result.discoveredFiles,
      discoveredBytes: result.discoveredBytes,
      discoveryComplete: result.discoveryComplete,
      contentWarning: warning,
    })
  }

  const failedCount = result.failedFiles + result.failedDirectories
  const savedFiles = input.storedCompletedFiles
  if (input.isZip && savedFiles > 0n) {
    // Keep completed OPFS ZIP members exportable via zero-copy `buildPartialZipBlob`
    // instead of sealing in place, which would turn pre-allocated failed slots into dead padding.
    const reason = catalogCapacityReason(result.error) ?? (
      status === 'stopped' || (status === 'partial' && failedCount === 0n)
        ? 'stopped'
        : 'failures'
    )
    return transitionReceiveState(current, {
      kind: 'partial',
      reason,
      savedFiles,
      failedFiles: failedCount,
      contentWarning: warning,
    })
  }

  return failedOPFSLifecycle(current, status, result.error)
}

function failedOPFSLifecycle(
  current: ReceiveTaskRecord['lifecycle'],
  status: Exclude<IncompleteOPFSJobStatus, 'paused'>,
  error: unknown,
): ReceiveTaskRecord['lifecycle'] {
  switch (status) {
    case 'stopped':
      return transitionReceiveState(current, { kind: 'failed', reason: 'cancelled' })
    case 'source-changed':
      return transitionReceiveState(current, { kind: 'failed', reason: 'source-changed' })
    case 'partial':
    case 'failed': {
      const reason = catalogCapacityReason(error) ?? 'error'
      return transitionReceiveState(current, {
        kind: 'failed',
        reason,
        ...(reason === 'error' && error instanceof Error ? { detail: error.message } : {}),
      })
    }
  }
}
