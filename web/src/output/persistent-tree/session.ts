import { ByteRangeSet, byteRange } from '../../content/geometry'
import { DirectoryAdmissionLedger } from '../../transfer/directory-admission-ledger'
import type { JobOutcome } from '../../transfer/outcome'
import {
  type BeginOutputFileResult,
  type DirectoryAdmission,
  type FileAbortDisposition,
  type OutputCapabilities,
  type OutputDirectory,
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
  /** Durable binding supplied by the picker-confirmed TransferIntent. */
  readonly checkpointBinding?: Omit<CheckpointNamespaceBinding, 'backend' | 'outputSessionId'>
  readonly tree: PersistentOutputTree
  readonly journal: OutputCheckpointJournal
  readonly durability?: 'ProcessRestart' | 'PowerLoss'
  readonly maximumOpenFiles?: number
  readonly crashHook?: CheckpointCrashHook
}

type SessionState = 'open' | 'finished' | 'aborted' | 'suspended'

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
  readonly #directoryAdmissions = new DirectoryAdmissionLedger()
  readonly #checkpointBinding: Omit<CheckpointNamespaceBinding, 'backend' | 'outputSessionId'> | undefined
  readonly #checkpointOwner: OutputSessionIdentity & CheckpointNamespaceBinding
  readonly #journalView: PersistentJournalView
  readonly #acquisition: PersistentAcquisitionContext

  #state: SessionState = 'open'

  private constructor(options: PersistentTreeOutputOptions) {
    this.identity = outputSessionIdentity(options.identity)
    this.#tree = options.tree
    this.#journal = options.journal
    this.#checkpointBinding = options.checkpointBinding === undefined
      ? undefined
      : Object.freeze({ ...options.checkpointBinding })
    this.#checkpointOwner = Object.freeze({
      ...this.identity,
      ...(this.#checkpointBinding === undefined ? {} : this.#checkpointBinding),
    })
    this.#journalView = new PersistentJournalView(this.#checkpointOwner, this.#journal, this.#tree)
    this.#acquisition = {
      identity: this.identity,
      checkpointOwner: this.#checkpointOwner,
      tree: this.#tree,
      journalView: this.#journalView,
      publish: (record) => this.#publish(record),
      deleteRecord: (record) => this.#deleteRecord(record),
      removeFileRecord: (record) => this.#removeFileRecord(record),
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
    this.#requireOpen()
    return this.#directoryAdmissions.admitDirectory(input, signal, async (directory, operationSignal) => {
      // The empty catalog path names the already-authorized picker root; creating
      // a child is only meaningful after an authenticated generation descends it.
      if (directory.path.length > 0) await this.#materializeDirectory(directory, operationSignal)
    })
  }

  async #materializeDirectory(input: OutputDirectoryAdmission, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    this.#requireOpen()
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

  async finalizeDirectory(input: OutputDirectory, signal: AbortSignal): Promise<void> {
    this.#requireOpen()
    signal.throwIfAborted()
    const directory = this.#directoryAdmissions.validateDirectoryFinalization(input)
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
      this.#checkpointIdentity(),
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

  async beginFile(input: OutputFile, signal: AbortSignal): Promise<BeginOutputFileResult> {
    signal.throwIfAborted()
    this.#requireOpen()
    const file = this.#directoryAdmissions.validateFileParent(input)
    const key = fileKey(file.path)
    await this.#requireParentDirectories(file.path)
    signal.throwIfAborted()
    if (await this.#journalView.readRecord(directoryKey(file.path)) !== undefined) {
      throw this.#bindingError('A directory journal record occupies an output file path')
    }
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
    try {
      const reopened = await reopenPersistentFile(this.#acquisition, file, key, signal)
      if (reopened === undefined) signal.throwIfAborted()
      const opened = reopened ?? await createPersistentFile(this.#acquisition, file, signal)
      const transaction = new PersistentFileTransaction(
        this,
        file,
        opened.handle,
        opened.record,
      )
      this.#active.set(key, transaction)
      return Object.freeze({
        transaction,
        durableRanges: verifiedFileRanges(opened.record),
      })
    } finally {
      this.#opening.delete(key)
    }
  }

  validateFileParent(input: OutputFile): OutputFile {
    this.#requireOpen()
    return this.#directoryAdmissions.validateFileParent(input)
  }

  async finishJob(outcome: JobOutcome, signal: AbortSignal): Promise<void> {
    this.#requireOpen()
    if (outcome.status === 'Aborted') throw new Error('Cannot finish an aborted output job')
    signal.throwIfAborted()
    if (this.#active.size !== 0 || this.#opening.size !== 0) {
      throw new PersistentOutputError(
        'output-state',
        'Cannot finish output while file transactions are active',
      )
    }
    signal.throwIfAborted()
    this.#state = 'finished'
  }

  async abortJob(): Promise<void> {
    if (this.#state === 'aborted') return
    if (this.#state === 'finished') return
    const failures: unknown[] = []
    const active = [...this.#active.values()]
    for (const transaction of active) {
      try {
        await transaction.abort()
      } catch (error) {
        failures.push(error)
      }
    }
    for await (const record of this.#journalView.recordsByKind('file', 'ascending')) {
      try {
        await this.#removeFileRecord(record as PersistedFileRecord)
      } catch (error) {
        failures.push(error)
      }
    }
    // Descending canonical keys place every child after and therefore before its
    // parent during deletion without retaining a share-wide depth-sorted array.
    for await (const record of this.#journalView.recordsByKind('directory', 'descending')) {
      const directory = record as PersistedDirectoryRecord
      try {
        if (directory.createdBySession) {
          await this.#tree.removeDirectory(
            directory.canonicalPath,
            directory.ownedDirectoryIdentity,
          )
        }
        await this.#deleteRecord(directory)
      } catch (error) {
        failures.push(error)
      }
    }
    this.#state = 'aborted'
    if (failures.length > 0) {
      throw new AggregateError(failures, 'Persistent output cleanup failed')
    }
  }

  async suspendJob(): Promise<void> {
    if (this.#state !== 'open') return
    const failures: unknown[] = []
    for (const transaction of [...this.#active.values()]) {
      try {
        await transaction.suspend()
      } catch (error) {
        failures.push(error)
      }
    }
    this.#state = 'suspended'
    if (failures.length > 0) {
      throw new AggregateError(failures, 'Persistent output suspension failed')
    }
  }

  stagedCatalog(): StagedOutputCatalog {
    return this.#journalView.catalog()
  }

  stagedOutputTotals(): Promise<StagedOutputTotals> {
    return this.#journalView.totals()
  }

  stagedFileFootprint(path: readonly string[]): Promise<StagedFileFootprint> {
    return this.#journalView.fileFootprint(path)
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
      this.#checkpointIdentity(),
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
    return verifiedFileRanges(record)
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
      this.#checkpointIdentity(),
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

  async abortFile(
    transaction: PersistentFileTransaction,
    file: OutputFile,
    handle: PersistentTreeFile,
  ): Promise<FileAbortDisposition> {
    this.#requireActive(transaction, file)
    const failures: unknown[] = []
    try {
      await handle.close()
    } catch (error) {
      failures.push(error)
    }
    const record = await this.#journalView.readRecord(fileKey(file.path))
    try {
      if (record?.kind === 'file') {
        await this.#removeFileRecord(record)
      } else {
        await this.#tree.removeFile(file.path, handle.identity)
      }
    } catch (error) {
      failures.push(error)
    } finally {
      this.#active.delete(fileKey(file.path))
    }
    if (failures.length > 0) {
      throw new AggregateError(failures, 'Could not isolate failed persistent output file')
    }
    return 'FileIsolated'
  }

  async suspendFile(
    transaction: PersistentFileTransaction,
    file: OutputFile,
    handle: PersistentTreeFile,
  ): Promise<void> {
    this.#requireActive(transaction, file)
    try {
      await handle.close()
    } finally {
      this.#active.delete(fileKey(file.path))
    }
  }

  async noteDataWritten(file: OutputFile): Promise<void> {
    await this.#cut('DataWritten', fileKey(file.path))
  }

  async #recoverJournal(): Promise<void> {
    await recoverOutputRecords(this.#checkpointIdentity(), this.#tree, this.#journal)
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
      await this.#deleteRecord(persisted)
      throw error
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
    this.#requireOpen()
    if (this.#active.get(fileKey(file.path)) !== transaction) {
      throw new PersistentOutputError('output-state', 'Output file transaction is not active')
    }
  }

  #requireOpen(): void {
    if (this.#state !== 'open') {
      throw new PersistentOutputError('output-state', 'Persistent output session is not open')
    }
  }

  #bindingError(message: string, cause?: unknown): PersistentOutputError {
    return new PersistentOutputError('journal-binding', message, cause)
  }

  #checkpointIdentity(): OutputSessionIdentity & CheckpointNamespaceBinding {
    return this.#checkpointOwner
  }

  #assertCheckpointBinding(): void {
    const journalBinding = this.#journal.binding
    const configured = this.#checkpointBinding
    if (configured === undefined || journalBinding === undefined) return
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
