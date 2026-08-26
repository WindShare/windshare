import type { OutputDiagnosticsPorts } from '../../src/output/diagnostics'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  classifyCheckpointLineage,
  deriveCheckpointLineageID,
  fileCheckpointIsComplete,
  sameCheckpointLineageSpec,
  validateFileCheckpoint,
  validateFileCheckpointTransition,
  type FileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import {
  checkpointMatchesNamespace,
  finalFileCheckpointProof,
  type CheckpointLineageDecision,
  type CheckpointLineageLookupRequest,
  type FileCheckpointJournal,
  type InitialCheckpointCASResult,
  type FileCheckpointPage,
  type FileCheckpointScan,
  type FinalFileCheckpointProof,
  type PersistentHandleRecord,
} from '../../src/output/persistence/journal'
import { durableCheckpointNamespaceIdentity } from '../../src/output/persistence/namespace'
import type {
  OpenedFileRevision,
  SemanticPersistentOutputJournal,
} from '../../src/output/persistent-tree/contracts'
import type { FileCheckpointCandidateObservation } from '../../src/output/persistent-tree/recovery'
import { PersistentTreeOutputSession } from '../../src/output/persistent-tree/session'
import { identity } from './planning/fixture'
import { MemoryTree } from './persistent-tree-file-fixture'

export const FILE_ID = identity(21)
const FILE_REVISION = identity(22)

export async function materializationFixture(
  diagnostics?: OutputDiagnosticsPorts,
  maximumConcurrentInitialClaimInspections?: number,
) {
  const binding = durableCheckpointNamespaceIdentity({
    operationId: identity(1),
    receiveIntentDigest: identity(2, 32),
    materializationBindingDigest: identity(3, 32),
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: identity(4, 32),
  })
  const events: string[] = []
  const tree = new MemoryTree(events, binding)
  const checkpoints = new MemoryCheckpointRepository(binding)
  const session = await PersistentTreeOutputSession.open({
    tree,
    checkpoints, semantic: checkpoints,
    ...(maximumConcurrentInitialClaimInspections === undefined
      ? {}
      : { maximumConcurrentInitialClaimInspections }),
    ...(diagnostics === undefined ? {} : { diagnostics }),
  })
  return { binding, events, tree, checkpoints, session }
}

export function revision(exactSize: bigint): OpenedFileRevision {
  return Object.freeze({ fileId: FILE_ID, fileRevision: FILE_REVISION, exactSize })
}

export class MemoryCheckpointRepository implements FileCheckpointJournal {
  readonly binding: FileCheckpointJournal['binding']
  readonly #candidates = new Map<string, FileCheckpointV2>()
  readonly #committed = new Map<string, FileCheckpointV2>()
  readonly #handles = new Map<string, PersistentHandleRecord<unknown>>()
  readonly #finalProofs = new Map<string, NonNullable<Awaited<ReturnType<
    SemanticPersistentOutputJournal['readMaterializationFinalProof']
  >>>>()
  readonly #directoryAdmissions = new Map<string, string>()
  readonly #directoryFinalizations = new Map<string, string>()
  #failNextCommit = false
  #failCreatedFile = false
  #failFinal: 'before' | 'after' | undefined
  commitCreatedFileCount = 0
  commitFinalFileCount = 0
  readCommittedCount = 0
  finalCheckpointProofCount = 0
  readonly lineageBatchSizes: number[] = []
  readonly lineageBatches: string[][] = []
  readonly installedClaimBatches: string[][] = []

  constructor(binding: FileCheckpointJournal['binding']) {
    this.binding = binding
  }

  async lookupLineage(
    request: CheckpointLineageLookupRequest,
  ): Promise<CheckpointLineageDecision> {
    return this.#lineageDecision(request)
  }

  async classifyLineages(
    requests: readonly CheckpointLineageLookupRequest[],
  ): Promise<readonly CheckpointLineageDecision[]> {
    this.lineageBatchSizes.push(requests.length)
    this.lineageBatches.push(requests.map(request => request.canonicalPath.join('/')))
    await Promise.resolve()
    return Promise.all(requests.map(request => this.lookupLineage(request)))
  }

  installInitialClaims(
    candidates: readonly FileCheckpointV2[],
  ): Promise<readonly InitialCheckpointCASResult[]> {
    this.installedClaimBatches.push(candidates.map(candidate => candidate.canonicalPath.join('/')))
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
    this.#candidates.set(candidate.recordId, candidate)
    return Object.freeze({ kind: 'installed', lineageId, record: candidate })
  }

  async commitCreatedFile(
    input: Parameters<SemanticPersistentOutputJournal['commitCreatedFile']>[0],
  ): Promise<void> {
    this.commitCreatedFileCount += 1
    if (this.#failCreatedFile) {
      this.#failCreatedFile = false
      throw new Error('simulated atomic created-file commit failure')
    }
    validateFileCheckpointTransition(input.candidate, input.committed)
    if (this.#candidates.get(input.candidate.recordId)?.checksum !== input.candidate.checksum) {
      throw new DOMException('created-file candidate missing', 'InvalidStateError')
    }
    this.#handles.set(input.handle.id, input.handle)
    this.#committed.set(input.committed.recordId, input.committed)
    this.#candidates.delete(input.candidate.recordId)
  }

