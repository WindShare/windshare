import {
  FSA_RESERVED_ROOT_LAYOUT_VERSION,
  validateReceiveIntent,
  type FSANamedContainerEntryReservation,
  type ReceiveIntent,
} from '../../transfer/intent'
import { IndexedDbFileCheckpointRepository } from '../browser/indexeddb-repository'
import {
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import type {
  FileCheckpointJournal,
  PersistentHandleInventoryRepository,
  SemanticFileCheckpointJournal,
} from '../persistence/journal'
import type { MaterializationLedgerJournal } from '../materialization-ledger/journal'
import { validateFileCheckpointPage } from '../persistence/journal'
import { durableCheckpointNamespaceIdentity } from '../persistence/namespace'
import type { FileCheckpointRecoveryRepository } from '../persistent-tree/recovery'

export interface FSAFileCheckpointRepository
extends FileCheckpointJournal, PersistentHandleInventoryRepository,
  Pick<FileCheckpointRecoveryRepository, 'resolveCandidate'> {
  close(): void
}

export interface FSASemanticOutputRepository extends
  FSAFileCheckpointRepository,
  SemanticFileCheckpointJournal<FileSystemFileHandle>,
  MaterializationLedgerJournal {}

export type FSAFileCheckpointRepositoryFactory = (
  binding: FileCheckpointJournal['binding'],
) => Promise<FSASemanticOutputRepository>

export interface FSAFileCheckpointRepositoryOptions {
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
}

export async function openFSAFileCheckpointRepository(
  options: FSAFileCheckpointRepositoryOptions,
  intent: ReceiveIntent,
  reservation: FSANamedContainerEntryReservation,
): Promise<FSASemanticOutputRepository> {
  const validated = await validateReceiveIntent(intent)
  if (validated.plan.kind !== 'direct-tree' ||
      validated.plan.reservation.kind !== 'named-container-entry' ||
      validated.plan.reservation.authorityKind !== 'fsa-container' ||
      validated.plan.reservation.fsaLayoutVersion !== FSA_RESERVED_ROOT_LAYOUT_VERSION ||
      validated.plan.reservation.digest !== reservation.digest) {
    throw new TypeError('FSA checkpoint repository requires the current reserved-root layout binding')
  }
  const boundReservation = validated.plan.reservation
  const binding = durableCheckpointNamespaceIdentity({
    operationId: validated.operationId,
    receiveIntentDigest: validated.digest,
    materializationBindingDigest: boundReservation.digest,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: boundReservation.authorityRef,
  })
  if (options.checkpointRepositoryFactory !== undefined) {
    return options.checkpointRepositoryFactory(binding)
  }
  return options.databaseName === undefined
    ? IndexedDbFileCheckpointRepository.open(binding)
    : IndexedDbFileCheckpointRepository.open(binding, options.databaseName)
}

export async function openFSASemanticOutputRepository(
  options: Pick<FSAFileCheckpointRepositoryOptions, 'databaseName'>,
  intent: ReceiveIntent,
  reservation: FSANamedContainerEntryReservation,
): Promise<FSASemanticOutputRepository> {
  const repository = await openFSAFileCheckpointRepository(
    options.databaseName === undefined ? {} : { databaseName: options.databaseName },
    intent,
    reservation,
  )
  return repository as FSASemanticOutputRepository
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
