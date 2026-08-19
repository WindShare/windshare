import { describe, expect, it } from 'vitest'

import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  classifyCheckpointLineage,
  deriveCheckpointLineageID,
  fileCheckpointIsComplete,
  newFileCheckpointV2,
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
} from '../../src/output/persistence/journal'
import { durableCheckpointNamespaceIdentity } from '../../src/output/persistence/namespace'
import type {
  OpenedFileRevision,
  PersistentDirectoryMaterialization,
  PersistentOutputTree,
  PersistentTreeFile,
  PersistentTreeTraceEvent,
} from '../../src/output/persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../../src/output/persistent-tree/errors'
import type { FileCheckpointCandidateObservation } from '../../src/output/persistent-tree/recovery'
import { PersistentTreeOutputSession } from '../../src/output/persistent-tree/session'
import { identity } from './planning/fixture'

const FILE_ID = identity(21)
const FILE_REVISION = identity(22)
const NEXT_REVISION = identity(23)

describe('persistent DirectoryTree materialization port', () => {
  it('opens the authenticated revision before creating a visible file', async () => {
    const fixture = await materializationFixture()
    const transaction = await fixture.session.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => {
        fixture.events.push('revision-opened')
        return revision(4n)
      },
    })

    expect(fixture.events).toEqual([
      'authorize',
      'prepare-root',
      'revision-opened',
      'create:report.bin',
    ])
    expect(fixture.tree.visible(['report.bin'])).toEqual(new Uint8Array())
    expect(transaction.verifiedRanges).toEqual([])
  })

  it('keeps prefix writes visible while checkpoint truth advances only after flush', async () => {
    const fixture = await materializationFixture()
    const transaction = await fixture.session.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => revision(6n),
    })
    await transaction.writeRange(0n, Uint8Array.of(1, 2, 3))

    expect(fixture.tree.visible(['report.bin'])).toEqual(Uint8Array.of(1, 2, 3))
    expect(transaction.verifiedRanges).toEqual([])
    await expect(transaction.checkpoint()).resolves.toEqual([{ start: 0n, end: 3n }])
    expect(transaction.verifiedRanges).toEqual([{ start: 0n, end: 3n }])
  })

  it('reopens the same owned file after restart and completes from its persisted range', async () => {
    const fixture = await materializationFixture()
    const first = await fixture.session.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => revision(6n),
    })
    await first.writeRange(0n, Uint8Array.of(1, 2, 3))
    await first.checkpoint()
    await first.close()
    await fixture.session.close()

    const reopened = await PersistentTreeOutputSession.open({
      tree: fixture.tree,
      checkpoints: fixture.checkpoints,
    })
    const second = await reopened.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => revision(6n),
    })
    expect(second.ownedObjectId).toBe(first.ownedObjectId)
    expect(second.verifiedRanges).toEqual([{ start: 0n, end: 3n }])
    expect(fixture.events.filter((event) => event === 'create:report.bin')).toHaveLength(1)

    await second.writeRange(3n, Uint8Array.of(4, 5, 6))
    const proof = await second.commit()
    expect(proof.complete).toBe(true)
    await expect(second.commit()).resolves.toEqual(proof)
    await expect(second.writeRange(0n, Uint8Array.of(9))).rejects.toMatchObject({
      kind: 'output-state',
    })
    await expect(second.checkpoint()).rejects.toMatchObject({ kind: 'output-state' })
    expect(fixture.tree.visible(['report.bin'])).toEqual(Uint8Array.of(1, 2, 3, 4, 5, 6))
  })

  it('publishes a genuine zero-byte revision through the ordinary file transaction', async () => {
    const fixture = await materializationFixture()
    const transaction = await fixture.session.beginFile({
      artifactPath: ['empty.bin'],
      openRevision: async () => revision(0n),
    })

    const proof = await transaction.commit()
    expect(proof.exactSize).toBe(0n)
    expect(proof.complete).toBe(true)
    expect(transaction.verifiedRanges).toEqual([])
    expect(fixture.tree.visible(['empty.bin'])).toEqual(new Uint8Array())
  })

  it('does not create a replacement when the opened revision changes', async () => {
    const fixture = await materializationFixture()
    const first = await fixture.session.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => revision(1n),
    })
    await first.writeRange(0n, Uint8Array.of(9))
    await first.commit()
    await first.close()

    await expect(fixture.session.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => ({
        fileId: FILE_ID,
        fileRevision: NEXT_REVISION,
        exactSize: 1n,
      }),
    })).rejects.toMatchObject({
      name: 'CheckpointLineageDecisionError',
      decision: 'revision-conflict',
    })
    expect(fixture.events.filter((event) => event === 'create:report.bin')).toHaveLength(1)
  })

  it('blocks an invalid same-revision size without creating another object', async () => {
    const fixture = await materializationFixture()
    const first = await fixture.session.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => revision(1n),
    })
    await first.writeRange(0n, Uint8Array.of(1))
    await first.commit()
    await first.close()

    await expect(fixture.session.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => revision(2n),
    })).rejects.toMatchObject({
      name: 'CheckpointLineageDecisionError',
      decision: 'invalid',
    })
    expect(fixture.events.filter(event => event === 'create:report.bin')).toHaveLength(1)
  })

  it('blocks multiple persisted objects for one lineage without moving ranges', async () => {
    const fixture = await materializationFixture()
    const first = await fixture.session.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => revision(2n),
    })
    await first.writeRange(0n, Uint8Array.of(1))
    await first.checkpoint()
    await first.close()
    const original = (await fixture.checkpoints.scanCommitted({
      direction: 'ascending',
      fileId: FILE_ID,
    })).records[0]!
    const conflictingCandidate = newFileCheckpointV2({
      ...original,
      ownedObjectId: identity(99, 32),
      commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
    })
    fixture.checkpoints.seedCommittedForTest(newFileCheckpointV2({
      ...conflictingCandidate,
      commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
    }))

    await expect(fixture.session.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => revision(2n),
    })).rejects.toMatchObject({
      name: 'CheckpointLineageDecisionError',
      decision: 'ownership-conflict',
    })
    const records = (await fixture.checkpoints.scanCommitted({
      direction: 'ascending',
      fileId: FILE_ID,
    })).records
    expect(records).toHaveLength(2)
    expect(records.find(record => record.recordId === original.recordId)?.verifiedRanges)
      .toEqual([{ start: 0n, end: 1n }])
    expect(records.find(record => record.recordId !== original.recordId)?.verifiedRanges)
      .toEqual([{ start: 0n, end: 1n }])
  })

  it('classifies an occupied absent-lineage destination as a collision before claiming', async () => {
    const fixture = await materializationFixture()
    fixture.tree.occupy(['report.bin'], identity(88, 32))

    await expect(fixture.session.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => revision(1n),
    })).rejects.toMatchObject({ name: 'DestinationCollisionError', kind: 'collision' })
    expect((await fixture.checkpoints.scanCandidates({ direction: 'ascending' })).records)
      .toEqual([])
  })

  it('recreates only the selected authority after a candidate-before-object restart', async () => {
    const fixture = await materializationFixture()
    fixture.tree.failNextCreation()
    await expect(fixture.session.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => revision(2n),
    })).rejects.toThrow('simulated pre-object crash')
    const selected = fixture.tree.proposedOwnedObjectIds[0]!
    expect((await fixture.checkpoints.scanCandidates({ direction: 'ascending' })).records)
      .toEqual([expect.objectContaining({ ownedObjectId: selected })])
    await fixture.session.close()

    const reopened = await PersistentTreeOutputSession.open({
      tree: fixture.tree,
      checkpoints: fixture.checkpoints,
    })
    const resumed = await reopened.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => revision(2n),
    })
    expect(resumed.ownedObjectId).toBe(selected)
    expect(fixture.tree.proposedOwnedObjectIds).toEqual([selected])
    expect(fixture.events.filter(event => event === 'create:report.bin')).toHaveLength(1)
  })

  it('rejects unresolved post-object recovery as operation ownership attention', async () => {
    const fixture = await materializationFixture()
    const transaction = await fixture.session.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => revision(2n),
    })
    await transaction.writeRange(0n, Uint8Array.of(7))
    await transaction.checkpoint()
    await transaction.close()
    const committed = (await fixture.checkpoints.scanCommitted({
      direction: 'ascending',
      fileId: FILE_ID,
    })).records[0]!
    fixture.checkpoints.seedCandidateForTest(newFileCheckpointV2({
      ...committed,
      stateGeneration: committed.stateGeneration + 1n,
      checkpointGeneration: committed.checkpointGeneration + 1n,
      commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
    }))
    fixture.tree.occupy(['report.bin'], identity(88, 32))
    await fixture.session.close()
    const trace: PersistentTreeTraceEvent[] = []

    await expect(PersistentTreeOutputSession.open({
      tree: fixture.tree,
      checkpoints: fixture.checkpoints,
      trace: event => trace.push(event),
    })).rejects.toMatchObject({
      name: 'InvalidStateError',
      reason: 'target-ownership-unknown',
      stage: 'checkpoint',
    })

    expect(trace).toEqual([expect.objectContaining({
      name: 'receive.operation.needs_attention',
      operation_id: fixture.binding.operationId,
      needs_attention_reason: 'target-ownership-unknown',
    })])
    expect((await fixture.checkpoints.scanCandidates({ direction: 'ascending' })).records)
      .toEqual([expect.objectContaining({ recordId: committed.recordId })])
  })

  it('recovers the same object when restart occurs after creation but before promotion', async () => {
    const fixture = await materializationFixture()
    fixture.checkpoints.failNextCommit()
    await expect(fixture.session.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => revision(2n),
    })).rejects.toThrow('simulated post-object crash')
    const selected = fixture.tree.proposedOwnedObjectIds[0]!
    expect(fixture.tree.visible(['report.bin'])).toEqual(new Uint8Array())
    await fixture.session.close()

    const reopened = await PersistentTreeOutputSession.open({
      tree: fixture.tree,
      checkpoints: fixture.checkpoints,
    })
    expect((await fixture.checkpoints.scanCandidates({ direction: 'ascending' })).records)
      .toEqual([])
    const resumed = await reopened.beginFile({
      artifactPath: ['report.bin'],
      openRevision: async () => revision(2n),
    })
    expect(resumed.ownedObjectId).toBe(selected)
    expect(fixture.tree.proposedOwnedObjectIds).toEqual([selected])
    expect(fixture.events.filter(event => event === 'create:report.bin')).toHaveLength(1)
  })

  it('concurrent callers converge on the repository-selected object identity', async () => {
    const fixture = await materializationFixture()
    const request = {
      artifactPath: ['report.bin'],
      openRevision: async () => revision(2n),
    } as const

    const [left, right] = await Promise.all([
      fixture.session.beginFile(request),
      fixture.session.beginFile(request),
    ])
    expect(left.ownedObjectId).toBe(right.ownedObjectId)
    expect(fixture.events.filter(event => event === 'create:report.bin')).toHaveLength(1)
    expect((await fixture.checkpoints.scanCommitted({
      direction: 'ascending',
      fileId: FILE_ID,
    })).records).toHaveLength(1)
  })

  it('rechecks object identity before writer acquisition and after checkpoint commit', async () => {
    const fixture = await materializationFixture()
    const beforeWriter = await fixture.session.beginFile({
      artifactPath: ['writer.bin'],
      openRevision: async () => revision(1n),
    })
    fixture.tree.failVerification(['writer.bin'], 'writer-open', 1)
    await expect(beforeWriter.writeRange(0n, Uint8Array.of(1))).rejects.toBeInstanceOf(
      TargetOwnershipUnknownError,
    )
    expect(beforeWriter.verifiedRanges).toEqual([])

    const afterCommit = await fixture.session.beginFile({
      artifactPath: ['commit.bin'],
      openRevision: async () => ({ ...revision(1n), fileId: identity(31) }),
    })
    await afterCommit.writeRange(0n, Uint8Array.of(7))
    fixture.tree.failVerification(['commit.bin'], 'commit', 2)
    await expect(afterCommit.commit()).rejects.toBeInstanceOf(TargetOwnershipUnknownError)
  })

  it('isolates one failed file without removing another successful visible file', async () => {
    const fixture = await materializationFixture()
    const good = await fixture.session.beginFile({
      artifactPath: ['good.bin'],
      openRevision: async () => ({ ...revision(1n), fileId: identity(41) }),
    })
    await good.writeRange(0n, Uint8Array.of(1))
    await good.commit()

    const bad = await fixture.session.beginFile({
      artifactPath: ['bad.bin'],
      openRevision: async () => ({ ...revision(1n), fileId: identity(42) }),
    })
    fixture.tree.failVerification(['bad.bin'], 'writer-open', 1)
    await expect(bad.writeRange(0n, Uint8Array.of(2))).rejects.toBeInstanceOf(
      TargetOwnershipUnknownError,
    )
    expect(fixture.tree.visible(['good.bin'])).toEqual(Uint8Array.of(1))
    expect(fixture.tree.visible(['bad.bin'])).toEqual(new Uint8Array())
  })
})

