import { bigintToSafeNumber, coveredBytes, type ByteRange } from '../../content/geometry'
import {
  classifyNativeError, FaultScope, FileDestinationConflictError, OutputFaultCode, RetryableFilePublishError, TransferFaultError,
  isPermissionDeniedError, outputFailure, outputFault,
} from '../../transfer/fault'
import type { CheckpointOutcome, CheckpointReason, TransferFileRequest, TransferOutputFile } from '../../transfer/output-port'
import { emitOutputTrace, type OutputDiagnosticsPorts } from '../diagnostics'
import type { ReceiveStore } from '../store/database'
import { fileRecordId, fromRangeTuples, maxRangeEnd, toRangeTuples, type FSADiskFingerprint, type ReceiveFileRecord } from '../store/types'
import {
  inspectFSALeaf, matchesFSADiskFingerprint, readFSADiskFingerprint, readFSALeafFingerprint,
  removeUncommittedEmptyPlaceholder, removeVerifiedFSALeaf,
} from './fsa-cleanup'

// Cap the extra network and disk work of replaying an unpublished file after a transient native close failure.
const MAX_FSA_PUBLISH_RETRY_BYTES = 128n * 1024n

export function destinationChanged(path: string, cause?: unknown): TransferFaultError {
  if (isPermissionDeniedError(cause)) {
    return new TransferFaultError(outputFault(FaultScope.FileLocal, OutputFaultCode.PermissionDenied),
      `Allow folder access to verify "${path}" and continue saving.`, { cause })
  }
  return new FileDestinationConflictError(path, { cause })
}

async function readExistingFingerprint(handle: FileSystemFileHandle, path: string): Promise<FSADiskFingerprint> {
  return readFSADiskFingerprint(handle).catch((cause: unknown) => { throw destinationChanged(path, cause) })
}

function verifyStoredExtent(fingerprint: FSADiskFingerprint, ranges: ReceiveFileRecord['completedRanges'], size: bigint, path: string): void {
  if (BigInt(fingerprint.size) !== maxRangeEnd(ranges) || BigInt(fingerprint.size) > size) throw destinationChanged(path)
}

export async function verifyExistingFSAFile(
  record: ReceiveFileRecord, handle: FileSystemFileHandle, path: string,
): Promise<ReceiveFileRecord> {
  const disk = await readExistingFingerprint(handle, path)
  if (record.status === 'completed') {
    if (BigInt(disk.size) !== record.size) throw destinationChanged(path)
    return record
  }
  const { fsaPendingPublish, ...confirmed } = record
  const fingerprint = fsaPendingPublish?.fingerprint ?? record.fsaDiskFingerprint
  if (!matchesFSADiskFingerprint(disk, fingerprint)) throw destinationChanged(path)
  const ranges = fsaPendingPublish?.ranges ?? record.completedRanges
  verifyStoredExtent(disk, ranges, record.size, path)
  return fsaPendingPublish === undefined ? record :
    { ...confirmed, handle, completedRanges: ranges, fsaDiskFingerprint: disk }
}

interface FSAFileOptions {
  taskId: string
  request: TransferFileRequest
  parentDir: FileSystemDirectoryHandle
  handle: FileSystemFileHandle
  physicalPath: readonly string[]
  displayPath: string
  openedAtMs: number
  signal: AbortSignal
  store: Pick<ReceiveStore, 'putFile' | 'deleteFiles'>
  existing: ReceiveFileRecord | undefined
  diagnostics: OutputDiagnosticsPorts | undefined
}

interface ObservedPublication {
  readonly fingerprint: FSADiskFingerprint
  readonly record: ReceiveFileRecord
}

type UnrecordedFile = { kind: 'placeholder'; acquiredAtMs: number } | { kind: 'published'; observed?: ObservedPublication }

