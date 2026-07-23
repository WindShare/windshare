import type { OutputSessionIdentity } from '../../transfer/output-session'
import type {
  OutputCheckpointJournal,
  PersistedOutputRecord,
} from '../persistence/journal'
import {
  type OutputJournalPage,
  type OutputJournalScan,
  outputPathKey,
  outputRecordKey,
  recordBelongsToSession,
  snapshotOutputRecord,
  validateOutputJournalPage,
} from '../persistence/journal'
import type { PersistentOutputTree } from './contracts'
import { PersistentOutputError } from './errors'

export async function recoverOutputRecords(
  identity: OutputSessionIdentity,
  tree: PersistentOutputTree,
  journal: OutputCheckpointJournal,
): Promise<void> {
  await scanJournal(
    identity,
    (scan) => journal.scanCandidates(scan),
    async (candidate) => {
      const key = validatedRecordKey(candidate, identity)
      let committed: PersistedOutputRecord | undefined
      try {
        committed = await journal.readCommitted(key)
      } catch (error) {
        throw bindingError('Committed output journal record could not be validated', error)
      }
      if (committed === undefined) await removeUncommittedCreation(candidate, tree)
      await journal.discardCandidate(key)
    },
  )
  await scanJournal(
    identity,
    (scan) => journal.scanCommitted(scan),
    async (candidate) => {
      const record = validatedRecord(candidate, identity)
      const conflictingKind = record.kind === 'file' ? 'directory' : 'file'
      if (await readRecord(journal, recordKey(conflictingKind, record.canonicalPath)) !== undefined) {
        throw bindingError('Output journal assigns both file and directory kinds to one path')
      }
      for (let length = 1; length < record.canonicalPath.length; length += 1) {
        const parent = await readRecord(
          journal,
          recordKey('directory', record.canonicalPath.slice(0, length)),
        )
        if (parent?.kind !== 'directory') {
          throw bindingError('Output journal contains a child without its owned parent directory')
        }
      }
    },
  )
}

async function scanJournal(
  identity: OutputSessionIdentity,
  scan: (options: OutputJournalScan) => Promise<OutputJournalPage>,
  visit: (record: PersistedOutputRecord) => Promise<void>,
): Promise<void> {
  let cursor: string | undefined
  do {
    let page: OutputJournalPage
    try {
      const options: OutputJournalScan = {
        direction: 'ascending',
        ...(cursor === undefined ? {} : { cursor }),
      }
      page = validateOutputJournalPage(await scan(options), options, identity)
    } catch (error) {
      throw bindingError('Output journal could not be scanned', error)
    }
    for (const record of page.records) await visit(record)
    cursor = page.nextCursor
  } while (cursor !== undefined)
}

function validatedRecord(
  candidate: PersistedOutputRecord,
  identity: OutputSessionIdentity,
): PersistedOutputRecord {
  let record: PersistedOutputRecord
  try {
    record = snapshotOutputRecord(candidate)
  } catch (error) {
    throw bindingError('Output journal contains a corrupt record', error)
  }
  if (!recordBelongsToSession(record, identity)) {
    throw bindingError('Output journal contains a record for another session')
  }
  return record
}

function validatedRecordKey(
  candidate: PersistedOutputRecord,
  identity: OutputSessionIdentity,
): string {
  return outputRecordKey(validatedRecord(candidate, identity))
}

async function removeUncommittedCreation(
  record: PersistedOutputRecord,
  tree: PersistentOutputTree,
): Promise<void> {
  if (record.kind === 'file') {
    await tree.removeFile(record.canonicalPath, record.ownedFileIdentity)
    return
  }
  if (record.createdBySession) {
    await tree.removeDirectory(record.canonicalPath, record.ownedDirectoryIdentity)
  }
  await tree.forgetIdentity?.(record.ownedDirectoryIdentity)
}

async function readRecord(
  journal: OutputCheckpointJournal,
  key: string,
): Promise<PersistedOutputRecord | undefined> {
  try {
    return await journal.readCommitted(key)
  } catch (error) {
    throw bindingError('Output journal record could not be validated', error)
  }
}

function recordKey(kind: PersistedOutputRecord['kind'], path: readonly string[]): string {
  return `${kind}:${outputPathKey(path)}`
}

function bindingError(message: string, cause?: unknown): PersistentOutputError {
  return new PersistentOutputError('journal-binding', message, cause)
}
