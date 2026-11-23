import { bigintToSafeNumber, coveredBytes, type ByteRange } from '../../content/geometry'
import { FaultScope, OutputFaultCode, TransferFaultError, isPermissionDeniedError, outputFailure, outputFault } from '../../transfer/fault'
import type { CheckpointOutcome, CheckpointReason, TransferFileRequest, TransferOutputFile } from '../../transfer/output-port'
import { emitOutputTrace, type OutputDiagnosticsPorts } from '../diagnostics'
import type { ReceiveStore } from '../store/database'
import { fileRecordId, fromRangeTuples, maxRangeEnd, toRangeTuples, type FSADiskFingerprint, type ReceiveFileRecord } from '../store/types'
import { matchesFSADiskFingerprint, readFSADiskFingerprint, removeUncommittedEmptyPlaceholder } from './fsa-cleanup'

function destinationChanged(cause?: unknown): TransferFaultError {
  return new TransferFaultError(outputFault(FaultScope.FileLocal, OutputFaultCode.TargetCollision),
    'The saved file changed or could not be verified. Its contents were preserved.', { cause })
}

async function readExistingFingerprint(handle: FileSystemFileHandle): Promise<FSADiskFingerprint> {
  return readFSADiskFingerprint(handle).catch((error: unknown) => {
    if (isPermissionDeniedError(error)) throw error
    throw destinationChanged(error)
  })
}

async function verifyPrefix(handle: FileSystemFileHandle, expected: FSADiskFingerprint | undefined): Promise<void> {
  if (!matchesFSADiskFingerprint(await readExistingFingerprint(handle), expected)) throw destinationChanged()
}

function verifyStoredExtent(
  fingerprint: FSADiskFingerprint | undefined,
  ranges: ReceiveFileRecord['completedRanges'],
  size: bigint,
): void {
  if (fingerprint === undefined || BigInt(fingerprint.size) < maxRangeEnd(ranges) || BigInt(fingerprint.size) > size) {
    throw destinationChanged()
  }
}

export async function verifyExistingFSAFile(record: ReceiveFileRecord): Promise<void> {
  if (record.handle === undefined) throw destinationChanged()
  if (record.status === 'completed') {
    // Completed files have no partial-file fingerprint. They can only be reused as a whole.
    const disk = await readExistingFingerprint(record.handle)
    if (BigInt(disk.size) !== record.size) throw destinationChanged()
  } else if (record.completedRanges.length > 0) {
    const fingerprint = record.fsaDiskFingerprint
    verifyStoredExtent(fingerprint, record.completedRanges, record.size)
    await verifyPrefix(record.handle, fingerprint)
  }
}

interface FSAFileOptions {
  taskId: string
  request: TransferFileRequest
  parentDir: FileSystemDirectoryHandle
  handle: FileSystemFileHandle
  physicalName: string
  openedAtMs: number
  store: ReceiveStore
  existing: ReceiveFileRecord | undefined
  diagnostics: OutputDiagnosticsPorts | undefined
}

export class FSAOutputFile implements TransferOutputFile {
  readonly completedRanges: readonly ByteRange[]
  readonly savedRevision?: string
  readonly savedSourceMetadata?: string
  readonly savedSize?: bigint
  readonly #options: FSAFileOptions
  #savedBytes: bigint
  #fingerprint: FSADiskFingerprint | undefined
  #freshPlaceholder: boolean
  #writer: FileSystemWritableFileStream | undefined
  #failure: TransferFaultError | undefined

  constructor(options: FSAFileOptions) {
    this.#options = options
    const prior = options.existing
    this.#freshPlaceholder = prior === undefined
    this.completedRanges = fromRangeTuples(prior?.completedRanges ?? [])
    this.#savedBytes = coveredBytes(this.completedRanges)
    this.#fingerprint = prior?.fsaDiskFingerprint
    if (prior !== undefined) {
      this.savedRevision = prior.revision
      this.savedSize = prior.size
      if (prior.sourceMetadata !== undefined) this.savedSourceMetadata = prior.sourceMetadata
    }
  }

  async writeAt(offset: bigint, data: Uint8Array): Promise<void> {
    this.#requireHealthy()
    try {
      if (this.#writer === undefined) {
        if (this.#savedBytes > 0n) await verifyPrefix(this.#options.handle, this.#fingerprint)
        this.#writer = await this.#options.handle.createWritable({ keepExistingData: this.#savedBytes > 0n })
      }
      await this.#writer.write({ type: 'write', position: bigintToSafeNumber(offset, 'FSA write offset'), data: data.slice() })
    } catch (cause) {
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
      backend: 'file_system_access', transition: 'transaction_committed',
    })
  }

  pause(ranges: readonly ByteRange[]): Promise<void> {
    return this.#publish('receiving', ranges)
  }

  async abort(reason?: unknown): Promise<void> {
    await this.#writer?.abort(reason).catch(() => undefined)
    this.#writer = undefined
    if (this.#freshPlaceholder) {
      this.#freshPlaceholder = false
      const { parentDir, physicalName, handle, openedAtMs } = this.#options
      await removeUncommittedEmptyPlaceholder(parentDir, physicalName, handle, openedAtMs)
    }
  }

  async #publish(status: 'receiving' | 'completed', ranges: readonly ByteRange[]): Promise<void> {
    this.#requireHealthy()
    const { taskId, request, physicalName, handle, store } = this.#options
    try {
      const writer = this.#writer
      if (writer !== undefined) {
        await writer.close()
        this.#writer = undefined
      } else if (this.#savedBytes > 0n) {
        // A pause or final commit can follow a disconnect cut without any intervening write.
        await verifyPrefix(handle, this.#fingerprint)
      }
      const fingerprint = status === 'receiving' && (writer !== undefined || this.#fingerprint === undefined)
        ? await readFSADiskFingerprint(handle) : this.#fingerprint
      const completedRanges = toRangeTuples(ranges)
      if (status === 'receiving') verifyStoredExtent(fingerprint, completedRanges, request.exactSize)
      const row: ReceiveFileRecord = {
        id: fileRecordId(taskId, request.fileId), taskId, fileId: request.fileId,
        revision: request.revision, path: request.path, physicalName, size: request.exactSize,
        ...(request.sourceMetadata === undefined ? {} : { sourceMetadata: request.sourceMetadata }),
        completedRanges, status, handle,
        ...(status === 'receiving' && fingerprint !== undefined ? { fsaDiskFingerprint: fingerprint } : {}),
      }
      await store.putFile(row)
      this.#fingerprint = fingerprint
      this.#savedBytes = coveredBytes(ranges)
      this.#freshPlaceholder = false
    } catch (cause) {
      throw this.#fail(cause, OutputFaultCode.CommitFailed)
    }
  }

  #requireHealthy(): void {
    if (this.#failure !== undefined) throw this.#failure
  }

  #fail(cause: unknown, code: OutputFaultCode): TransferFaultError {
    this.#failure = outputFailure(FaultScope.FileLocal, cause, code)
    return this.#failure
  }
}
