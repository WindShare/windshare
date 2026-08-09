import type {
  NamedContainerEntryReservation,
  ReceiveIntent,
} from '../../transfer/intent'
import { IndexedDbFileCheckpointRepository } from '../browser/indexeddb-repository'
import {
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import type {
  FileCheckpointJournal,
  PersistentHandleInventoryRepository,
} from '../persistence/journal'
import { validateFileCheckpointPage } from '../persistence/journal'
import { durableCheckpointNamespaceIdentity } from '../persistence/namespace'

export interface FSAFileCheckpointRepository
extends FileCheckpointJournal, PersistentHandleInventoryRepository {
  close(): void
}

export type FSAFileCheckpointRepositoryFactory = (
  binding: FileCheckpointJournal['binding'],
) => Promise<FSAFileCheckpointRepository>

export interface FSAFileCheckpointRepositoryOptions {
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
}

export async function openFSAFileCheckpointRepository(
  options: FSAFileCheckpointRepositoryOptions,
  intent: ReceiveIntent,
  reservation: NamedContainerEntryReservation,
): Promise<FSAFileCheckpointRepository> {
  const binding = durableCheckpointNamespaceIdentity({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: reservation.digest,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: reservation.authorityRef,
  })
  if (options.checkpointRepositoryFactory !== undefined) {
    return options.checkpointRepositoryFactory(binding)
  }
  return options.databaseName === undefined
    ? IndexedDbFileCheckpointRepository.open(binding)
    : IndexedDbFileCheckpointRepository.open(binding, options.databaseName)
}

export async function scanAllFSAFileCheckpoints(
  journal: FileCheckpointJournal,
  source: 'committed' | 'candidates',
): Promise<readonly FileCheckpointV2[]> {
  const records: FileCheckpointV2[] = []
  let cursor: string | undefined
  do {
    const scan = {
      direction: 'ascending' as const,
      ...(cursor === undefined ? {} : { cursor }),
    }
    const page = validateFileCheckpointPage(
      source === 'committed'
        ? await journal.scanCommitted(scan)
        : await journal.scanCandidates(scan),
      scan,
      journal.binding,
    )
    records.push(...page.records)
    cursor = page.nextCursor
  } while (cursor !== undefined)
  return Object.freeze(records)
}