export class FSAOutputFile implements TransferOutputFile {
  readonly completedRanges: readonly ByteRange[]
  readonly savedRevision?: string
  readonly savedSourceMetadata?: string
  readonly savedSize?: bigint
  readonly #options: FSAFileOptions
  #checkpoint: ReceiveFileRecord | undefined
  #savedBytes: bigint
  #unrecorded: UnrecordedFile | undefined
  #hasIntent = false
  #writer: FileSystemWritableFileStream | undefined
  #failure: TransferFaultError | undefined

  constructor(options: FSAFileOptions) {
    this.#options = options
    const prior = options.existing
    this.#checkpoint = prior
    this.#unrecorded = prior === undefined ? { kind: 'placeholder', acquiredAtMs: Date.now() } : undefined
    this.completedRanges = fromRangeTuples(prior?.completedRanges ?? [])
    this.#savedBytes = coveredBytes(this.completedRanges)
    if (prior !== undefined) {
      this.savedRevision = prior.revision
      this.savedSize = prior.size
      if (prior.sourceMetadata !== undefined) this.savedSourceMetadata = prior.sourceMetadata
    }
  }

  async writeAt(offset: bigint, data: Uint8Array): Promise<void> {
    this.#requireHealthy()
    const { handle, signal } = this.#options
    signal.throwIfAborted()
    try {
      if (this.#writer === undefined) {
        if (this.#checkpoint !== undefined) await this.#verifyCurrent()
        signal.throwIfAborted()
        this.#writer = await handle.createWritable({ keepExistingData: this.#savedBytes > 0n })
        // Copying a multi-GB prefix can outlive the lease or user action that requested this write.
        if (signal.aborted) {
          await this.#writer.abort(signal.reason).catch(() => undefined)
          this.#writer = undefined
          throw signal.reason
        }
      }
      await this.#writer.write({ type: 'write', position: bigintToSafeNumber(offset, 'FSA write offset'), data: data.slice() })
    } catch (cause) {
      if (signal.aborted && cause === signal.reason) throw cause
      throw this.#fail(cause, OutputFaultCode.WriteFailed)
    }
  }

  async checkpoint(ranges: readonly ByteRange[], reason: CheckpointReason): Promise<CheckpointOutcome> {
    this.#requireHealthy()
    const gained = coveredBytes(ranges) - this.#savedBytes
    // Reopening copies the published prefix. Doubling durable coverage bounds the total copy cost.
    if (reason === 'disconnect' && (gained <= 0n || gained < this.#savedBytes)) {
      emitOutputTrace(this.#options.diagnostics?.trace, 'checkpoint', {
        backend: 'file_system_access', task_id: this.#options.taskId, file_id: this.#options.request.fileId,
        trigger: reason, outcome: 'skipped', previous_saved_bytes: String(this.#savedBytes), added_bytes: String(gained),
      })
      return 'skipped'
    }
    await this.#publish('receiving', ranges)
    return 'saved'
  }

  async commit(ranges: readonly ByteRange[]): Promise<void> {
    await this.#publish('completed', ranges)
    emitOutputTrace(this.#options.diagnostics?.trace, 'output_write', {
      backend: 'file_system_access', task_id: this.#options.taskId, file_id: this.#options.request.fileId,
      transition: 'transaction_committed',
    })
  }

  pause(ranges: readonly ByteRange[]): Promise<void> {
    return this.#publish('receiving', ranges)
  }

  async abort(reason?: unknown): Promise<void> {
    await this.#writer?.abort(reason).catch(() => undefined)
    this.#writer = undefined
    try {
      await this.#reconcileAbortedPublish()
    } catch (cause) {
      this.#traceRecovery('cleanup_deferred', cause)
    }
  }

  async #reconcileAbortedPublish(): Promise<void> {
    const { parentDir, handle, openedAtMs, store, request, taskId } = this.#options
    const file = this.#unrecorded
    if (file === undefined) {
      // A failed close may have left the previous checkpoint intact. A crash here can repeat this verification.
      if (this.#hasIntent && this.#checkpoint !== undefined) {
        await this.#verifyCurrent()
        await store.putFile(this.#checkpoint)
        this.#hasIntent = false
      }
      return
    }
    const name = this.#options.physicalPath.at(-1)!
    const outcome = file.kind === 'placeholder'
      ? await removeUncommittedEmptyPlaceholder(parentDir, name, handle, openedAtMs, file.acquiredAtMs)
      : await this.#recoverPublication(file.observed)
    if (outcome !== 'missing' && outcome !== 'removed') return
    if (this.#hasIntent) await store.deleteFiles([fileRecordId(taskId, request.fileId)])
    this.#hasIntent = false
    this.#unrecorded = undefined
  }

  async #recoverPublication(observed: ObservedPublication | undefined): Promise<'unverified' | 'missing' | 'changed' | 'recorded' | 'preserved' | 'removed'> {
    if (observed === undefined) return 'unverified'
    const { parentDir, handle, physicalPath, store } = this.#options
    const name = physicalPath.at(-1)!
    const state = await inspectFSALeaf(parentDir, name, handle, observed.fingerprint)
    if (state !== 'matching') return state
    try {
      await store.putFile(observed.record)
      this.#unrecorded = undefined
      this.#hasIntent = false
      this.#traceRecovery('publication_recovered')
      return 'recorded'
    } catch (cause) {
      this.#traceRecovery('publication_record_failed', cause)
    }
    // A metadata outage cannot revoke a prefix already promised to the user, or a completely written file.
    if (this.#savedBytes > 0n || observed.record.status === 'completed') return 'preserved'
    return removeVerifiedFSALeaf(parentDir, name, handle, observed.fingerprint)
  }

  async #publish(status: 'receiving' | 'completed', ranges: readonly ByteRange[]): Promise<void> {
    this.#requireHealthy()
    const { store, displayPath } = this.#options
    const row = this.#row(status, toRangeTuples(ranges))
    try {
      if (this.#writer !== undefined) {
        if (status === 'receiving' || this.#checkpoint !== undefined) await this.#recordIntent()
        await this.#closeForPublication(this.#writer, status)
      } else if (this.#checkpoint !== undefined) {
        await this.#verifyCurrent()
        // With no intervening write, pause and retry already have a durable checkpoint.
        if (status === 'receiving') return
      }
      const confirmed = await this.#confirmRow(row)
      await store.putFile(confirmed)
      this.#checkpoint = confirmed
      this.#savedBytes = coveredBytes(ranges)
      this.#unrecorded = undefined
      this.#hasIntent = false
    } catch (cause) {
      if (this.#unrecorded?.kind === 'published') {
        this.#traceRecovery('publication_unrecorded', cause)
        if (!(cause instanceof TransferFaultError)) {
          const fault = outputFailure(FaultScope.FileLocal, cause, OutputFaultCode.CommitFailed).fault
          throw this.#fail(new TransferFaultError(fault,
            `WindShare could not record the write to "${displayPath}". Check that file before restarting the download.`, { cause }),
          OutputFaultCode.CommitFailed)
        }
      }
      throw this.#fail(cause, OutputFaultCode.CommitFailed)
    }
  }

