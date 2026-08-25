import type {
  FSAFileCheckpointRepositoryFactory,
  FSASemanticOutputRepository,
} from '../../src/output/file-system-access/checkpoint-repository'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  classifyCheckpointLineage,
  deriveCheckpointLineageID,
  fileCheckpointIsComplete,
  sameCheckpointLineageSpec,
  validateFileCheckpoint,
  validateFileCheckpointTransition,
  type FileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import {
  finalFileCheckpointProof,
  type CheckpointLineageDecision,
  type CheckpointLineageLookupRequest,
  type CheckpointNamespaceBinding,
  type FileCheckpointPage,
  type FileCheckpointScan,
  type FinalFileCheckpointProof,
  type InitialCheckpointCASResult,
  type PersistentHandleRecord,
} from '../../src/output/persistence/journal'
import {
  compareMaterializationLedgerEntryCursors,
  materializationLedgerEntryCursor,
} from '../../src/output/materialization-ledger/codec'
import { deriveMaterializationLedgerSealId } from '../../src/output/materialization-ledger/evidence'
import {
  MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  type MaterializationLedgerBindingV1,
  type MaterializationLedgerEntryV1,
  type MaterializationLedgerPageSummaryV1,
  type MaterializationLedgerSealV1,
} from '../../src/output/materialization-ledger/model'
import {
  createMaterializationLedgerPageSummary,
  sealMaterializationLedgerPages,
  validateMaterializationLedgerSealPages,
} from '../../src/output/materialization-ledger/page'
import type { FileCheckpointCandidateObservation } from '../../src/output/persistent-tree/recovery'

