import { DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME, openIndexedDbCheckpointDatabase } from '../../browser/indexeddb-database'
import type { ReceiveOperationTransition } from '../../workspace/repository'
import type {
  DirectZipBootstrapCandidateBatchV1,
  DirectZipBootstrapCandidateScanV1,
  DirectZipBootstrapCommitV1,
  DirectZipCandidatePromotionV1,
  DirectZipCandidateRetirementV1,
  DirectZipCandidateV1,
  DirectZipImmutablePageV1,
  DirectZipJournalFenceV1,
  DirectZipJournalTrace,
  DirectZipPageBatchV1,
  DirectZipPageScanV1,
  DirectZipRecoveryLifecycleCommitV1,
  DirectZipStateRowV1,
} from './model'
import type { DirectZipBootstrapCandidateCutV1, DirectZipBootstrapLeaseReplacementV1, DirectZipJournalRepository, DirectZipOrphanCollectionV1 } from './repository'
import { IndexedDbDirectZipBootstrapTransactions } from './indexeddb/bootstrap-transactions'
import { IndexedDbDirectZipCheckpointTransactions } from './indexeddb/checkpoint-transactions'
import { IndexedDbDirectZipJournalStorage } from './indexeddb/storage'

export { DirectZipJournalConcurrencyError } from './indexeddb/authority'

export class IndexedDbDirectZipJournalRepository implements DirectZipJournalRepository {
  readonly #storage: IndexedDbDirectZipJournalStorage
  readonly #bootstrap: IndexedDbDirectZipBootstrapTransactions
  readonly #checkpoints: IndexedDbDirectZipCheckpointTransactions

  private constructor(database: IDBDatabase, trace?: DirectZipJournalTrace) {
    this.#storage = new IndexedDbDirectZipJournalStorage(database, trace)
    this.#bootstrap = new IndexedDbDirectZipBootstrapTransactions(this.#storage)
    this.#checkpoints = new IndexedDbDirectZipCheckpointTransactions(this.#storage)
  }

  static async open(input: { readonly databaseName?: string; readonly trace?: DirectZipJournalTrace } = {}): Promise<IndexedDbDirectZipJournalRepository> {
    return new IndexedDbDirectZipJournalRepository(
      await openIndexedDbCheckpointDatabase(input.databaseName ?? DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME),
      input.trace,
    )
  }

  readState(operationId: string): Promise<DirectZipStateRowV1 | undefined> { return this.#storage.readState(operationId) }
  readCandidate(operationId: string, candidateId: string): Promise<DirectZipCandidateV1 | undefined> { return this.#storage.readCandidate(operationId, candidateId) }
  readOperationCandidate(operationId: string): Promise<DirectZipCandidateV1 | undefined> { return this.#storage.readOperationCandidate(operationId) }
  commitLeaseAcquisition(transition: ReceiveOperationTransition): Promise<void> { return this.#bootstrap.commitLeaseAcquisition(transition) }
  createBootstrapCandidate(cut: DirectZipBootstrapCandidateCutV1): Promise<void> { return this.#bootstrap.createBootstrapCandidate(cut) }
  replaceBootstrapLease(cut: DirectZipBootstrapLeaseReplacementV1): Promise<void> { return this.#bootstrap.replaceBootstrapLease(cut) }
  stagePage(fence: DirectZipJournalFenceV1, page: DirectZipImmutablePageV1): Promise<void> { return this.#checkpoints.stagePage(fence, page) }
  bindCandidate(fence: DirectZipJournalFenceV1, candidate: Extract<DirectZipCandidateV1, { kind: 'epoch' | 'closing' }>): Promise<void> { return this.#checkpoints.bindCandidate(fence, candidate) }
  commitBootstrap(cut: DirectZipBootstrapCommitV1): Promise<void> { return this.#bootstrap.commitBootstrap(cut) }
  promoteCandidate(cut: DirectZipCandidatePromotionV1): Promise<void> { return this.#checkpoints.promoteCandidate(cut) }
  commitRecoveryLifecycle(cut: DirectZipRecoveryLifecycleCommitV1): Promise<void> { return this.#checkpoints.commitRecoveryLifecycle(cut) }
  retireCandidate(cut: DirectZipCandidateRetirementV1): Promise<void> { return this.#checkpoints.retireCandidate(cut) }
  readPageBatch(scan: DirectZipPageScanV1): Promise<DirectZipPageBatchV1> { return this.#storage.readPageBatch(scan) }
  streamPages(scan: Omit<DirectZipPageScanV1, 'afterPageOrdinal'>): AsyncIterable<DirectZipImmutablePageV1> { return this.#storage.streamPages(scan) }
  readBootstrapCandidateBatch(scan: DirectZipBootstrapCandidateScanV1 = {}): Promise<DirectZipBootstrapCandidateBatchV1> { return this.#storage.readBootstrapCandidateBatch(scan) }
  streamBootstrapCandidates(): AsyncIterable<Extract<DirectZipCandidateV1, { kind: 'bootstrap' }>> { return this.#storage.streamBootstrapCandidates() }
  collectOrphanPages(fence: DirectZipJournalFenceV1): Promise<DirectZipOrphanCollectionV1> { return this.#storage.collectOrphanPages(fence) }
  close(): void { this.#storage.close() }
}