  async #closeForPublication(writer: FileSystemWritableFileStream, status: 'receiving' | 'completed'): Promise<void> {
    try {
      await writer.close()
    } catch (cause) {
      const { request, signal } = this.#options
      const file = this.#unrecorded
      if (status === 'completed' && file?.kind === 'placeholder' && !this.#hasIntent &&
          request.exactSize <= MAX_FSA_PUBLISH_RETRY_BYTES && !signal.aborted &&
          cause instanceof DOMException && cause.name === 'InvalidStateError' &&
          await this.#retireRejectedPlaceholder(writer, file.acquiredAtMs, cause)) {
        throw new RetryableFilePublishError(cause)
      }
      throw cause
    }
    this.#writer = undefined
    this.#unrecorded = { kind: 'published' }
  }

  async #retireRejectedPlaceholder(writer: FileSystemWritableFileStream, acquiredAtMs: number, cause: DOMException): Promise<boolean> {
    const { parentDir, physicalPath, handle, openedAtMs, diagnostics, taskId, request } = this.#options
    try {
      await writer.abort(cause)
      this.#writer = undefined
      const outcome = await removeUncommittedEmptyPlaceholder(parentDir, physicalPath.at(-1)!, handle, openedAtMs, acquiredAtMs)
      emitOutputTrace(diagnostics?.trace, 'output_write', {
        backend: 'file_system_access', task_id: taskId, file_id: request.fileId,
        transition: 'fresh_publish_rejected', placeholder: outcome, error: classifyNativeError(cause),
      })
      if (outcome !== 'removed') return false
      this.#unrecorded = undefined
      return true
    } catch (cleanupError) {
      this.#traceRecovery('cleanup_deferred', cleanupError)
      return false
    }
  }

  async #confirmRow(row: ReceiveFileRecord): Promise<ReceiveFileRecord> {
    const { displayPath } = this.#options
    if (row.status === 'completed' && !(this.#hasIntent && this.#unrecorded?.kind === 'published')) return row
    const fingerprint = await this.#readCurrent()
    verifyStoredExtent(fingerprint, row.completedRanges, row.size, displayPath)
    const confirmed = row.status === 'receiving' ? { ...row, fsaDiskFingerprint: fingerprint } : row
    if (this.#unrecorded?.kind === 'published') this.#unrecorded = { kind: 'published', observed: { fingerprint, record: confirmed } }
    return confirmed
  }

  async #recordIntent(): Promise<void> {
    const { store, displayPath } = this.#options
    const fingerprint = await this.#verifyCurrent()
    if (this.#checkpoint === undefined && fingerprint.size !== 0) throw destinationChanged(displayPath)
    // IndexedDB and the OS cannot share a commit. Recovery may trust the earlier version only if it still matches.
    const intent = { fingerprint, ranges: this.#checkpoint?.completedRanges ?? [] }
    await store.putFile({ ...this.#row('receiving', []), fsaPendingPublish: intent })
    this.#hasIntent = true
  }

  async #verifyCurrent(): Promise<FSADiskFingerprint> {
    const disk = await this.#readCurrent()
    const checkpoint = this.#checkpoint
    if (checkpoint === undefined) return disk
    const matches = checkpoint.status === 'completed' ? BigInt(disk.size) === checkpoint.size :
      matchesFSADiskFingerprint(disk, checkpoint.fsaDiskFingerprint)
    if (!matches) throw destinationChanged(this.#options.displayPath)
    return disk
  }

  async #readCurrent(): Promise<FSADiskFingerprint> {
    const { parentDir, handle, physicalPath, displayPath } = this.#options
    const fingerprint = await readFSALeafFingerprint(parentDir, physicalPath.at(-1)!, handle)
      .catch((cause: unknown) => { throw destinationChanged(displayPath, cause) })
    if (fingerprint === undefined) throw destinationChanged(displayPath)
    return fingerprint
  }

  #row(status: 'receiving' | 'completed', completedRanges: ReceiveFileRecord['completedRanges']): ReceiveFileRecord {
    const { taskId, request, physicalPath, handle } = this.#options
    return {
      id: fileRecordId(taskId, request.fileId), taskId, fileId: request.fileId,
      revision: request.revision, path: request.path, fsaPath: physicalPath, size: request.exactSize,
      ...(request.sourceMetadata === undefined ? {} : { sourceMetadata: request.sourceMetadata }),
      completedRanges, status, handle,
    }
  }

  #traceRecovery(transition: string, cause?: unknown): void {
    const { diagnostics, taskId, request } = this.#options
    emitOutputTrace(diagnostics?.trace, 'checkpoint', {
      backend: 'file_system_access', task_id: taskId, file_id: request.fileId, transition,
      ...(cause === undefined ? {} : { error: classifyNativeError(cause) }),
    })
  }

  #requireHealthy(): void {
    if (this.#failure !== undefined) throw this.#failure
  }

  #fail(cause: unknown, code: OutputFaultCode): TransferFaultError {
    this.#failure = outputFailure(FaultScope.FileLocal, cause, code)
    return this.#failure
  }
}
