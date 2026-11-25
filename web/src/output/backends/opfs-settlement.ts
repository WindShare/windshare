import { catalogCapacityReason, classifyNativeError, transferFailureReason } from '../../transfer/fault'
import type { SettledTransferJobResult } from '../../transfer/job'
import { transitionReceiveState } from '../../transfer/state'
import { emitOutputTrace, type OutputTraceSource } from '../diagnostics'
import type { ReceiveStore } from '../store/database'
import { maxRangeEnd, type OPFSTargetRecord, type ReceiveFileRecord, type ReceiveTaskRecord } from '../store/types'
import { hasZipLayout } from '../zip/stored-members'
import {
  artifactDownloadMediaType,
  triggerBrowserDownload,
  type BrowserDownloadWindow,
} from './browser-handoff'
import { mergeSourceOmittedEntries } from './contract'

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

export function completedOPFSTaskRecord(
  record: ReceiveTaskRecord,
  target: OPFSTargetRecord,
  lifecycle: ReceiveTaskRecord['lifecycle'],
  sourceOmittedEntries: bigint | undefined,
  updatedAt = Date.now(),
): ReceiveTaskRecord {
  const copy: { -readonly [K in keyof ReceiveTaskRecord]: ReceiveTaskRecord[K] } = {
    ...record,
    target,
    lifecycle,
    discoveryComplete: true,
    updatedAt,
  }
  if (sourceOmittedEntries === undefined) delete copy.sourceOmittedEntries
  else copy.sourceOmittedEntries = sourceOmittedEntries
  return Object.freeze(copy)
}

export interface PublishCompletedOPFSInput {
  readonly record: ReceiveTaskRecord
  readonly fileHandle: Pick<FileSystemFileHandle, 'getFile'>
  readonly sealedSize: bigint
  readonly allocatedLength: bigint
  readonly result: SettledTransferJobResult
  readonly store: Pick<ReceiveStore, 'putTask'>
  readonly windowPort: BrowserDownloadWindow
  readonly trace?: OutputTraceSource
  readonly onRecordPersisted: (record: ReceiveTaskRecord) => void
  readonly onTruncated: (physicalSize: bigint) => Promise<ReceiveTaskRecord>
}

export async function publishCompletedOPFSTask(input: PublishCompletedOPFSInput): Promise<ReceiveTaskRecord> {
  let file: File | undefined
  try {
    file = await input.fileHandle.getFile()
  } catch (error) {
    if (error instanceof DOMException && error.name === 'NotFoundError') {
      return input.onTruncated(0n)
    }
    // A transient failure to read a committed object can be retried from Downloads.
  }
  if (file !== undefined && BigInt(file.size) !== input.sealedSize) {
    return input.onTruncated(BigInt(file.size))
  }

  const currentTarget = input.record.target as OPFSTargetRecord
  const target: OPFSTargetRecord = Object.freeze({
    ...currentTarget,
    allocatedLength: input.allocatedLength,
    sealedSize: input.sealedSize,
  })
  const readyLifecycle = readyToSaveLifecycle(
    input.record.lifecycle,
    input.sealedSize,
    target.fileName,
    input.result.contentWarning,
  )
  const sourceOmittedEntries = mergeSourceOmittedEntries(
    input.result.sourceOmittedEntries,
    input.record.sourceOmittedEntries,
    input.result.discoveryComplete,
  )

  // Persist the sealed `ready-to-save` barrier before browser handoff so a close/crash after
  // `anchor.click()` still leaves the completed ZIP offline-exportable rather than paused.
  const { record: readyRecord, persistError } = await persistReadyToSave(
    input,
    target,
    readyLifecycle,
    sourceOmittedEntries,
  )
  // A late stop or pause leaves a complete file in `ready-to-save` for the user to export on demand.
  if (input.result.status !== 'completed' || file === undefined) {
    if (persistError !== undefined) throw persistError
    return readyRecord
  }

  // Even when IndexedDB cannot persist `ready-to-save`, still hand off the verified local file so
  // a metadata write failure never withholds an already-completed download from the user.
  const handedOffLifecycle = handoffCompletedFile(
    input,
    file,
    target.fileName,
    readyLifecycle,
    sourceOmittedEntries,
  )
  if (persistError !== undefined) throw persistError
  if (handedOffLifecycle === readyRecord.lifecycle) return readyRecord
  return persistDownloadStarted(input, readyRecord, target, handedOffLifecycle, sourceOmittedEntries)
}