  async commitDurableCut(previous: FileCheckpointV2, durable: FileCheckpointV2): Promise<void> {
    await this.#replaceCommitted(previous, durable)
  }

  async resumePausedCheckpoint(paused: FileCheckpointV2, active: FileCheckpointV2): Promise<void> {
    await this.#replaceCommitted(paused, active)
  }

  async restartOwnedFile(
    input: Parameters<SemanticPersistentOutputJournal['restartOwnedFile']>[0],
  ) {
    const current = this.#committed.get(input.previous.recordId)
    if (current?.checksum === input.reset.checksum) return 'idempotent' as const
    if (current?.checksum !== input.previous.checksum ||
        this.#handles.get(input.expectedHandle.id)?.ownedObjectId !== input.previous.ownedObjectId) {
      throw new DOMException('owned-file restart authority changed', 'InvalidStateError')
    }
    validateFileCheckpoint(input.reset)
    this.#committed.set(input.reset.recordId, input.reset)
    return 'restart' as const
  }

  async commitFinalFile(
    input: Parameters<SemanticPersistentOutputJournal['commitFinalFile']>[0],
  ) {
    this.commitFinalFileCount += 1
    if (this.#failFinal === 'before') {
      this.#failFinal = undefined
      throw new Error('simulated pre-final-transaction crash')
    }
    const current = this.#committed.get(input.expectedCommittedCheckpoint.recordId)
    const final = input.records.finalCheckpoint
    const existingProof = this.#finalProofs.get(input.records.finalProof.proofId)
    const idempotent = current?.checksum === final.checksum &&
      existingProof?.proofDigest === input.records.finalProof.proofDigest
    if (!idempotent) {
      if (current?.checksum !== input.expectedCommittedCheckpoint.checksum) {
        throw new DOMException('final checkpoint predecessor changed', 'InvalidStateError')
      }
      this.#committed.set(final.recordId, final)
      this.#finalProofs.set(input.records.finalProof.proofId, input.records.finalProof)
    }
    const receipt = Object.freeze({
      classification: idempotent ? 'idempotent' as const : 'insert' as const,
      finalCheckpoint: final,
      finalProof: input.records.finalProof,
      ledgerEntry: input.records.ledgerEntry,
    })
    if (this.#failFinal === 'after') {
      this.#failFinal = undefined
      throw new Error('simulated ambiguous final transaction response')
    }
    return receipt
  }

  async readMaterializationFinalProof(
    _binding: Parameters<SemanticPersistentOutputJournal['readMaterializationFinalProof']>[0],
    proofId: string,
  ) {
    return this.#finalProofs.get(proofId)
  }

  async appendDirectoryAdmission(
    _binding: Parameters<SemanticPersistentOutputJournal['appendDirectoryAdmission']>[0],
    entry: Parameters<SemanticPersistentOutputJournal['appendDirectoryAdmission']>[1],
  ) {
    const existing = this.#directoryAdmissions.get(entry.entryId)
    if (existing !== undefined && existing !== entry.entryDigest) {
      throw new DOMException('directory admission changed', 'ConstraintError')
    }
    this.#directoryAdmissions.set(entry.entryId, entry.entryDigest)
    return existing === undefined ? 'insert' as const : 'idempotent' as const
  }

  async appendDirectoryFinalization(
    _binding: Parameters<SemanticPersistentOutputJournal['appendDirectoryFinalization']>[0],
    entry: Parameters<SemanticPersistentOutputJournal['appendDirectoryFinalization']>[1],
  ) {
    const existing = this.#directoryFinalizations.get(entry.entryId)
    if (existing !== undefined && existing !== entry.entryDigest) {
      throw new DOMException('directory finalization changed', 'ConstraintError')
    }
    this.#directoryFinalizations.set(entry.entryId, entry.entryDigest)
    return existing === undefined ? 'insert' as const : 'idempotent' as const
  }

