import { ByteRangeSet, byteRange } from '../../content/geometry'
import type { JobOutcome } from '../../transfer/outcome'
import {
  type BeginOutputFileResult,
  type FileAbortDisposition,
  type OutputCapabilities,
  type OutputDirectory,
  type OutputFile,
  type OutputSession,
  type OutputSessionIdentity,
  VerifiedDurableRanges,
  MAXIMUM_OPEN_OUTPUT_FILES,
  outputCapabilities,
  outputSessionIdentity,
  snapshotOutputDirectory,
  snapshotOutputFile,
} from '../../transfer/output-session'
import {
  type OutputCheckpointJournal,
  type PersistedDirectoryRecord,
  type PersistedFileRecord,
  type PersistedOutputRecord,
  directoryRecord,
  fileRecord,
  outputRecordKey,
  recordBelongsToSession,
  sameOutputRecord,
  snapshotOutputRecord,
  validateOutputJournalPage,
} from '../persistence/journal'
import type {
  CheckpointCrashHook,
  PersistentOutputTree,
  PersistentTreeFile,
} from './contracts'
import { PersistentOutputError } from './errors'
import { PersistentFileTransaction } from './file-transaction'
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

export interface PersistentTreeOutputOptions {
  readonly identity: OutputSessionIdentity
  readonly tree: PersistentOutputTree
  readonly journal: OutputCheckpointJournal
  readonly durability?: 'ProcessRestart' | 'PowerLoss'
  readonly maximumOpenFiles?: number
  readonly crashHook?: CheckpointCrashHook
}

export interface StagedOutputFile {
  readonly record: PersistedFileRecord
  read(): Promise<Blob>
}

export interface StagedOutputCatalog {
  directories(): AsyncIterable<PersistedDirectoryRecord>
  files(): AsyncIterable<StagedOutputFile>
}

export interface StagedFileFootprint {
  readonly logicalBytes: bigint
  readonly coveredBytes: bigint
}

export interface StagedOutputTotals {
  readonly logicalBytes: bigint
  readonly additionalBytes: bigint
}

type SessionState = 'open' | 'finished' | 'aborted' | 'suspended'

/**
 * The session is the sole authority that turns bytes into resume state. A write
 * only becomes visible to TransferJob after data flush, journal flush, and the
 * journal's atomic candidate publication all succeed in that order.
 */
export class PersistentTreeOutputSession implements OutputSession {
  readonly identity: OutputSessionIdentity
  readonly capabilities: OutputCapabilities

  readonly #tree: PersistentOutputTree
  readonly #journal: OutputCheckpointJournal
  readonly #crashHook: CheckpointCrashHook | undefined
  readonly #active = new Map<string, PersistentFileTransaction>()
  readonly #opening = new Set<string>()
  readonly #maximumOpenFiles: number

  #state: SessionState = 'open'