async function persistReadyToSave(
  input: PublishCompletedOPFSInput,
  target: OPFSTargetRecord,
  readyLifecycle: ReceiveTaskRecord['lifecycle'],
  sourceOmittedEntries: bigint | undefined,
): Promise<Readonly<{ record: ReceiveTaskRecord; persistError?: unknown }>> {
  if (isPersistedCompletedRecord(input.record, target, readyLifecycle, sourceOmittedEntries)) {
    return { record: input.record }
  }
  const candidate = completedOPFSTaskRecord(input.record, target, readyLifecycle, sourceOmittedEntries)
  try {
    await input.store.putTask(candidate)
    input.onRecordPersisted(candidate)
    emitOutputTrace(input.trace, 'publication', {
      backend: 'origin_private',
      task_id: input.record.taskId,
      transition: 'ready_to_save',
      sealed_bytes: String(target.sealedSize),
    })
    return { record: candidate }
  } catch (persistError) {
    emitOutputTrace(input.trace, 'publication', {
      backend: 'origin_private',
      task_id: input.record.taskId,
      transition: 'ready_to_save_persist_failed',
      error_class: classifyNativeError(persistError),
    })
    return { record: input.record, persistError }
  }
}

function handoffCompletedFile(
  input: PublishCompletedOPFSInput,
  file: File,
  fileName: string,
  lifecycle: ReceiveTaskRecord['lifecycle'],
  sourceOmittedEntries: bigint | undefined,
): ReceiveTaskRecord['lifecycle'] {
  try {
    const mediaType = artifactDownloadMediaType(input.record.artifact.kind)
    triggerBrowserDownload(input.windowPort, file, fileName, mediaType)
    emitOutputTrace(input.trace, 'publication', {
      backend: 'origin_private',
      task_id: input.record.taskId,
      transition: 'committed',
      ...(sourceOmittedEntries === undefined ? {} : { source_omitted_entries: String(sourceOmittedEntries) }),
    })
    return transitionReceiveState(lifecycle, { kind: 'download-started', retained: true, fileName })
  } catch {
    /* Downloads can retry a handoff that needs a fresh user gesture. */
    return lifecycle
  }
}

async function persistDownloadStarted(
  input: PublishCompletedOPFSInput,
  readyRecord: ReceiveTaskRecord,
  target: OPFSTargetRecord,
  lifecycle: ReceiveTaskRecord['lifecycle'],
  sourceOmittedEntries: bigint | undefined,
): Promise<ReceiveTaskRecord> {
  const updated = completedOPFSTaskRecord(readyRecord, target, lifecycle, sourceOmittedEntries)
  try {
    await input.store.putTask(updated)
    input.onRecordPersisted(updated)
    return updated
  } catch (error) {
    // `ready-to-save` is already durable and browser handoff succeeded; keep the persisted state
    // instead of failing a completed download.
    emitOutputTrace(input.trace, 'publication', {
      backend: 'origin_private',
      task_id: input.record.taskId,
      transition: 'handoff_persist_failed',
      error_class: classifyNativeError(error),
    })
    return readyRecord
  }
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
      const reason = transferFailureReason(error)
      return transitionReceiveState(current, {
        kind: 'failed',
        reason,
        ...(reason === 'error' && error instanceof Error ? { detail: error.message } : {}),
      })
    }
  }
}