async function materializationFixture() {
  const binding = durableCheckpointNamespaceIdentity({
    operationId: identity(1),
    receiveIntentDigest: identity(2, 32),
    materializationBindingDigest: identity(3, 32),
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: identity(4, 32),
  })
  const events: string[] = []
  const tree = new MemoryTree(events)
  const checkpoints = new MemoryCheckpointRepository(binding)
  const session = await PersistentTreeOutputSession.open({ tree, checkpoints })
  return { binding, events, tree, checkpoints, session }
}

function revision(exactSize: bigint): OpenedFileRevision {
  return Object.freeze({ fileId: FILE_ID, fileRevision: FILE_REVISION, exactSize })
}

class MemoryTree implements PersistentOutputTree {
  readonly #events: string[]
  readonly #files = new Map<string, MemoryFile>()
  readonly proposedOwnedObjectIds: string[] = []
  #failNextCreation = false
  #nextObject = 60

  constructor(events: string[]) {
    this.#events = events
  }

  async authorize(): Promise<void> {
    this.#events.push('authorize')
  }

  async prepareRoot(): Promise<void> {
    this.#events.push('prepare-root')
  }

  async ensureDirectory(path: readonly string[]): Promise<PersistentDirectoryMaterialization> {
    if (path.some((component) => component.length === 0)) throw new TypeError('empty path component')
    return Object.freeze({ ownedObjectId: identity(this.#nextObject++, 32), created: true })
  }

  async validateDirectory(): Promise<boolean> {
    return true
  }

  async proposeFileOwnedObjectId(
    _path: readonly string[],
    revision: OpenedFileRevision,
  ): Promise<string> {
    if (revision.exactSize < 0n) throw new RangeError('negative revision size')
    const proposed = identity(this.#nextObject++, 32)
    this.proposedOwnedObjectIds.push(proposed)
    return proposed
  }

  async inspectFileDestination(
    path: readonly string[],
  ): Promise<'absent' | 'occupied'> {
    return this.#files.has(path.join('/')) ? 'occupied' : 'absent'
  }

  async createFileAfterRevisionOpen(
    path: readonly string[],
    revision: OpenedFileRevision,
    selectedOwnedObjectId: string,
  ): Promise<PersistentTreeFile> {
    if (revision.exactSize < 0n) throw new RangeError('negative revision size')
    const key = path.join('/')
    const existing = this.#files.get(key)
    if (existing !== undefined) {
      if (existing.ownedObjectId === selectedOwnedObjectId) return existing
      throw new DOMException('collision', 'InvalidModificationError')
    }
    if (this.#failNextCreation) {
      this.#failNextCreation = false
      throw new Error('simulated pre-object crash')
    }
    this.#events.push(`create:${key}`)
    const file = new MemoryFile(selectedOwnedObjectId)
    this.#files.set(key, file)
    return file
  }

  async openFile(
    path: readonly string[],
    ownedObjectId: string,
  ): Promise<PersistentTreeFile | undefined> {
    const file = this.#files.get(path.join('/'))
    return file?.ownedObjectId === ownedObjectId ? file : undefined
  }

  async removeFile(path: readonly string[], ownedObjectId: string): Promise<void> {
    const key = path.join('/')
    if (this.#files.get(key)?.ownedObjectId !== ownedObjectId) {
      throw new TargetOwnershipUnknownError('cleanup', identity(1))
    }
    this.#files.delete(key)
  }

  async removeDirectory(): Promise<void> {}

  failNextCreation(): void {
    this.#failNextCreation = true
  }

  occupy(path: readonly string[], ownedObjectId: string): void {
    this.#files.set(path.join('/'), new MemoryFile(ownedObjectId))
  }

  visible(path: readonly string[]): Uint8Array | undefined {
    return this.#files.get(path.join('/'))?.snapshot()
  }

  failVerification(
    path: readonly string[],
    stage: 'writer-open' | 'checkpoint' | 'commit',
    occurrence: number,
  ): void {
    this.#files.get(path.join('/'))?.failVerification(stage, occurrence)
  }
}

class MemoryFile implements PersistentTreeFile {
  readonly ownedObjectId: string
  #bytes = new Uint8Array()
  readonly #verificationCounts = new Map<string, number>()
  #failure: { readonly stage: string; readonly occurrence: number } | undefined

  constructor(ownedObjectId: string) {
    this.ownedObjectId = ownedObjectId
  }

  async writeAt(offset: bigint, data: Uint8Array): Promise<void> {
    await this.verify('writer-open')
    const start = Number(offset)
    const size = Math.max(this.#bytes.byteLength, start + data.byteLength)
    const next = new Uint8Array(size)
    next.set(this.#bytes)
    next.set(data, start)
    this.#bytes = next
  }

  async flush(): Promise<void> {}

  async size(): Promise<bigint> {
    return BigInt(this.#bytes.byteLength)
  }

  async verify(stage: 'writer-open' | 'checkpoint' | 'commit'): Promise<void> {
    const count = (this.#verificationCounts.get(stage) ?? 0) + 1
    this.#verificationCounts.set(stage, count)
    if (this.#failure?.stage === stage && this.#failure.occurrence === count) {
      throw new TargetOwnershipUnknownError(stage, identity(1))
    }
  }

  async close(): Promise<void> {}

  async read(): Promise<Blob> {
    return new Blob([this.#bytes])
  }

  snapshot(): Uint8Array {
    return this.#bytes.slice()
  }

  failVerification(stage: string, occurrence: number): void {
    this.#failure = { stage, occurrence }
  }
}

class MemoryCheckpointRepository implements FileCheckpointJournal {
  readonly binding: FileCheckpointJournal['binding']
  readonly #candidates = new Map<string, FileCheckpointV2>()
  readonly #committed = new Map<string, FileCheckpointV2>()
  #failNextCommit = false

  constructor(binding: FileCheckpointJournal['binding']) {
    this.binding = binding
  }

  async lookupLineage(
    request: CheckpointLineageLookupRequest,
  ): Promise<CheckpointLineageDecision> {
    return this.#lineageDecision(request)
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
