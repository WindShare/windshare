import type {
  FSAFileCheckpointRepositoryFactory,
  FSASemanticOutputRepository,
} from '../../src/output/file-system-access/checkpoint-repository'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  deriveCheckpointLineageID,
  fileCheckpointIsComplete,
  fileCheckpointIsFinalized,
  sameCheckpointLineageSpec,
  validateFileCheckpoint,
  validateFileCheckpointTransition,
  type FileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import {
  RECOVERY_METADATA_RETIREMENT_BATCH_LIMIT,
  finalFileCheckpointProof,
  resolveCheckpointLineage,
  type CheckpointLineageDecision,
  type CheckpointLineageLookupRequest,
  type CheckpointNamespaceBinding,
  type FileCheckpointPage,
  type FileCheckpointScan,
  type FinalFileCheckpointCommit,
  type FinalFileCheckpointCommitReceipt,
  type FinalFileCheckpointProof,
  type InitialCheckpointCASResult,
  type PersistentHandleRecord,
  type RecoveryMetadataRetirementResult,
} from '../../src/output/persistence/journal'
import type { FileCheckpointCandidateObservation } from '../../src/output/persistent-tree/recovery'

interface MemoryCheckpointStore {
  readonly candidates: Map<string, FileCheckpointV2>
  readonly committed: Map<string, FileCheckpointV2>
  readonly handles: Map<string, PersistentHandleRecord>
}

export function memoryCheckpointFactory(
  onRetire?: () => void,
  beforeRetire?: () => void,
): FSAFileCheckpointRepositoryFactory {
  const stores = new Map<string, MemoryCheckpointStore>()
  return async (binding) => {
    let store = stores.get(binding.operationId)
    if (store === undefined) {
      store = {
        candidates: new Map(),
        committed: new Map(),
        handles: new Map(),
      }
      stores.set(binding.operationId, store)
    }
    return new MemoryCheckpointRepository(binding, store, onRetire, beforeRetire)
  }
}

class MemoryCheckpointRepository implements FSASemanticOutputRepository {
  readonly binding: CheckpointNamespaceBinding
  readonly #store: MemoryCheckpointStore
  readonly #onRetire: (() => void) | undefined
  readonly #beforeRetire: (() => void) | undefined

  constructor(
    binding: CheckpointNamespaceBinding,
    store: MemoryCheckpointStore,
    onRetire?: () => void,
    beforeRetire?: () => void,
  ) {
    this.binding = binding
    this.#store = store
    this.#onRetire = onRetire
    this.#beforeRetire = beforeRetire
  }

  async lookupLineage(
    request: CheckpointLineageLookupRequest,
  ): Promise<CheckpointLineageDecision> {
    return this.#lineageDecision(request)
  }

  classifyLineages(
    requests: readonly CheckpointLineageLookupRequest[],
  ): Promise<readonly CheckpointLineageDecision[]> {
    return Promise.all(requests.map(request => this.lookupLineage(request)))
  }

  installInitialClaims(
    candidates: readonly FileCheckpointV2[],
  ): Promise<readonly InitialCheckpointCASResult[]> {
    return Promise.all(candidates.map(candidate => this.createInitialCheckpoint(candidate)))
  }