  failNextCreatedFileCommit(): void { this.#failCreatedFile = true }
  failNextFinalCommit(cut: 'before' | 'after'): void { this.#failFinal = cut }

  committed(fileId: string): FileCheckpointV2 {
    const records = [...this.#committed.values()].filter(record => record.fileId === fileId)
    const latest = records.sort((left, right) =>
      left.stateGeneration < right.stateGeneration ? 1 : -1)[0]
    if (latest === undefined) throw new Error('test checkpoint is missing')
    return latest
  }

  async resolveCandidate(
    candidate: FileCheckpointV2,
    observation: Exclude<FileCheckpointCandidateObservation, { kind: 'ownership-unknown' }>,
  ): Promise<void> {
    const current = this.#candidates.get(candidate.recordId)
    if (current?.checksum !== candidate.checksum) {
      throw new DOMException('candidate changed during recovery', 'InvalidStateError')
    }
    const resolved = observation.kind === 'verified'
      ? observation.committed
      : observation.checkpoint
    this.#committed.set(candidate.recordId, resolved)
    this.#candidates.delete(candidate.recordId)
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
    const physical = new Map(this.#committed)
    for (const [recordId, candidate] of this.#candidates) {
      if (!physical.has(recordId)) physical.set(recordId, candidate)
    }
    const records = [...physical.values()].filter(record =>
      sameCheckpointLineageSpec(record, spec))
    const decision = classifyCheckpointLineage(
      { fileRevision: request.fileRevision, exactSize: request.exactSize },
      records.map(record => ({
        fileRevision: record.fileRevision,
        exactSize: record.exactSize,
        ownedObjectId: record.ownedObjectId,
      })),
    )
    if (decision === 'absent') {
      return Object.freeze({ kind: 'absent', lineageId: request.lineageId })
    }
    if (decision === 'exact') {
      return Object.freeze({ kind: 'exact', lineageId: request.lineageId, record: records[0]! })
    }
    return Object.freeze({
      kind: decision,
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
        this.#committed.get(previous.recordId)?.checksum !== previous.checksum) {
      throw new DOMException('committed checkpoint predecessor missing', 'InvalidStateError')
    }
    const current = this.#candidates.get(candidate.recordId)
    if (current !== undefined && current.checksum !== candidate.checksum) {
      throw new DOMException('checkpoint candidate changed', 'InvalidStateError')
    }
    this.#candidates.set(candidate.recordId, candidate)
  }

  async commitCheckpointCandidate(
    candidate: FileCheckpointV2,
    committed: FileCheckpointV2,
  ): Promise<void> {
    if (this.#failNextCommit) {
      this.#failNextCommit = false
      throw new Error('simulated post-object crash')
    }
    validateFileCheckpointTransition(candidate, committed)
    if (candidate.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE ||
        committed.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED) {
      throw new TypeError('memory repository commits only verified checkpoints')
    }
    const currentCandidate = this.#candidates.get(candidate.recordId)
    const previous = this.#committed.get(candidate.recordId)
    if (currentCandidate === undefined && previous?.checksum === committed.checksum) return
    if (currentCandidate?.checksum !== candidate.checksum) {
      throw new DOMException('candidate missing', 'InvalidStateError')
    }
    if (previous !== undefined) validateFileCheckpointTransition(previous, committed)
    this.#committed.set(committed.recordId, committed)
    this.#candidates.delete(committed.recordId)
  }

  seedCommittedForTest(record: FileCheckpointV2): void {
    validateFileCheckpoint(record)
    if (!checkpointMatchesNamespace(record, this.binding) ||
        record.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED) {
      throw new TypeError('test checkpoint seed escaped its namespace')
    }
    this.#committed.set(record.recordId, record)
  }

  seedCandidateForTest(record: FileCheckpointV2): void {
    validateFileCheckpoint(record)
    if (!checkpointMatchesNamespace(record, this.binding) ||
        record.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE) {
      throw new TypeError('test candidate seed escaped its namespace')
    }
    this.#candidates.set(record.recordId, record)
  }

  async readCommitted(recordId: string): Promise<FileCheckpointV2 | undefined> {
    this.readCommittedCount += 1
    return this.#committed.get(recordId)
  }

  async scanCommitted(scan: FileCheckpointScan): Promise<FileCheckpointPage> {
    return this.#scan(this.#committed, scan)
  }

  async scanCandidates(scan: FileCheckpointScan): Promise<FileCheckpointPage> {
    return this.#scan(this.#candidates, scan)
  }

  async finalCheckpointProof(
    recordId: string,
    generation: bigint,
  ): Promise<FinalFileCheckpointProof> {
    this.finalCheckpointProofCount += 1
    const record = this.#committed.get(recordId)
    if (record === undefined || record.checkpointGeneration !== generation ||
        !fileCheckpointIsComplete(record)) {
      throw new DOMException('final checkpoint missing', 'NotFoundError')
    }
    return finalFileCheckpointProof(record)
  }

  failNextCommit(): void {
    this.#failNextCommit = true
  }

  async retireOperation(): Promise<void> {
    this.#candidates.clear()
    this.#committed.clear()
  }

  async #replaceCommitted(previous: FileCheckpointV2, next: FileCheckpointV2): Promise<void> {
    validateFileCheckpointTransition(previous, next)
    if (this.#committed.get(previous.recordId)?.checksum !== previous.checksum) {
      throw new DOMException('durable checkpoint predecessor changed', 'InvalidStateError')
    }
    this.#committed.set(next.recordId, next)
  }

  #scan(
    records: ReadonlyMap<string, FileCheckpointV2>,
    scan: FileCheckpointScan,
  ): FileCheckpointPage {
    const sorted = [...records.values()]
      .filter((record) => scan.fileId === undefined || record.fileId === scan.fileId)
      .sort((left, right) => left.recordId.localeCompare(right.recordId))
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
}