  private constructor(options: PersistentTreeOutputOptions) {
    this.identity = outputSessionIdentity(options.identity)
    this.#tree = options.tree
    this.#journal = options.journal
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

  async ensureDirectory(input: OutputDirectory): Promise<void> {
    this.#requireOpen()
    const directory = snapshotOutputDirectory(input)
    const key = directoryKey(directory.path)
    await this.#requireParentDirectories(directory.path)
    if (await this.#readRecord(fileKey(directory.path)) !== undefined) {
      throw this.#bindingError('A file journal record occupies an output directory path')
    }
    const existing = await this.#readRecord(key)
    if (existing !== undefined) {
      if (existing.kind !== 'directory') {
        throw this.#bindingError('A file journal record occupies an output directory path')
      }
      await this.#requireDirectoryIdentity(existing)
      return
    }

    const materialized = await this.#tree.ensureDirectory(directory.path)
    const record = directoryRecord(
      this.identity,
      directory.path,
      materialized.identity,
      materialized.created,
      directory.modifiedTimeMilliseconds,
      false,
      1n,
    )
    await this.#publish(record)
    await this.#requireDirectoryIdentity(record)
  }

  async finalizeDirectory(input: OutputDirectory, signal: AbortSignal): Promise<void> {
    this.#requireOpen()
    signal.throwIfAborted()
    const directory = snapshotOutputDirectory(input)
    const existing = await this.#readRecord(directoryKey(directory.path))
    if (existing?.kind !== 'directory') {
      throw new PersistentOutputError(
        'output-state',
        'Output directory must be materialized before finalization',
      )
    }
    await this.#requireDirectoryIdentity(existing)
    signal.throwIfAborted()
    if (existing.createdBySession &&
        directory.modifiedTimeMilliseconds !== undefined &&
        this.#tree.setDirectoryModificationTime !== undefined) {
      await this.#tree.setDirectoryModificationTime(
        directory.path,
        existing.ownedDirectoryIdentity,
        directory.modifiedTimeMilliseconds,
      )
    }
    await this.#publish(directoryRecord(
      this.identity,
      directory.path,
      existing.ownedDirectoryIdentity,
      existing.createdBySession,
      directory.modifiedTimeMilliseconds,
      true,
      nextGeneration(existing),
    ))
    signal.throwIfAborted()
    const finalized = await this.#readRecord(directoryKey(directory.path))
    if (finalized?.kind !== 'directory') {
      throw this.#bindingError('Finalized output directory journal record disappeared')
    }
    await this.#requireDirectoryIdentity(finalized)
  }

  async beginFile(input: OutputFile): Promise<BeginOutputFileResult> {
    this.#requireOpen()
    const file = snapshotOutputFile(input)
    const key = fileKey(file.path)
    await this.#requireParentDirectories(file.path)
    if (await this.#readRecord(directoryKey(file.path)) !== undefined) {
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
      const reopened = await this.#reopenFile(file)
      const opened = reopened ?? await this.#createFile(file)
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
    for await (const record of this.#recordsByKind('file', 'ascending')) {
      try {
        await this.#removeFileRecord(record as PersistedFileRecord)
      } catch (error) {
        failures.push(error)
      }
    }
    // Descending canonical keys place every child after and therefore before its
    // parent during deletion without retaining a share-wide depth-sorted array.
    for await (const record of this.#recordsByKind('directory', 'descending')) {
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
    return Object.freeze({
      directories: () => this.#stagedDirectories(),
      files: () => this.#stagedFiles(),
    })
  }

  async stagedOutputTotals(): Promise<StagedOutputTotals> {
    let logicalBytes = 0n
    let additionalBytes = 0n
    for await (const candidate of this.#recordsByKind('file', 'ascending')) {
      const record = candidate as PersistedFileRecord
      const covered = coveredBytes(record)
      logicalBytes += record.exactSize
      additionalBytes += record.exactSize - covered
    }
    return Object.freeze({ logicalBytes, additionalBytes })
  }

  async stagedFileFootprint(path: readonly string[]): Promise<StagedFileFootprint> {
    const record = await this.#readRecord(fileKey(path))
    if (record === undefined) return Object.freeze({ logicalBytes: 0n, coveredBytes: 0n })
    if (record.kind !== 'file') throw this.#bindingError('A directory occupies the staged file path')
    return Object.freeze({ logicalBytes: record.exactSize, coveredBytes: coveredBytes(record) })
  }

  async checkpointFile(
    transaction: PersistentFileTransaction,
    file: OutputFile,
    handle: PersistentTreeFile,
    ranges: ByteRangeSet,
  ): Promise<VerifiedDurableRanges> {
    this.#requireActive(transaction, file)
    const key = fileKey(file.path)
    await handle.flush()
    await this.#cut('DataFlushed', key)
    const record = fileRecord(
      this.identity,
      fileOwnership(this.identity, file.path, handle.identity),
      file,
      ranges.ranges,
      false,
      transaction.nextGeneration(),
    )
    await this.#journal.writeCandidate(record)
    await this.#cut('JournalWritten', key)
    await this.#journal.flushCandidate(key)
    await this.#cut('JournalFlushed', key)
    await this.#journal.commitCandidate(key)
    await this.#cut('CheckpointCommitted', key)
    await this.#verifyCommittedFile(record, false)
    await this.#cut('CheckpointVerified', key)
    return verifiedFileRanges(record)
  }

  async commitFile(
    transaction: PersistentFileTransaction,
    file: OutputFile,
    handle: PersistentTreeFile,
    ranges: ByteRangeSet,
  ): Promise<void> {
    this.#requireActive(transaction, file)
    const wholeFile = byteRange(0n, file.exactSize)
    if (!ranges.covers(wholeFile)) {
      throw new PersistentOutputError(
        'incomplete-file',
        'Output file cannot commit before every byte is durable',
      )
    }
    await handle.close()
    if (file.modifiedTimeMilliseconds !== undefined &&
        this.#tree.setFileModificationTime !== undefined) {
      await this.#tree.setFileModificationTime(
        file.path,
        handle.identity,
        file.modifiedTimeMilliseconds,
      )
    }
    const record = fileRecord(
      this.identity,
      fileOwnership(this.identity, file.path, handle.identity),
      file,
      ranges.ranges,
      true,
      transaction.nextGeneration(),
    )
    await this.#publish(record)
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
    const record = await this.#readRecord(fileKey(file.path))
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
    await recoverOutputRecords(this.identity, this.#tree, this.#journal)
  }

  async #reopenFile(
    file: OutputFile,
  ): Promise<{ readonly handle: PersistentTreeFile; readonly record: PersistedFileRecord } | undefined> {
    const key = fileKey(file.path)
    const existing = await this.#readRecord(key)
    if (existing === undefined) return undefined
    if (existing.kind !== 'file') {
      throw this.#bindingError('A directory journal record occupies an output file path')
    }
    const persisted = await this.#readCommittedRecord(existing)
    const handle = await this.#tree.openFile(file.path, persisted.ownedFileIdentity)
    if (handle === undefined) {
      await this.#deleteRecord(persisted)
      throw new PersistentOutputError(
        'output-identity',
        'Persisted output handle no longer identifies the journal-owned file',
      )
    }
    if (!sameOutputSource(persisted.source, file.source) || persisted.exactSize !== file.exactSize) {
      await handle.close()
      await this.#removeFileRecord(persisted)
      return undefined
    }
    const actualSize = await handle.size()
    const durableEnd = persisted.durableRanges.at(-1)?.end ?? 0n
    if (actualSize < durableEnd || actualSize > persisted.exactSize) {
      await handle.close()
      await this.#removeFileRecord(persisted)
      return undefined
    }
    return { handle, record: persisted }
  }

  async #createFile(
    file: OutputFile,
  ): Promise<{ readonly handle: PersistentTreeFile; readonly record: PersistedFileRecord }> {
    let handle: PersistentTreeFile
    try {
      handle = await this.#tree.createFileExclusive(file.path)
    } catch (error) {
      throw new PersistentOutputError(
        'exclusive-create',
        'Refusing to overwrite an output file not owned by this journal',
        error,
      )
    }
    const record = fileRecord(
      this.identity,
      fileOwnership(this.identity, file.path, handle.identity),
      file,
      [],
      false,
      1n,
    )
    try {
      await this.#publish(record)
      await this.#verifyFileIdentity(record, false)
      return { handle, record }
    } catch (error) {
      const failures: unknown[] = []
      for (const cleanup of [
        () => handle.close(),
        () => this.#tree.removeFile(file.path, handle.identity),
        () => this.#deleteRecord(record),
      ]) {
        try {
          await cleanup()
        } catch (cleanupError) {
          failures.push(cleanupError)
        }
      }
      if (failures.length > 0) {
        throw new AggregateError(
          [error, ...failures],
          'File creation journal and cleanup failed',
          { cause: error },
        )
      }
      throw error
    }
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
    if (!await this.#tree.validateDirectory(
      persisted.canonicalPath,
      persisted.ownedDirectoryIdentity,
    )) {
      await this.#deleteRecord(persisted)
      throw new PersistentOutputError(
        'output-identity',
        'Persisted directory handle no longer identifies the journal-owned path',
      )
    }
  }

  async #readStagedFile(record: PersistedFileRecord): Promise<Blob> {
    const handle = await this.#tree.openFile(
      record.canonicalPath,
      record.ownedFileIdentity,
    )
    if (handle === undefined) {
      throw new PersistentOutputError('output-identity', 'Staged file identity changed before export')
    }
    try {
      return await handle.read()
    } finally {
      await handle.close()
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
      const parent = await this.#readRecord(directoryKey(path.slice(0, length)))
      if (parent?.kind !== 'directory') {
        throw new PersistentOutputError(
          'output-state',
          'Output parent directory must be materialized before its child',
        )
      }
    }
  }

  async *#recordsByKind(
    kind: PersistedOutputRecord['kind'],
    direction: 'ascending' | 'descending',
  ): AsyncGenerator<PersistedOutputRecord> {
    let cursor: string | undefined
    do {
      const scan = {
        kind,
        direction,
        ...(cursor === undefined ? {} : { cursor }),
      } as const
      const page = validateOutputJournalPage(
        await this.#journal.scanCommitted(scan),
        scan,
        this.identity,
      )
      for (const candidate of page.records) {
        const record = snapshotOutputRecord(candidate)
        if (record.kind !== kind || !recordBelongsToSession(record, this.identity)) {
          throw this.#bindingError('Output journal scan escaped its kind or session boundary')
        }
        yield record
      }
      cursor = page.nextCursor
    } while (cursor !== undefined)
  }

  async *#stagedDirectories(): AsyncGenerator<PersistedDirectoryRecord> {
    for await (const record of this.#recordsByKind('directory', 'ascending')) {
      yield record as PersistedDirectoryRecord
    }
  }

  async *#stagedFiles(): AsyncGenerator<StagedOutputFile> {
    for await (const candidate of this.#recordsByKind('file', 'ascending')) {
      const record = candidate as PersistedFileRecord
      if (!record.committed) continue
      yield Object.freeze({
        record,
        read: () => this.#readStagedFile(record),
      })
    }
  }

  async #readRecord(key: string): Promise<PersistedOutputRecord | undefined> {
    let candidate: PersistedOutputRecord | undefined
    try {
      candidate = await this.#journal.readCommitted(key)
    } catch (error) {
      throw this.#bindingError('Output journal record could not be read', error)
    }
    if (candidate === undefined) return undefined
    const record = snapshotOutputRecord(candidate)
    if (!recordBelongsToSession(record, this.identity) || outputRecordKey(record) !== key) {
      throw this.#bindingError('Output journal lookup returned a different record identity')
    }
    return record
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

  async #cut(phase: Parameters<CheckpointCrashHook>[0], key: string): Promise<void> {
    await this.#crashHook?.(phase, key)
  }
}

function coveredBytes(record: PersistedFileRecord): bigint {
  return record.durableRanges.reduce((total, range) => total + range.end - range.start, 0n)
}