interface MemoryCheckpointStore {
  readonly candidates: Map<string, FileCheckpointV2>
  readonly committed: Map<string, FileCheckpointV2>
  readonly handles: Map<string, PersistentHandleRecord>
  readonly proofs: Map<string, NonNullable<Awaited<ReturnType<
    FSASemanticOutputRepository['readMaterializationFinalProof']
  >>>>
  readonly ledgerEntries: Map<string, MaterializationLedgerEntryV1>
  readonly ledgerPages: Map<string, MaterializationLedgerPageSummaryV1>
  readonly ledgerSeals: Map<string, MaterializationLedgerSealV1>
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
        proofs: new Map(),
        ledgerEntries: new Map(),
        ledgerPages: new Map(),
        ledgerSeals: new Map(),
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
    const kind = classifyCheckpointLineage(
      { fileRevision: request.fileRevision, exactSize: request.exactSize },
      records.map(record => ({
        fileRevision: record.fileRevision,
        exactSize: record.exactSize,
        ownedObjectId: record.ownedObjectId,
      })),
    )
    if (kind === 'absent') {
      return Object.freeze({ kind, lineageId: request.lineageId })
    }
    if (kind === 'exact') {
      return Object.freeze({ kind, lineageId: request.lineageId, record: records[0]! })
    }
    return Object.freeze({
      kind,
      lineageId: request.lineageId,
      records: Object.freeze(records),
    })
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
        committed.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED) {
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
    this.#replaceCommitted(previous, durable)
  }

  async resumePausedCheckpoint(paused: FileCheckpointV2, active: FileCheckpointV2): Promise<void> {
    this.#replaceCommitted(paused, active)
  }

  async restartOwnedFile(
    input: Parameters<FSASemanticOutputRepository['restartOwnedFile']>[0],
  ) {
    const current = this.#store.committed.get(input.previous.recordId)
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
    input: Parameters<FSASemanticOutputRepository['commitFinalFile']>[0],
  ) {
    const current = this.#store.committed.get(input.expectedCommittedCheckpoint.recordId)
    const final = input.records.finalCheckpoint
    const existingProof = this.#store.proofs.get(input.records.finalProof.proofId)
    const existingEntry = this.#store.ledgerEntries.get(input.records.ledgerEntry.entryId)
    const idempotent = current?.checksum === final.checksum &&
      existingProof?.proofDigest === input.records.finalProof.proofDigest &&
      existingEntry?.entryDigest === input.records.ledgerEntry.entryDigest
    if (!idempotent) {
      if (current?.checksum !== input.expectedCommittedCheckpoint.checksum) {
        throw new DOMException('final checkpoint predecessor changed', 'InvalidStateError')
      }
      this.#store.committed.set(final.recordId, final)
      this.#store.proofs.set(input.records.finalProof.proofId, input.records.finalProof)
      this.#appendLedgerEntry(input.binding, input.records.ledgerEntry)
    }
    return Object.freeze({
      classification: idempotent ? 'idempotent' as const : 'insert' as const,
      finalCheckpoint: final,
      finalProof: input.records.finalProof,
      ledgerEntry: input.records.ledgerEntry,
    })
  }

  async appendDirectoryAdmission(
    binding: MaterializationLedgerBindingV1,
    entry: Parameters<FSASemanticOutputRepository['appendDirectoryAdmission']>[1],
  ) {
    return this.#appendLedgerEntry(binding, entry)
  }

  async appendDirectoryFinalization(
    binding: MaterializationLedgerBindingV1,
    entry: Parameters<FSASemanticOutputRepository['appendDirectoryFinalization']>[1],
  ) {
    return this.#appendLedgerEntry(binding, entry)
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

  async readMaterializationFinalProof(
    binding: MaterializationLedgerBindingV1,
    proofId: string,
  ) {
    this.#requireLedgerBinding(binding)
    return this.#store.proofs.get(proofId)
  }

  async scanMaterializationLedgerEntries(
    binding: MaterializationLedgerBindingV1,
    request: Parameters<FSASemanticOutputRepository['scanMaterializationLedgerEntries']>[1],
  ) {
    this.#requireLedgerBinding(binding)
    const entries = [...this.#store.ledgerEntries.values()].sort((left, right) =>
      compareMaterializationLedgerEntryCursors(
        materializationLedgerEntryCursor(left),
        materializationLedgerEntryCursor(right),
      ))
    const after = request.after === undefined
      ? entries
      : entries.filter(entry => compareMaterializationLedgerEntryCursors(
          materializationLedgerEntryCursor(entry),
          request.after!,
        ) > 0)
    const page = after.slice(0, request.limit)
    const continuation = after.length > page.length && page.at(-1) !== undefined
      ? materializationLedgerEntryCursor(page.at(-1)!)
      : undefined
    return Object.freeze({
      entries: Object.freeze(page),
      ...(continuation === undefined ? {} : { continuation }),
    })
  }

  async countCheckpointCandidates(binding: MaterializationLedgerBindingV1): Promise<bigint> {
    this.#requireLedgerBinding(binding)
    return BigInt(this.#store.candidates.size)
  }

  async persistMaterializationLedgerPage(
    binding: MaterializationLedgerBindingV1,
    page: MaterializationLedgerPageSummaryV1,
  ) {
    this.#requireLedgerBinding(binding)
    const key = `${page.sealId}:${page.pageOrdinal}`
    const existing = this.#store.ledgerPages.get(key)
    if (existing !== undefined && existing.pageDigest !== page.pageDigest) {
      throw new DOMException('materialization ledger page changed', 'ConstraintError')
    }
    this.#store.ledgerPages.set(key, page)
    return existing === undefined ? 'insert' as const : 'idempotent' as const
  }

  async scanMaterializationLedgerPages(
    binding: MaterializationLedgerBindingV1,
    sealId: string,
    afterPageOrdinal?: bigint,
  ) {
    this.#requireLedgerBinding(binding)
    const pages = [...this.#store.ledgerPages.values()]
      .filter(page => page.sealId === sealId &&
        (afterPageOrdinal === undefined || page.pageOrdinal > afterPageOrdinal))
      .sort((left, right) => left.pageOrdinal < right.pageOrdinal ? -1 : 1)
    return Object.freeze({ pages: Object.freeze(pages) })
  }

  async persistMaterializationLedgerSeal(
    binding: MaterializationLedgerBindingV1,
    seal: MaterializationLedgerSealV1,
  ) {
    this.#requireLedgerBinding(binding)
    const existing = this.#store.ledgerSeals.get(seal.sealId)
    if (existing !== undefined && existing.sealDigest !== seal.sealDigest) {
      throw new DOMException('materialization ledger seal changed', 'ConstraintError')
    }
    this.#store.ledgerSeals.set(seal.sealId, seal)
    return existing === undefined ? 'insert' as const : 'idempotent' as const
  }

  async sealMaterializationLedger(
    input: Parameters<FSASemanticOutputRepository['sealMaterializationLedger']>[0],
  ): Promise<MaterializationLedgerSealV1> {
    this.#requireLedgerBinding(input.binding)
    const candidateCheckpointCount = await this.countCheckpointCandidates(input.binding)
    if (candidateCheckpointCount !== 0n) {
      throw new DOMException('materialization ledger cannot seal with checkpoint candidates', 'InvalidStateError')
    }
    const sealId = await deriveMaterializationLedgerSealId(input.binding, input.sealSequence)
    let request: Parameters<FSASemanticOutputRepository['scanMaterializationLedgerEntries']>[1] = {
      limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
    }
    let pageOrdinal = 0n
    let directoryCarry: Parameters<typeof createMaterializationLedgerPageSummary>[0]['directoryCarry']
    for (;;) {
      const page = await this.scanMaterializationLedgerEntries(input.binding, request)
      if (page.entries.length === 0) break
      const built = await createMaterializationLedgerPageSummary({
        binding: input.binding,
        sealId,
        pageOrdinal,
        page,
        request,
        ...(directoryCarry === undefined ? {} : { directoryCarry }),
      })
      await this.persistMaterializationLedgerPage(input.binding, built.summary)
      pageOrdinal += 1n
      directoryCarry = built.directoryCarry
      if (built.continuation === undefined) break
      request = { after: built.continuation, limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT }
    }
    const pages = (await this.scanMaterializationLedgerPages(input.binding, sealId)).pages
    const seal = await sealMaterializationLedgerPages({
      ...input,
      candidateCheckpointCount,
      pages,
    })
    await this.persistMaterializationLedgerSeal(input.binding, seal)
    return validateMaterializationLedgerSealPages({ binding: input.binding, seal, pages })
  }

  async retireMaterializationLedgerBatch(
    binding: MaterializationLedgerBindingV1,
    limit: typeof MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  ) {
    this.#beforeRetire?.()
    this.#requireLedgerBinding(binding)
    let remaining: number = limit
    let deletedRows = 0
    for (const records of [
      this.#store.candidates,
      this.#store.committed,
      this.#store.handles,
      this.#store.proofs,
      this.#store.ledgerEntries,
      this.#store.ledgerPages,
      this.#store.ledgerSeals,
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
      this.#store.proofs,
      this.#store.ledgerEntries,
      this.#store.ledgerPages,
      this.#store.ledgerSeals,
    ].some(records => records.size !== 0)
    if (!more) this.#onRetire?.()
    return Object.freeze({ deletedRows, state: more ? 'more' as const : 'complete' as const })
  }

  async retireOperation(): Promise<void> {
    this.#store.candidates.clear()
    this.#store.committed.clear()
    this.#store.handles.clear()
    this.#store.proofs.clear()
    this.#store.ledgerEntries.clear()
    this.#store.ledgerPages.clear()
    this.#store.ledgerSeals.clear()
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

  #appendLedgerEntry(
    binding: MaterializationLedgerBindingV1,
    entry: MaterializationLedgerEntryV1,
  ) {
    this.#requireLedgerBinding(binding)
    const existing = this.#store.ledgerEntries.get(entry.entryId)
    if (existing !== undefined && existing.entryDigest !== entry.entryDigest) {
      throw new DOMException('materialization ledger entry changed', 'ConstraintError')
    }
    this.#store.ledgerEntries.set(entry.entryId, entry)
    return existing === undefined ? 'insert' as const : 'idempotent' as const
  }

  #replaceCommitted(previous: FileCheckpointV2, next: FileCheckpointV2): void {
    validateFileCheckpointTransition(previous, next)
    if (this.#store.committed.get(previous.recordId)?.checksum !== previous.checksum) {
      throw new DOMException('committed checkpoint predecessor changed', 'InvalidStateError')
    }
    this.#store.committed.set(next.recordId, next)
  }

  #requireLedgerBinding(binding: MaterializationLedgerBindingV1): void {
    if (binding.operationId !== this.binding.operationId ||
        binding.receiveIntentDigest !== this.binding.receiveIntentDigest ||
        binding.materializationBindingDigest !== this.binding.materializationBindingDigest ||
        binding.authorityRef !== this.binding.authorityRef) {
      throw new TypeError('memory materialization ledger belongs to another checkpoint namespace')
    }
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