  async createInitialCheckpoint(
    candidate: FileCheckpointV2,
  ): Promise<InitialCheckpointCASResult> {
    validateFileCheckpoint(candidate)
    const lineageId = deriveCheckpointLineageID(candidate)
    const decision = this.#lineageDecision({
      lineageId,
      fileId: candidate.fileId,
      canonicalPath: candidate.canonicalPath,
      fileRevision: candidate.fileRevision,
      exactSize: candidate.exactSize,
    })
    if (decision.kind !== 'absent') return decision
    this.#store.candidates.set(candidate.recordId, candidate)
    return Object.freeze({ kind: 'installed', lineageId, record: candidate })
  }

  async resolveCandidate(
    candidate: FileCheckpointV2,
    observation: Exclude<FileCheckpointCandidateObservation, { kind: 'ownership-unknown' }>,
  ): Promise<void> {
    if (this.#store.candidates.get(candidate.recordId)?.checksum !== candidate.checksum) {
      throw new DOMException('candidate changed during recovery', 'InvalidStateError')
    }
    const resolved = observation.kind === 'verified'
      ? observation.committed
      : observation.checkpoint
    this.#store.committed.set(candidate.recordId, resolved)
    this.#store.candidates.delete(candidate.recordId)
  }

  #lineageDecision(request: CheckpointLineageLookupRequest): CheckpointLineageDecision {
    const spec = Object.freeze({
      ...this.binding,
      fileId: request.fileId,
      canonicalPath: request.canonicalPath,
    })
    if (deriveCheckpointLineageID(spec) !== request.lineageId) {
      throw new TypeError('lineage lookup ID does not match its coordinates')
    }
    const physical = new Map(this.#store.committed)
    for (const [recordId, candidate] of this.#store.candidates) {
      if (!physical.has(recordId)) physical.set(recordId, candidate)
    }
    const records = [...physical.values()].filter(record =>
      sameCheckpointLineageSpec(record, spec))
    return resolveCheckpointLineage(request, records)
  }

  async stageCheckpointUpdate(
    previous: FileCheckpointV2,
    candidate: FileCheckpointV2,
  ): Promise<void> {
    validateFileCheckpointTransition(previous, candidate)
    if (previous.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE ||
        candidate.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE ||
        this.#store.committed.get(previous.recordId)?.checksum !== previous.checksum) {
      throw new DOMException('committed checkpoint predecessor missing', 'InvalidStateError')
    }
    const current = this.#store.candidates.get(candidate.recordId)
    if (current !== undefined && current.checksum !== candidate.checksum) {
      throw new DOMException('checkpoint candidate changed', 'InvalidStateError')
    }
    this.#store.candidates.set(candidate.recordId, candidate)
  }

  async commitCheckpointCandidate(
    candidate: FileCheckpointV2,
    committed: FileCheckpointV2,
  ): Promise<void> {
    validateFileCheckpointTransition(candidate, committed)
    if (candidate.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE ||
        committed.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE) {
      throw new TypeError('memory repository commits only verified checkpoints')
    }
    const currentCandidate = this.#store.candidates.get(candidate.recordId)
    const previous = this.#store.committed.get(candidate.recordId)
    if (currentCandidate === undefined && previous?.checksum === committed.checksum) return
    if (currentCandidate?.checksum !== candidate.checksum) {
      throw new DOMException('checkpoint candidate missing', 'InvalidStateError')
    }
    if (previous !== undefined) validateFileCheckpointTransition(previous, committed)
    this.#store.committed.set(committed.recordId, committed)
    this.#store.candidates.delete(committed.recordId)
  }

  async commitCreatedFile(
    input: Parameters<FSASemanticOutputRepository['commitCreatedFile']>[0],
  ): Promise<void> {
    validateFileCheckpointTransition(input.candidate, input.committed)
    if (this.#store.candidates.get(input.candidate.recordId)?.checksum !== input.candidate.checksum) {
      throw new DOMException('created-file candidate missing', 'InvalidStateError')
    }
    this.#store.handles.set(input.handle.id, input.handle)
    this.#store.committed.set(input.committed.recordId, input.committed)
    this.#store.candidates.delete(input.candidate.recordId)
  }

  async commitDurableCut(previous: FileCheckpointV2, durable: FileCheckpointV2): Promise<void> {
    const current = this.#store.committed.get(previous.recordId)
    if (previous.fileRevision !== durable.fileRevision &&
        current !== undefined && fileCheckpointIsFinalized(current)) {
      throw new DOMException('metadata reuse cannot replace a historical delivery', 'InvalidStateError')
    }
    this.#replaceCommitted(previous, durable)
  }

  async resumePausedCheckpoint(paused: FileCheckpointV2, active: FileCheckpointV2): Promise<void> {
    this.#replaceCommitted(paused, active)
  }

  async restartOwnedFile(
    input: Parameters<FSASemanticOutputRepository['restartOwnedFile']>[0],
  ) {
    const current = this.#store.committed.get(input.previous.recordId)
    if (current !== undefined && fileCheckpointIsFinalized(current)) {
      throw new DOMException('owned-file restart conflicts with final authority', 'InvalidStateError')
    }
    if (current?.checksum === input.reset.checksum) return 'idempotent' as const
    if (current?.checksum !== input.previous.checksum ||
        this.#store.handles.get(input.expectedHandle.id)?.ownedObjectId !==
          input.expectedHandle.ownedObjectId) {
      throw new DOMException('owned-file restart authority changed', 'InvalidStateError')
    }
    validateFileCheckpoint(input.reset)
    this.#store.committed.set(input.reset.recordId, input.reset)
    return 'restart' as const
  }

  async commitFinalFile(
    input: FinalFileCheckpointCommit,
  ): Promise<FinalFileCheckpointCommitReceipt> {
    const current = this.#store.committed.get(input.expectedCommittedCheckpoint.recordId)
    const final = input.finalCheckpoint
    const idempotent = current?.checksum === final.checksum
    if (!idempotent) {
      if (current?.checksum !== input.expectedCommittedCheckpoint.checksum) {
        throw new DOMException('final checkpoint predecessor changed', 'InvalidStateError')
      }
      this.#store.committed.set(final.recordId, final)
    }
    return Object.freeze({
      classification: idempotent ? 'idempotent' as const : 'insert' as const,
      finalCheckpoint: final,
    })
  }

  async readCommitted(recordId: string): Promise<FileCheckpointV2 | undefined> {
    return this.#store.committed.get(recordId)
  }

  async scanCommitted(scan: FileCheckpointScan): Promise<FileCheckpointPage> {
    return scanRecords(this.#store.committed, scan)
  }

  async scanCandidates(scan: FileCheckpointScan): Promise<FileCheckpointPage> {
    return scanRecords(this.#store.candidates, scan)
  }

  async finalCheckpointProof(
    recordId: string,
    generation: bigint,
  ): Promise<FinalFileCheckpointProof> {
    const record = this.#store.committed.get(recordId)
    if (record === undefined || record.checkpointGeneration !== generation ||
        !fileCheckpointIsComplete(record)) {
      throw new DOMException('final checkpoint missing', 'NotFoundError')
    }
    return finalFileCheckpointProof(record)
  }

  async countCheckpointCandidates(): Promise<bigint> {
    return BigInt(this.#store.candidates.size)
  }

  async retireRecoveryMetadataBatch(
    limit: number = RECOVERY_METADATA_RETIREMENT_BATCH_LIMIT,
  ): Promise<RecoveryMetadataRetirementResult> {
    this.#beforeRetire?.()
    let remaining: number = limit
    let deletedRows = 0
    for (const records of [
      this.#store.candidates,
      this.#store.committed,
      this.#store.handles,
    ]) {
      for (const key of records.keys()) {
        if (remaining === 0) break
        records.delete(key)
        remaining -= 1
        deletedRows += 1
      }
      if (remaining === 0) break
    }
    const more = [
      this.#store.candidates,
      this.#store.committed,
      this.#store.handles,
    ].some(records => records.size !== 0)
    if (!more) this.#onRetire?.()
    return Object.freeze({ deletedRows, state: more ? 'more' as const : 'complete' as const })
  }

  async retireOperation(): Promise<void> {
    this.#store.candidates.clear()
    this.#store.committed.clear()
    this.#store.handles.clear()
    this.#onRetire?.()
  }

  async putHandle(record: PersistentHandleRecord): Promise<void> {
    this.#store.handles.set(record.id, record)
  }

  async readHandle(id: string): Promise<PersistentHandleRecord | undefined> {
    return this.#store.handles.get(id)
  }

  async listHandles(): Promise<readonly PersistentHandleRecord[]> {
    return [...this.#store.handles.values()].sort((left, right) => left.id.localeCompare(right.id))
  }

  async deleteHandle(id: string): Promise<void> {
    this.#store.handles.delete(id)
  }

  #replaceCommitted(previous: FileCheckpointV2, next: FileCheckpointV2): void {
    validateFileCheckpointTransition(previous, next)
    if (this.#store.committed.get(previous.recordId)?.checksum !== previous.checksum) {
      throw new DOMException('committed checkpoint predecessor changed', 'InvalidStateError')
    }
    this.#store.committed.set(next.recordId, next)
  }

  close(): void {}
}

function scanRecords(
  records: ReadonlyMap<string, FileCheckpointV2>,
  scan: FileCheckpointScan,
): FileCheckpointPage {
  const sorted = [...records.values()]
    .filter((record) => scan.fileId === undefined || record.fileId === scan.fileId)
    .sort((left, right) => {
      if (left.recordId === right.recordId) return 0
      return left.recordId < right.recordId ? -1 : 1
    })
  if (scan.direction === 'descending') sorted.reverse()
  const after = scan.cursor === undefined
    ? sorted
    : sorted.filter((record) => scan.direction === 'ascending'
        ? record.recordId > scan.cursor!
        : record.recordId < scan.cursor!)
  const limit = scan.limit ?? 128
  const page = after.slice(0, limit)
  return Object.freeze({
    records: Object.freeze(page),
    ...(after.length >= limit && page.at(-1) !== undefined
      ? { nextCursor: page.at(-1)!.recordId }
      : {}),
  })
}
