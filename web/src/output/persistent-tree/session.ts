import { ByteRangeSet, byteRange } from '../../content/geometry'
import {
  DirectoryAdmissionLedger,
  type DirectoryFileMutationLease,
} from '../../transfer/directory-admission-ledger'
import type { JobOutcome } from '../../transfer/outcome'
import {
  type BeginOutputFileResult,
  type DirectoryAdmission,
  type DirectoryAdmissionScope,
  DirectoryAdmissionBindingError,
  type DirectorySettlement,
  type FileRetirementDisposition,
  type JobSettlement,
  type OutputCapabilities,
  type OutputDirectoryAdmission,
  OutputDirectoryMutationError,
  type OutputFile,
  type OutputSession,
  type OutputSessionIdentity,
  VerifiedDurableRanges,
  MAXIMUM_OPEN_OUTPUT_FILES,
  outputCapabilities,
  outputSessionIdentity,
  snapshotOutputDirectoryAdmission,
} from '../../transfer/output-session'
import {
  BoundaryFaultError,
  CheckpointFaultCode,
  FaultScope,
  checkpointFault,
  consumeFileRetirementAuthorization,
} from '../../transfer/fault'
import {
  type OutputCheckpointJournal,
  type CheckpointNamespaceBinding,
  type PersistedDirectoryRecord,
  type PersistedFileRecord,
  type PersistedOutputRecord,
  directoryRecord,
  fileRecord,
  outputRecordKey,
  sameOutputRecord,
} from '../persistence/journal'
import { durableCheckpointNamespaceIdentity } from '../persistence/namespace'
import type {
  CheckpointCrashHook,
  PersistentOutputTree,
  PersistentTreeFile,
} from './contracts'
import {
  createPersistentFile,
  materializePersistentDirectory,
  reopenPersistentFile,
  type PersistentAcquisitionContext,
} from './acquisition'
import { PersistentOutputError } from './errors'
import { PersistentFileTransaction } from './file-transaction'
import { PersistentOutputLifecycle } from './lifecycle'
import {
  PersistentJournalView,
  type StagedFileFootprint,
  type StagedOutputCatalog,
  type StagedOutputTotals,
} from './journal-view'
import {
  directoryKey,
  fileKey,
  fileOwnership,
  nextGeneration,
  sameOutputSource,
  verifiedFileRanges,
} from './record-identity'
import { recoverOutputRecords } from './recovery'

export { PersistentOutputError } from './errors'
export type {
  StagedFileFootprint,
  StagedOutputCatalog,
  StagedOutputFile,
  StagedOutputTotals,
} from './journal-view'

export interface PersistentTreeOutputOptions {
  readonly identity: OutputSessionIdentity
  readonly directoryAdmissionScope: DirectoryAdmissionScope
  readonly tree: PersistentOutputTree
  readonly journal: OutputCheckpointJournal
  readonly durability?: 'ProcessRestart' | 'PowerLoss'
  readonly maximumOpenFiles?: number
  readonly crashHook?: CheckpointCrashHook
}

export interface PersistentBeginOutputFileResult extends BeginOutputFileResult {
  readonly transaction: PersistentFileTransaction
}

export interface PersistentSessionOperation {
  readonly closing: AbortSignal
  acquireFile(file: OutputFile): DirectoryFileMutationLease
  beginFile(
    file: DirectoryFileMutationLease,
    signal: AbortSignal,
  ): Promise<PersistentBeginOutputFileResult>
  stagedFileFootprint(path: readonly string[]): Promise<StagedFileFootprint>
}

/**
 * The session is the sole authority that turns bytes into resume state. A write
 * only becomes visible to TransferJob after data flush, journal flush, and the
 * journal's atomic candidate publication all succeed in that order.
 */
export class PersistentTreeOutputSession implements OutputSession {
  readonly identity: OutputSessionIdentity
  readonly format = 'directory' as const
  readonly capabilities: OutputCapabilities

