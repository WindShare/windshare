import {
  OutputDirectoryMutationError,
  OutputSessionCompromisedError,
  type OutputDirectoryAdmission,
  type OutputFile,
  type OutputSessionIdentity,
} from '../../transfer/output-session'
import type {
  CheckpointNamespaceBinding,
  PersistedDirectoryRecord,
  PersistedFileRecord,
  PersistedOutputRecord,
} from '../persistence/journal'
import { directoryRecord, fileRecord } from '../persistence/journal'
import type { PersistentOutputTree, PersistentTreeFile } from './contracts'
import { PersistentOutputError } from './errors'
import type { PersistentJournalView } from './journal-view'
import { fileOwnership, sameOutputSource } from './record-identity'

type CheckpointOwner = OutputSessionIdentity & CheckpointNamespaceBinding

export interface PersistentAcquisitionContext {
  readonly identity: OutputSessionIdentity
  readonly checkpointOwner: CheckpointOwner
  readonly tree: PersistentOutputTree
  readonly journalView: PersistentJournalView
  readonly publish: (record: PersistedOutputRecord) => Promise<void>
  readonly deleteRecord: (record: PersistedOutputRecord) => Promise<void>
  readonly removeFileRecord: (record: PersistedFileRecord) => Promise<void>
  readonly readCommittedRecord: <T extends PersistedOutputRecord>(record: T) => Promise<T>
  readonly verifyDirectoryIdentity: (record: PersistedDirectoryRecord) => Promise<void>
  readonly verifyFileIdentity: (record: PersistedFileRecord, exactSize: boolean) => Promise<void>
}

export interface AcquiredPersistentFile {
  readonly handle: PersistentTreeFile
  readonly record: PersistedFileRecord
}

/**
 * Acquisition is a transaction boundary: once a physical handle exists, every
 * failure either returns an owned handle or proves cleanup before returning.
 */
export async function materializePersistentDirectory(
  context: PersistentAcquisitionContext,
  directory: OutputDirectoryAdmission,
  signal: AbortSignal,
): Promise<void> {
  const materialized = await context.tree.ensureDirectory(directory.path)
  const record = directoryRecord(
    context.checkpointOwner,
    directory.path,
    materialized.identity,
    materialized.created,
    directory.modifiedTime?.milliseconds,
    false,
    1n,
  )
  try {
    signal.throwIfAborted()
    await context.publish(record)
    signal.throwIfAborted()
    await context.verifyDirectoryIdentity(record)
  } catch (error) {
    const cleanupFailures: unknown[] = []
    let removalConfirmed = !materialized.created
    if (materialized.created) {
      try {
        await context.tree.removeDirectory(directory.path, materialized.identity)
        removalConfirmed = true
      } catch (cleanupError) {
        cleanupFailures.push(cleanupError)
      }
    }
    if (removalConfirmed) {
      try {
        await context.deleteRecord(record)
      } catch (cleanupError) {
        cleanupFailures.push(cleanupError)
      }
    }
    if (cleanupFailures.length > 0) {
      throw new OutputDirectoryMutationError(
        'Output directory acquisition could not establish cleanup ownership',
        true,
        { cause: new AggregateError([error, ...cleanupFailures], 'Directory materialization cleanup failed') },
      )
    }
    throw error
  }
}

export async function reopenPersistentFile(
  context: PersistentAcquisitionContext,
  file: OutputFile,
  key: string,
  signal: AbortSignal,
): Promise<AcquiredPersistentFile | undefined> {
  const existing = await context.journalView.readRecord(key)
  if (existing === undefined) return undefined
  if (existing.kind !== 'file') throw bindingError('A directory journal record occupies an output file path')
  const persisted = await context.readCommittedRecord(existing)
  const handle = await context.tree.openFile(file.path, persisted.ownedFileIdentity)
  if (handle === undefined) {
    await context.deleteRecord(persisted)
    throw new PersistentOutputError(
      'output-identity',
      'Persisted output handle no longer identifies the journal-owned file',
    )
  }
  let closed = false
  try {
    signal.throwIfAborted()
    if (!sameOutputSource(persisted.source, file.source) || persisted.exactSize !== file.exactSize) {
      await handle.close()
      closed = true
      await context.removeFileRecord(persisted)
      return undefined
    }
    const actualSize = await handle.size()
    signal.throwIfAborted()
    const durableEnd = persisted.durableRanges.at(-1)?.end ?? 0n
    if (actualSize < durableEnd || actualSize > persisted.exactSize) {
      await handle.close()
      closed = true
      await context.removeFileRecord(persisted)
      return undefined
    }
    return { handle, record: persisted }
  } catch (error) {
    if (!closed) {
      try {
        await handle.close()
      } catch (closeError) {
        throw new OutputSessionCompromisedError('Reopened output file validation and close failed', {
          cause: new AggregateError([error, closeError], 'Reopened output file validation and close failed'),
        })
      }
    }
    throw error
  }
}

export async function createPersistentFile(
  context: PersistentAcquisitionContext,
  file: OutputFile,
  signal: AbortSignal,
): Promise<AcquiredPersistentFile> {
  let handle: PersistentTreeFile
  try {
    handle = await context.tree.createFileExclusive(file.path)
  } catch (error) {
    throw new PersistentOutputError(
      'exclusive-create',
      'Refusing to overwrite an output file not owned by this journal',
      error,
    )
  }
  const record = fileRecord(
    context.checkpointOwner,
    fileOwnership(context.identity, file.path, handle.identity),
    file,
    [],
    false,
    1n,
  )
  try {
    signal.throwIfAborted()
    await context.publish(record)
    signal.throwIfAborted()
    await context.verifyFileIdentity(record, false)
    return { handle, record }
  } catch (error) {
    const failures: unknown[] = []
    try {
      await handle.close()
    } catch (cleanupError) {
      failures.push(cleanupError)
    }
    let removalConfirmed = false
    try {
      await context.tree.removeFile(file.path, handle.identity)
      removalConfirmed = true
    } catch (cleanupError) {
      failures.push(cleanupError)
    }
    if (removalConfirmed) {
      try {
        await context.deleteRecord(record)
      } catch (cleanupError) {
        failures.push(cleanupError)
      }
    }
    if (failures.length > 0) {
      throw new OutputSessionCompromisedError('File creation journal and cleanup failed', {
        cause: new AggregateError([error, ...failures], 'File creation journal and cleanup failed'),
      })
    }
    throw error
  }
}

function bindingError(message: string): PersistentOutputError {
  return new PersistentOutputError('journal-binding', message)
}
