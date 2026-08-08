import {
  type CheckpointNamespaceBinding,
  type OutputCheckpointJournal,
  type PersistedDirectoryRecord,
  type PersistedFileRecord,
  type PersistedOutputRecord,
  outputRecordKey,
  recordBelongsToCheckpointNamespace,
  snapshotOutputRecord,
  validateOutputJournalPage,
} from '../persistence/journal'
import type { PersistentOutputTree } from './contracts'
import { PersistentOutputError } from './errors'
import { fileKey } from './record-identity'

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

type StagedOutputTree = Pick<PersistentOutputTree, 'openFile'>

/**
 * Read-only projection over one confirmed checkpoint namespace. Keeping scan
 * validation here prevents export/quota readers from acquiring mutation or
 * cleanup authority over the persistent output session.
 */
export class PersistentJournalView {
  readonly #binding: CheckpointNamespaceBinding
  readonly #journal: OutputCheckpointJournal
  readonly #tree: StagedOutputTree

  constructor(binding: CheckpointNamespaceBinding, journal: OutputCheckpointJournal, tree: StagedOutputTree) {
    this.#binding = binding
    this.#journal = journal
    this.#tree = tree
  }

  catalog(): StagedOutputCatalog {
    return Object.freeze({
      directories: () => this.#stagedDirectories(),
      files: () => this.#stagedFiles(),
    })
  }

  async totals(): Promise<StagedOutputTotals> {
    let logicalBytes = 0n
    let additionalBytes = 0n
    for await (const candidate of this.recordsByKind('file', 'ascending')) {
      const record = candidate as PersistedFileRecord
      const covered = coveredBytes(record)
      logicalBytes += record.exactSize
      additionalBytes += record.exactSize - covered
    }
    return Object.freeze({ logicalBytes, additionalBytes })
  }

  async fileFootprint(path: readonly string[]): Promise<StagedFileFootprint> {
    const record = await this.readRecord(fileKey(path))
    if (record === undefined) return Object.freeze({ logicalBytes: 0n, coveredBytes: 0n })
    if (record.kind !== 'file') throw this.#bindingError('A directory occupies the staged file path')
    return Object.freeze({ logicalBytes: record.exactSize, coveredBytes: coveredBytes(record) })
  }

  async *recordsByKind(
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
        this.#binding,
      )
      for (const candidate of page.records) {
        const record = snapshotOutputRecord(candidate)
        if (record.kind !== kind ||
            !recordBelongsToCheckpointNamespace(record, this.#binding)) {
          throw this.#bindingError('Output journal scan escaped its kind or session boundary')
        }
        yield record
      }
      cursor = page.nextCursor
    } while (cursor !== undefined)
  }

  async readRecord(key: string): Promise<PersistedOutputRecord | undefined> {
    let candidate: PersistedOutputRecord | undefined
    try {
      candidate = await this.#journal.readCommitted(key)
    } catch (error) {
      throw this.#bindingError('Output journal record could not be read', error)
    }
    if (candidate === undefined) return undefined
    const record = snapshotOutputRecord(candidate)
    if (!recordBelongsToCheckpointNamespace(record, this.#binding) ||
        outputRecordKey(record) !== key) {
      throw this.#bindingError('Output journal lookup returned a different record identity')
    }
    return record
  }

  async *#stagedDirectories(): AsyncGenerator<PersistedDirectoryRecord> {
    for await (const record of this.recordsByKind('directory', 'ascending')) {
      yield record as PersistedDirectoryRecord
    }
  }

  async *#stagedFiles(): AsyncGenerator<StagedOutputFile> {
    for await (const candidate of this.recordsByKind('file', 'ascending')) {
      const record = candidate as PersistedFileRecord
      if (!record.committed) continue
      yield Object.freeze({
        record,
        read: () => this.#readStagedFile(record),
      })
    }
  }

  async #readStagedFile(record: PersistedFileRecord): Promise<Blob> {
    const handle = await this.#tree.openFile(record.canonicalPath, record.ownedFileIdentity)
    if (handle === undefined) {
      throw new PersistentOutputError('output-identity', 'Staged file identity changed before export')
    }
    try {
      return await handle.read()
    } finally {
      await handle.close()
    }
  }

  #bindingError(message: string, cause?: unknown): PersistentOutputError {
    return new PersistentOutputError('journal-binding', message, cause)
  }
}

function coveredBytes(record: PersistedFileRecord): bigint {
  return record.durableRanges.reduce((total, range) => total + range.end - range.start, 0n)
}