  readonly #tree: PersistentOutputTree
  readonly #journal: OutputCheckpointJournal
  readonly #crashHook: CheckpointCrashHook | undefined
  readonly #active = new Map<string, PersistentFileTransaction>()
  readonly #opening = new Set<string>()
  readonly #maximumOpenFiles: number
  readonly #directoryAdmissions: DirectoryAdmissionLedger
  readonly #checkpointBinding: CheckpointNamespaceBinding
  readonly #checkpointOwner: OutputSessionIdentity & CheckpointNamespaceBinding
  readonly #journalView: PersistentJournalView
  readonly #acquisition: PersistentAcquisitionContext
  readonly #lifecycle: PersistentOutputLifecycle

  private constructor(options: PersistentTreeOutputOptions) {
    this.identity = outputSessionIdentity(options.identity)
    this.#tree = options.tree
    this.#journal = options.journal
    this.#checkpointBinding = durableCheckpointNamespaceIdentity(options.journal.binding)
    if (this.#checkpointBinding.backend !== this.identity.backend) {
      throw new PersistentOutputError(
        'journal-binding',
        'Output journal backend does not match the runtime output backend',
      )
    }
    if (this.#checkpointBinding.transferIntentDigest !==
        options.directoryAdmissionScope.transferIntentDigest) {
      throw new DirectoryAdmissionBindingError(
        'directory admission scope does not match the durable checkpoint namespace',
      )
    }
    this.#directoryAdmissions = new DirectoryAdmissionLedger(options.directoryAdmissionScope)
    this.#checkpointOwner = Object.freeze({
      ...this.identity,
      ...this.#checkpointBinding,
    })
    this.#journalView = new PersistentJournalView(this.#checkpointOwner, this.#journal, this.#tree)
    this.#acquisition = {
      identity: this.identity,
      checkpointOwner: this.#checkpointOwner,
      tree: this.#tree,
      journalView: this.#journalView,
      publish: (record) => this.#publish(record),
      deleteRecord: (record) => this.#deleteRecord(record),
      readCommittedRecord: (record) => this.#readCommittedRecord(record),
      verifyDirectoryIdentity: (record) => this.#verifyDirectoryIdentity(record),
      verifyFileIdentity: (record, exactSize) => this.#verifyFileIdentity(record, exactSize),
    }
    this.#crashHook = options.crashHook
    this.#maximumOpenFiles = options.maximumOpenFiles ?? MAXIMUM_OPEN_OUTPUT_FILES
    if (!Number.isSafeInteger(this.#maximumOpenFiles) || this.#maximumOpenFiles <= 0 ||
        this.#maximumOpenFiles > MAXIMUM_OPEN_OUTPUT_FILES) {
      throw new RangeError(
        `maximum open output files must be between 1 and ${MAXIMUM_OPEN_OUTPUT_FILES}`,
      )
    }
    this.capabilities = outputCapabilities({
      durability: options.durability ?? 'ProcessRestart',
      randomWrite: true,
      fileFailureIsolation: true,
      modificationTime:
        options.tree.setFileModificationTime !== undefined &&
        options.tree.setDirectoryModificationTime !== undefined,
    })
    this.#lifecycle = new PersistentOutputLifecycle(this.capabilities.durability, this.#active, this.#opening)
  }

  static async open(options: PersistentTreeOutputOptions): Promise<PersistentTreeOutputSession> {
    const session = new PersistentTreeOutputSession(options)
    session.#assertCheckpointBinding()
    try {
      await options.tree.authorize()
    } catch (error) {
      throw new PersistentOutputError(
        'authorization',
        'Persistent output access was not authorized',
        error,
      )
    }
    await session.#recoverJournal()
    return session
  }

  admitDirectory(input: OutputDirectoryAdmission, signal: AbortSignal): Promise<DirectoryAdmission> {
    return this.runOperation(async () => {
      this.#lifecycle.requireSessionOpen()
      return this.#directoryAdmissions.admitDirectory(input, signal, async (directory, operationSignal) => {
        // The empty catalog path names the already-authorized picker root; creating
        // a child is only meaningful after an authenticated generation descends it.
        if (directory.path.length > 0) await this.#materializeDirectory(directory, operationSignal)
      })
    })
  }

  async #materializeDirectory(input: OutputDirectoryAdmission, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    this.#lifecycle.requireSessionOpen()
    const directory = snapshotOutputDirectoryAdmission(input)
    const key = directoryKey(directory.path)
    await this.#requireParentDirectories(directory.path)
    signal.throwIfAborted()
    if (await this.#journalView.readRecord(fileKey(directory.path)) !== undefined) {
      throw this.#bindingError('A file journal record occupies an output directory path')
    }
    const existing = await this.#journalView.readRecord(key)
    if (existing !== undefined) {
      if (existing.kind !== 'directory') {
        throw this.#bindingError('A file journal record occupies an output directory path')
      }
      await this.#requireDirectoryIdentity(existing)
      return
    }

    await materializePersistentDirectory(this.#acquisition, directory, signal)
  }

  finalizeDirectory(
    admission: DirectoryAdmission,
    signal: AbortSignal,
  ): Promise<DirectorySettlement> {
    return this.runOperation((operation) => {
      this.#lifecycle.requireSessionOpen()
      return this.#directoryAdmissions.finalizeDirectory(
        admission,
        signal,
        (directory, operationSignal) => this.#finalizeDirectory(directory, operationSignal),
        operation.closing,
      )
    })
  }

  async #finalizeDirectory(input: OutputDirectoryAdmission, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    const directory = snapshotOutputDirectoryAdmission(input)
    // The picker root is caller-owned. Its receipt still seals runtime authority,
    // but catalog metadata must not rewrite the existing container.
    if (directory.path.length === 0) return
    const existing = await this.#journalView.readRecord(directoryKey(directory.path))
    if (existing?.kind !== 'directory') {
      throw new PersistentOutputError(
        'output-state',
        'Output directory must be materialized before finalization',
      )
    }
    await this.#requireDirectoryIdentity(existing)
    signal.throwIfAborted()
    if (existing.createdBySession &&
        directory.modifiedTime !== undefined &&
        this.#tree.setDirectoryModificationTime !== undefined) {
      try {
        await this.#tree.setDirectoryModificationTime(
          directory.path,
          existing.ownedDirectoryIdentity,
          directory.modifiedTime.milliseconds,
        )
      } catch (cause) {
        throw new OutputDirectoryMutationError(
          'Output backend could not apply child directory metadata',
          false,
          { cause },
        )
      }
    }
    await this.#publish(directoryRecord(
      this.#checkpointOwner,
      directory.path,
      existing.ownedDirectoryIdentity,
      existing.createdBySession,
      directory.modifiedTime?.milliseconds,
      true,
      nextGeneration(existing),
    ))
    signal.throwIfAborted()
    const finalized = await this.#journalView.readRecord(directoryKey(directory.path))
    if (finalized?.kind !== 'directory') {
      throw this.#bindingError('Finalized output directory journal record disappeared')
    }
    await this.#requireDirectoryIdentity(finalized)
  }

  beginFile(input: OutputFile, signal: AbortSignal): Promise<PersistentBeginOutputFileResult> {
    return this.runOperation((operation) => {
      const file = operation.acquireFile(input)
      return operation.beginFile(file, signal)
    })
  }

  async #beginFile(
    fileMutation: DirectoryFileMutationLease,
    signal: AbortSignal,
  ): Promise<PersistentBeginOutputFileResult> {
    const file = fileMutation.file
    const key = fileKey(file.path)
    let opening = false
    try {
      signal.throwIfAborted()
      this.#lifecycle.requireSessionOpen()
      if (this.#active.has(key) || this.#opening.has(key)) {
        throw new PersistentOutputError('output-state', 'Output file already has an active transaction')
      }
      if (this.#active.size + this.#opening.size >= this.#maximumOpenFiles) {
        throw new PersistentOutputError(
          'resource-limit',
          'Persistent output has reached its open file transaction limit',
        )
      }
      this.#opening.add(key)
      opening = true
      await this.#requireParentDirectories(file.path)
      signal.throwIfAborted()
      if (await this.#journalView.readRecord(directoryKey(file.path)) !== undefined) {
        throw this.#bindingError('A directory journal record occupies an output file path')
      }
      const reopened = await reopenPersistentFile(this.#acquisition, file, key, signal)
      if (reopened === undefined) signal.throwIfAborted()
      const opened = reopened ?? await createPersistentFile(this.#acquisition, file, signal)
      const transaction = new PersistentFileTransaction(
        this,
        file,
        opened.handle,
        opened.record,
        fileMutation,
      )
      this.#active.set(key, transaction)
      return Object.freeze({
        transaction,
        durableRanges: verifiedFileRanges(this.identity, opened.record),
      })
    } catch (error) {
      fileMutation.release()
      throw error
    } finally {
      if (opening) this.#opening.delete(key)
    }
  }

  completeJob(_outcome: JobOutcome, signal: AbortSignal): Promise<JobSettlement> {
    return this.#lifecycle.complete(signal)
  }

  pauseJob(reason: unknown): Promise<JobSettlement> {
    return this.#lifecycle.pause(reason)
  }

  runOperation<T>(
    operation: (session: PersistentSessionOperation) => Promise<T>,
  ): Promise<T> {
    return this.#lifecycle.run((lease) => {
      const sessionOperation: PersistentSessionOperation = Object.freeze({
        closing: lease.closing,
        acquireFile: (file: OutputFile) => this.#directoryAdmissions.acquireFileMutation(file),
        beginFile: (file: DirectoryFileMutationLease, signal: AbortSignal) =>
          this.#beginFile(file, signal),
        stagedFileFootprint: (path: readonly string[]) => this.#journalView.fileFootprint(path),
      })
      return operation(sessionOperation)
    })
  }

  isFileTransactionActive(transaction: PersistentFileTransaction, file: OutputFile): boolean {
    return this.#active.get(fileKey(file.path)) === transaction
  }

  stagedCatalog(): StagedOutputCatalog {
    return this.#journalView.catalog()
  }

  stagedOutputTotals(): Promise<StagedOutputTotals> {
    return this.runOperation(async () => this.#journalView.totals())
  }

  stagedFileFootprint(path: readonly string[]): Promise<StagedFileFootprint> {
    return this.runOperation(async () => this.#journalView.fileFootprint(path))
  }

  async checkpointFile(
    transaction: PersistentFileTransaction,
    file: OutputFile,
    handle: PersistentTreeFile,
    ranges: ByteRangeSet,
    signal: AbortSignal,
  ): Promise<VerifiedDurableRanges> {
    signal.throwIfAborted()
    this.#requireActive(transaction, file)
    const key = fileKey(file.path)
    await handle.flush()
    signal.throwIfAborted()
    await this.#cut('DataFlushed', key)
    const record = fileRecord(
      this.#checkpointOwner,
      fileOwnership(this.identity, file.path, handle.identity),
      file,
      ranges.ranges,
      false,
      transaction.nextGeneration(),
    )
    await this.#journal.writeCandidate(record)
    signal.throwIfAborted()
    await this.#cut('JournalWritten', key)
    await this.#journal.flushCandidate(key)
    signal.throwIfAborted()
    await this.#cut('JournalFlushed', key)
    await this.#journal.commitCandidate(key)
    signal.throwIfAborted()
    await this.#cut('CheckpointCommitted', key)
    await this.#verifyCommittedFile(record, false)
    signal.throwIfAborted()
    await this.#cut('CheckpointVerified', key)
    return verifiedFileRanges(this.identity, record)
  }

  async commitFile(
    transaction: PersistentFileTransaction,
    file: OutputFile,
    handle: PersistentTreeFile,
    ranges: ByteRangeSet,
    signal: AbortSignal,
  ): Promise<void> {
    signal.throwIfAborted()
    this.#requireActive(transaction, file)
    const wholeFile = byteRange(0n, file.exactSize)
    if (!ranges.covers(wholeFile)) {
      throw new PersistentOutputError(
        'incomplete-file',
        'Output file cannot commit before every byte is durable',
      )
    }
    await handle.close()
    signal.throwIfAborted()
    if (file.modifiedTime !== undefined &&
        this.#tree.setFileModificationTime !== undefined) {
      await this.#tree.setFileModificationTime(
        file.path,
        handle.identity,
        file.modifiedTime.milliseconds,
      )
      signal.throwIfAborted()
    }
    const record = fileRecord(
      this.#checkpointOwner,
      fileOwnership(this.identity, file.path, handle.identity),
      file,
      ranges.ranges,
      true,
      transaction.nextGeneration(),
    )
    await this.#publish(record)
    signal.throwIfAborted()
    await this.#verifyFileIdentity(record, true)
    this.#active.delete(fileKey(file.path))
  }

  async retireFile(
    transaction: PersistentFileTransaction,
    file: OutputFile,
    handle: PersistentTreeFile,
    reason: unknown,
  ): Promise<FileRetirementDisposition> {
    this.#requireActive(transaction, file)
    consumeFileRetirementAuthorization(reason)
    const failures: unknown[] = []
    try {
      await handle.close()
    } catch (error) {
      failures.push(error)
    }
    try {
      const record = await this.#journalView.readRecord(fileKey(file.path))
      if (record?.kind !== 'file') {
        throw new PersistentOutputError(
          'output-state',
          'Authorized file retirement could not revalidate its checkpoint record',
        )
      }
      if (!recordMatchesActiveFile(record, file, handle)) {
        throw new BoundaryFaultError(
          checkpointFault(FaultScope.OutputPause, CheckpointFaultCode.OwnershipMismatch),
          'Authorized file retirement no longer identifies the active owned object',
        )
      }
      if (record.committed) {
        // Publication is irreversible user output. Even typed source retirement
        // may only settle the runtime owner after revalidating that final object.
        await this.#verifyFileIdentity(record, true)
      } else {
        await this.#removeFileRecord(record)
      }
    } catch (error) {
      failures.push(error)
    } finally {
      this.#active.delete(fileKey(file.path))
    }
    if (failures.length > 0) {
      throw new AggregateError(
        failures,
        'Could not isolate failed persistent output file',
        { cause: reason },
      )
    }
    return 'FileIsolated'
  }

  async pauseFile(
    transaction: PersistentFileTransaction,
    file: OutputFile,
    handle: PersistentTreeFile,
    reason: unknown,
  ): Promise<void> {
    this.#requireActive(transaction, file)
    try {
      await handle.close()
    } catch (error) {
      throw new AggregateError(
        [error, reason],
        'Could not pause persistent output file',
        { cause: error },
      )
    } finally {
      this.#active.delete(fileKey(file.path))
    }
  }

  async noteDataWritten(file: OutputFile): Promise<void> {
    await this.#cut('DataWritten', fileKey(file.path))
  }

  async #recoverJournal(): Promise<void> {
    await recoverOutputRecords(this.#checkpointOwner, this.#tree, this.#journal)
  }

  async #publish(record: PersistedOutputRecord): Promise<void> {
    const key = outputRecordKey(record)
    await this.#journal.writeCandidate(record)
    await this.#journal.flushCandidate(key)
    await this.#journal.commitCandidate(key)
    await this.#readCommittedRecord(record)
  }

  async #deleteRecord(record: PersistedOutputRecord): Promise<void> {
    const key = outputRecordKey(record)
    await this.#journal.discardCandidate(key)
    await this.#journal.deleteCommitted(key)
    await this.#tree.forgetIdentity?.(record.kind === 'file'
      ? record.ownedFileIdentity
      : record.ownedDirectoryIdentity)
  }

  async #removeFileRecord(record: PersistedFileRecord): Promise<void> {
    await this.#tree.removeFile(record.canonicalPath, record.ownedFileIdentity)
    await this.#deleteRecord(record)
  }

  async #requireDirectoryIdentity(record: PersistedDirectoryRecord): Promise<void> {
    const persisted = await this.#readCommittedRecord(record)
    try {
      await this.#verifyDirectoryIdentity(persisted)
    } catch (error) {
      throw new BoundaryFaultError(
        checkpointFault(FaultScope.OutputPause, CheckpointFaultCode.OwnershipMismatch),
        'Persisted output directory no longer matches its checkpoint identity',
        { cause: error },
      )
    }
  }

  async #verifyDirectoryIdentity(record: PersistedDirectoryRecord): Promise<void> {
    if (!await this.#tree.validateDirectory(
      record.canonicalPath,
      record.ownedDirectoryIdentity,
    )) {
      throw new PersistentOutputError(
        'output-identity',
        'Persisted directory handle no longer identifies the journal-owned path',
      )
    }
  }

  async #verifyCommittedFile(record: PersistedFileRecord, exactSize: boolean): Promise<void> {
    const reopened = await this.#readCommittedRecord(record)
    await this.#verifyFileIdentity(reopened, exactSize)
  }

  async #verifyFileIdentity(record: PersistedFileRecord, exactSize: boolean): Promise<void> {
    const reopened = await this.#tree.openFile(
      record.canonicalPath,
      record.ownedFileIdentity,
    )
    if (reopened === undefined) {
      throw new PersistentOutputError(
        'output-identity',
        'Persisted output file could not be reopened with its owned identity',
      )
    }
    try {
      const actualSize = await reopened.size()
      const durableEnd = record.durableRanges.at(-1)?.end ?? 0n
      if (actualSize < durableEnd || actualSize > record.exactSize ||
          (exactSize && actualSize !== record.exactSize)) {
        throw new PersistentOutputError(
          'output-identity',
          'Persisted output size changed during checkpoint verification',
        )
      }
    } finally {
      await reopened.close()
    }
  }

  async #readCommittedRecord<T extends PersistedOutputRecord>(record: T): Promise<T> {
    const reopened = await this.#journal.readCommitted(outputRecordKey(record))
    if (reopened === undefined || !sameOutputRecord(reopened, record)) {
      throw this.#bindingError('Atomic journal publication did not reopen as the expected record')
    }
    return reopened as T
  }

  async #requireParentDirectories(path: readonly string[]): Promise<void> {
    for (let length = 1; length < path.length; length += 1) {
      const parent = await this.#journalView.readRecord(directoryKey(path.slice(0, length)))
      if (parent?.kind !== 'directory') {
        throw new PersistentOutputError(
          'output-state',
          'Output parent directory must be materialized before its child',
        )
      }
    }
  }

  #requireActive(transaction: PersistentFileTransaction, file: OutputFile): void {
    this.#lifecycle.requireSessionOpen()
    if (this.#active.get(fileKey(file.path)) !== transaction) {
      throw new PersistentOutputError('output-state', 'Output file transaction is not active')
    }
  }

  #bindingError(message: string, cause?: unknown): PersistentOutputError {
    return new PersistentOutputError('journal-binding', message, cause)
  }

  #assertCheckpointBinding(): void {
    const journalBinding = this.#journal.binding
    const configured = this.#checkpointBinding
    if (journalBinding.backend !== this.identity.backend ||
        journalBinding.transferIntentDigest !== configured.transferIntentDigest ||
        journalBinding.rootIdentity !== configured.rootIdentity) {
      throw this.#bindingError('Output journal binding does not match the confirmed output target')
    }
  }

  async #cut(phase: Parameters<CheckpointCrashHook>[0], key: string): Promise<void> {
    await this.#crashHook?.(phase, key)
  }
}

function recordMatchesActiveFile(
  record: PersistedFileRecord,
  file: OutputFile,
  handle: PersistentTreeFile,
): boolean {
  return record.ownedFileIdentity === handle.identity &&
    record.exactSize === file.exactSize &&
    sameOutputSource(record.source, file.source) &&
    record.canonicalPath.length === file.path.length &&
    record.canonicalPath.every((segment, index) => segment === file.path[index])
}
