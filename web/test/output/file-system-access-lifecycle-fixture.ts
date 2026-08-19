import { directoryId } from '../../src/catalog/model'
import {
  createCompleteDirectoryResultRoot,
  createDirectTreePlan,
  createReceiveIntent,
  createResultRootDirectoryTreeArtifact,
  createSelectionSpec,
  createSingleFileDirectoryTreeArtifact,
  type DirectoryTreeArtifact,
  type DirectTreePlan,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import type { DirectoryAdmission } from '../../src/transfer/directory-admission'
import { fsaParentOffer } from '../../src/output/capability/acquisition'
import type { AcquiredFSAParentAuthority } from '../../src/output/capability/contract'
import type { BrowserLockManagerRuntime } from '../../src/output/browser/namespace-mutation'
import { verifyFSAOperationBinding } from '../../src/output/browser/indexeddb-root-binding'
import {
  bindNewFileSystemAccessOutput,
  type FSAFileCheckpointRepository,
  type FSAFileCheckpointRepositoryFactory,
  type FSAOutputTraceEvent,
  type FileSystemAccessOutputSession,
} from '../../src/output/file-system-access/session'
import { createFileSystemAccessSettlementAuthority } from '../../src/output/file-system-access/settlement'
import {
  discardReopenedFileSystemAccessOutput,
  type ReopenedFileSystemAccessDiscardOperation,
} from '../../src/output/file-system-access/fresh-page-discard'
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
import type {
  FileCheckpointCandidateObservation,
} from '../../src/output/persistent-tree/recovery'
import {
  RECEIVE_RECORD_LIFECYCLE_STATE,
  receiveOperationLeaseRecord,
  type PersistedReceiveRecord,
  type ReceiveOperationHandleRecord,
  type ReceiveOperationLeaseRecord,
} from '../../src/output/workspace/records'
import { createExpiryReceipt, persistedReceiptRecord } from '../../src/output/workspace/receipts'
import {
  prepareReceiveOperationTransition,
  type ReceiveOperationRepository,
  type ReceiveOperationTransition,
} from '../../src/output/workspace/repository'
import { reduceReceiveLifecycle } from '../../src/output/workspace/lifecycle'
import { decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import { initialReceiveLifecycleState, type ReceiveLifecycleState } from '../../src/output/workspace/state'
import { outputSessionIdentity } from '../../src/transfer/output-session'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  transferWorkerSettlement,
} from '../../src/transfer/outcome'
import { createPersistentDirectTreeExecution } from '../../src/transfer/settlement/persistent-execution'
import { identity } from './planning/fixture'

export const SIGNAL = new AbortController().signal
export const SUCCESS = transferWorkerSettlement('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
export const PAUSED = transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY)

export interface FreshDiscardFixture {
  readonly parent: MemoryDirectory
  readonly repository: MemoryOperationRepository
  readonly checkpointFactory: FSAFileCheckpointRepositoryFactory
  readonly locks: MemoryLockManager
  readonly intent: ReceiveIntent
  readonly reservationName: string
  readonly leaseId: string
  readonly operation: ReopenedFileSystemAccessDiscardOperation
  retired(): number
  closed(): number
}

export async function freshDiscardFixture(input: Readonly<{
  seed: number
  successfulFile: boolean
}>): Promise<FreshDiscardFixture> {
  const parent = new MemoryDirectory(`downloads-${input.seed}`)
  const primaryRepository = new MemoryOperationRepository()
  const locks = new MemoryLockManager()
  let retireCount = 0
  const checkpointFactory = memoryCheckpointFactory(() => { retireCount += 1 })
  const session = await bindTask({
    parent,
    repository: primaryRepository,
    checkpointFactory,
    locks,
    artifact: input.successfulFile ? await resultRootArtifact() : await singleFileArtifact(),
    operationSeed: input.seed,
  })
  const initialLeaseId = identity(input.seed + 3)
  const transferJobId = identity(input.seed + 4)
  await startReceiving(primaryRepository, session.intent, initialLeaseId)
  const execution = await fsaExecution(
    session,
    primaryRepository,
    initialLeaseId,
    transferJobId,
    identity(input.seed + 5),
  )

  let parentAdmission: DirectoryAdmission | undefined
  if (input.successfulFile) {
    parentAdmission = await execution.directories.admitDirectory({
      source: {
        directoryId: directoryId(session.intent.syntheticRoot),
        generation: identity(input.seed + 6),
        path: Object.freeze([]),
      },
      artifactPath: Object.freeze([]),
    }, SIGNAL)
    const complete = await execution.output.beginFile(outputFileRequest({
      intent: session.intent,
      fileId: identity(input.seed + 7),
      fileRevision: identity(input.seed + 8),
      artifactPath: ['kept.bin'],
      exactSize: 2n,
      parentAdmission,
    }), SIGNAL)
    await complete.transaction.writeRange(0n, Uint8Array.of(7, 8), SIGNAL)
    await complete.transaction.commit(SIGNAL)
  }

  const unfinished = await execution.output.beginFile(outputFileRequest({
    intent: session.intent,
    fileId: input.successfulFile ? identity(input.seed + 9) : identity(3),
    fileRevision: identity(input.seed + 10),
    artifactPath: input.successfulFile
      ? ['unfinished.bin']
      : [session.reservation.reservedName],
    exactSize: 2n,
    ...(parentAdmission === undefined ? {} : { parentAdmission }),
  }), SIGNAL)
  await unfinished.transaction.writeRange(0n, Uint8Array.of(1), SIGNAL)
  await unfinished.transaction.pause('fresh-page-discard')
  await execution.pause({
    worker: PAUSED,
    materialization: input.successfulFile
      ? { entryCount: 1n, fileCount: 1n, directoryCount: 0n, rawBytes: 2n }
      : { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
    reason: new DOMException('page closed', 'AbortError'),
  }, SIGNAL)

  // A distinct repository object proves that cleanup does not depend on the old page's runtime.
  const repository = new MemoryOperationRepository({
    records: primaryRepository.records,
    handles: primaryRepository.handles,
    leases: primaryRepository.leases,
  })
  const lifecycle = await lifecycleState(repository, session.intent.operationId)
  const leaseId = identity(input.seed + 11)
  await repository.commitTransition({
    operationId: session.intent.operationId,
    expectedLifecycleGeneration: lifecycle.generation,
    expectedLeaseId: initialLeaseId,
    lease: {
      kind: 'put',
      record: receiveOperationLeaseRecord({
        operationId: session.intent.operationId,
        leaseId,
        acquiredAt: 1_500,
      }),
    },
  })
  const binding = await verifyFSAOperationBinding({
    repository,
    intent: session.intent,
  })
  let closeCount = 0
  let closePromise: Promise<void> | undefined
  const operation: ReopenedFileSystemAccessDiscardOperation = Object.freeze({
    kind: 'direct-tree',
    intent: session.intent,
    lifecycle,
    binding,
    lease: Object.freeze({ operationId: session.intent.operationId, leaseId }),
    repository,
    close: () => {
      closePromise ??= (async () => {
        closeCount += 1
        await repository.commitTransition({
          operationId: session.intent.operationId,
          expectedLeaseId: leaseId,
          lease: { kind: 'delete', leaseId },
        })
        repository.close()
      })()
      return closePromise
    },
  })
  return Object.freeze({
    parent,
    repository,
    checkpointFactory,
    locks,
    intent: session.intent,
    reservationName: session.reservation.reservedName,
    leaseId,
    operation,
    retired: () => retireCount,
    closed: () => closeCount,
  })
}

export function discardFreshFixture(
  fixture: FreshDiscardFixture,
  operation: ReopenedFileSystemAccessDiscardOperation = fixture.operation,
  nowMilliseconds = 2_000,
) {
  return discardReopenedFileSystemAccessOutput({
    operation,
    lockManager: fixture.locks,
    checkpointRepositoryFactory: fixture.checkpointFactory,
    clock: () => nowMilliseconds,
  })
}

export async function persistFreshFixtureExpiry(
  fixture: FreshDiscardFixture,
): Promise<Extract<ReceiveLifecycleState, { kind: 'expired' }>> {
  const current = await lifecycleState(fixture.repository, fixture.intent.operationId)
  if (current.kind !== 'resumable-receive') {
    throw new TypeError('fresh discard expiry fixture is not resumable')
  }
  const receipt = await createExpiryReceipt({
    operationId: fixture.intent.operationId,
    receiveIntentDigest: fixture.intent.digest,
    priorStableState: 'resumable-receive',
    expiresAt: current.expiresAt,
    retainedSuccessCount: current.completedFileCount,
    cleanupState: 'cleanup-pending',
  })
  const reduction = reduceReceiveLifecycle(current, {
    kind: 'expiry-observed',
    expiryReceiptDigest: receipt.digest,
    cleanupState: 'cleanup-pending',
    expectedGeneration: current.generation,
    leaseId: fixture.leaseId,
  }, {
    planKind: 'direct-tree',
    preparationRequired: false,
    activeLeaseId: fixture.leaseId,
    nowMilliseconds: current.expiresAt,
  })
  if (reduction.status !== 'applied' || reduction.state.kind !== 'expired') {
    throw new TypeError('fresh discard expiry fixture did not become Expired')
  }
  await fixture.repository.commitTransition({
    operationId: fixture.intent.operationId,
    expectedLifecycleGeneration: current.generation,
    expectedLeaseId: fixture.leaseId,
    records: [await persistedReceiptRecord(receipt)],
    lifecycle: reduction.state,
  })
  return reduction.state
}

export async function bindTask(input: Readonly<{
  parent: MemoryDirectory
  repository: MemoryOperationRepository
  checkpointFactory: FSAFileCheckpointRepositoryFactory
  locks: MemoryLockManager
  artifact: DirectoryTreeArtifact
  operationSeed: number
  trace?: (event: FSAOutputTraceEvent) => void
}>) {
  const selection = await selectionSpec()
  return bindNewFileSystemAccessOutput({
    authority: acquiredParent(input.parent),
    artifact: input.artifact,
    operationRepository: input.repository,
    lockManager: input.locks,
    checkpointRepositoryFactory: input.checkpointFactory,
    operationId: identity(input.operationSeed),
    reservationId: identity(input.operationSeed + 1),
    authorityRef: identity(input.operationSeed + 2, 32),
    ...(input.trace === undefined ? {} : { trace: input.trace }),
    freezeIntent: async (reservation) => createReceiveIntent({
      selection,
      artifact: input.artifact,
      plan: await createDirectTreePlan(input.artifact, reservation),
    }),
  })
}

export async function fsaExecution(
  session: FileSystemAccessOutputSession,
  repository: MemoryOperationRepository,
  lifecycleLeaseId: string,
  transferJobId: string,
  outputSessionId: string,
) {
  const settlement = await createFileSystemAccessSettlementAuthority({
    intent: session.intent,
    repository,
    lifecycleLeaseId,
    transferJobId,
    clock: () => 1_000,
  })
  return createPersistentDirectTreeExecution({
    intent: directTreeIntent(session.intent),
    materialization: session,
    outputIdentity: outputSessionIdentity({ backend: 'fsa-test', outputSessionId }),
    settlement: settlement.bindMaterialization(session),
  })
}

export function outputFileRequest(input: Readonly<{
  intent: ReceiveIntent
  fileId: string
  fileRevision: string
  artifactPath: readonly string[]
  exactSize: bigint
  parentAdmission?: DirectoryAdmission
}>) {
  return Object.freeze({
    source: Object.freeze({ shareInstance: input.intent.shareInstance, fileId: input.fileId }),
    sourcePath: Object.freeze([...input.artifactPath]),
    artifactPath: Object.freeze([...input.artifactPath]),
    expectedSize: input.exactSize,
    ...(input.parentAdmission === undefined ? {} : { parentAdmission: input.parentAdmission }),
    openRevision: async () => Object.freeze({
      shareInstance: input.intent.shareInstance,
      fileId: input.fileId,
      fileRevision: input.fileRevision,
      exactSize: input.exactSize,
    }),
  })
}

function directTreeIntent(
  intent: ReceiveIntent,
): ReceiveIntent & Readonly<{ plan: DirectTreePlan }> {
  if (intent.plan.kind !== 'direct-tree') throw new TypeError('test intent is not DirectTree')
  return intent as ReceiveIntent & Readonly<{ plan: DirectTreePlan }>
}

export async function installIntentFrozen(
  repository: MemoryOperationRepository,
  intent: ReceiveIntent,
  leaseId: string,
): Promise<void> {
  await repository.commitTransition({
    operationId: intent.operationId,
    lifecycle: initialReceiveLifecycleState({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
    }),
    lease: {
      kind: 'put',
      record: receiveOperationLeaseRecord({
        operationId: intent.operationId,
        leaseId,
        acquiredAt: 1_000,
      }),
    },
  })
}

export async function startReceiving(
  repository: MemoryOperationRepository,
  intent: ReceiveIntent,
  leaseId: string,
): Promise<void> {
  await installIntentFrozen(repository, intent, leaseId)
  const current = await lifecycleState(repository, intent.operationId)
  const reduction = reduceReceiveLifecycle(current, {
    kind: 'receive-started',
    expectedGeneration: current.generation,
    leaseId,
  }, {
    planKind: 'direct-tree',
    preparationRequired: false,
    activeLeaseId: leaseId,
    nowMilliseconds: 1_000,
  })
  await repository.commitTransition({
    operationId: intent.operationId,
    expectedLifecycleGeneration: current.generation,
    expectedLeaseId: leaseId,
    lifecycle: reduction.state,
  })
}

export async function resumeReceiving(
  repository: MemoryOperationRepository,
  intent: ReceiveIntent,
  leaseId: string,
): Promise<void> {
  const current = await lifecycleState(repository, intent.operationId)
  const reduction = reduceReceiveLifecycle(current, {
    kind: 'resume-started',
    expectedGeneration: current.generation,
    leaseId,
  }, {
    planKind: 'direct-tree',
    preparationRequired: false,
    activeLeaseId: leaseId,
    nowMilliseconds: 1_500,
  })
  await repository.commitTransition({
    operationId: intent.operationId,
    expectedLifecycleGeneration: current.generation,
    expectedLeaseId: leaseId,
    lifecycle: reduction.state,
  })
}

async function lifecycleState(
  repository: MemoryOperationRepository,
  operationId: string,
): Promise<ReceiveLifecycleState> {
  const record = await repository.readLifecycle(operationId)
  if (record === undefined) throw new TypeError('test lifecycle is missing')
  return decodeStoredReceiveLifecycleState(record)
}

async function selectionSpec() {
  return createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
}

export async function resultRootArtifact(): Promise<DirectoryTreeArtifact> {
  return createResultRootDirectoryTreeArtifact(
    createCompleteDirectoryResultRoot(identity(70), 'photos'),
  )
}

export async function singleFileArtifact(): Promise<DirectoryTreeArtifact> {
  return createSingleFileDirectoryTreeArtifact({
    fileId: identity(3),
    sourcePath: 'report.bin',
    outputName: 'report.bin',
  })
}

function acquiredParent(parent: MemoryDirectory): AcquiredFSAParentAuthority {
  const offer = fsaParentOffer()
  return Object.freeze({
    kind: 'fsa-parent-directory-authority',
    environmentTargetOfferId: offer.id,
    offer,
    parent: parent as unknown as FileSystemDirectoryHandle,
  })
}

interface MemoryOperationStore {
  readonly records: Map<string, PersistedReceiveRecord>
  readonly handles: Map<string, ReceiveOperationHandleRecord>
  readonly leases: Map<string, ReceiveOperationLeaseRecord>
}

export class MemoryOperationRepository implements ReceiveOperationRepository {
  readonly records: Map<string, PersistedReceiveRecord>
  readonly handles: Map<string, ReceiveOperationHandleRecord>
  readonly leases: Map<string, ReceiveOperationLeaseRecord>
  afterFirstCommit: (() => Promise<void>) | undefined
  #commitCount = 0

  constructor(state: MemoryOperationStore = {
    records: new Map(),
    handles: new Map(),
    leases: new Map(),
  }) {
    this.records = state.records
    this.handles = state.handles
    this.leases = state.leases
  }

  async commitTransition(transition: ReceiveOperationTransition): Promise<void> {
    if (transition.expectedLifecycleGeneration !== undefined) {
      const current = await this.readLifecycle(transition.operationId)
      if (current === undefined ||
          decodeStoredReceiveLifecycleState(current).generation !==
            transition.expectedLifecycleGeneration) {
        throw new DOMException('lifecycle generation changed', 'InvalidStateError')
      }
    }
    if (transition.expectedLeaseId !== undefined &&
        this.leases.get(transition.operationId)?.leaseId !== transition.expectedLeaseId) {
      throw new DOMException('operation lease changed', 'InvalidStateError')
    }
    const prepared = await prepareReceiveOperationTransition(transition)
    for (const record of prepared.records) this.records.set(record.id, record)
    for (const handle of prepared.handles) this.handles.set(handle.id, handle)
    for (const id of prepared.deleteRecordIds) this.records.delete(id)
    for (const id of prepared.deleteHandleIds) this.handles.delete(id)
    if (prepared.lease?.kind === 'put') {
      this.leases.set(prepared.operationId, prepared.lease.record)
    } else if (prepared.lease?.kind === 'delete') {
      this.leases.delete(prepared.operationId)
    }
    this.#commitCount += 1
    if (this.#commitCount === 1) await this.afterFirstCommit?.()
  }

  async readRecord(id: string): Promise<PersistedReceiveRecord | undefined> {
    return this.records.get(id)
  }

  async readLifecycle(operationId: string): Promise<PersistedReceiveRecord | undefined> {
    return [...this.records.values()].find(record =>
      record.operationId === operationId && record.kind === RECEIVE_RECORD_LIFECYCLE_STATE)
  }

  async listRecords(operationId: string): Promise<readonly PersistedReceiveRecord[]> {
    return [...this.records.values()].filter((record) => record.operationId === operationId)
  }

  async listManifestPages(): Promise<readonly []> {
    return []
  }

  async readHandle<T = unknown>(id: string): Promise<ReceiveOperationHandleRecord<T> | undefined> {
    return this.handles.get(id) as ReceiveOperationHandleRecord<T> | undefined
  }

  async listHandles(operationId: string): Promise<readonly ReceiveOperationHandleRecord[]> {
    return [...this.handles.values()]
      .filter(handle => handle.operationId === operationId)
      .sort((left, right) => left.id.localeCompare(right.id))
  }

  async readLease(operationId: string): Promise<ReceiveOperationLeaseRecord | undefined> {
    return this.leases.get(operationId)
  }

  recordsOfKind(kind: PersistedReceiveRecord['kind']): readonly PersistedReceiveRecord[] {
    return [...this.records.values()].filter(record => record.kind === kind)
  }

  close(): void {}
}

export class MemoryLockManager implements BrowserLockManagerRuntime {
  readonly #held = new Set<string>()
  releaseCount = 0

  async request(
    name: string,
    _options: { readonly mode: 'exclusive'; readonly ifAvailable: true },
    callback: (lock: { readonly name: string } | null) => Promise<void>,
  ): Promise<void> {
    if (this.#held.has(name)) {
      await callback(null)
      return
    }
    this.#held.add(name)
    try {
      await callback({ name })
    } finally {
      this.#held.delete(name)
      this.releaseCount += 1
    }
  }
}

interface MemoryCheckpointStore {
  readonly candidates: Map<string, FileCheckpointV2>
  readonly committed: Map<string, FileCheckpointV2>
  readonly handles: Map<string, PersistentHandleRecord>
}

export function memoryCheckpointFactory(onRetire?: () => void): FSAFileCheckpointRepositoryFactory {
  const stores = new Map<string, MemoryCheckpointStore>()
  return async (binding) => {
    let store = stores.get(binding.operationId)
    if (store === undefined) {
      store = { candidates: new Map(), committed: new Map(), handles: new Map() }
      stores.set(binding.operationId, store)
    }
    return new MemoryCheckpointRepository(binding, store, onRetire)
  }
}

class MemoryCheckpointRepository implements FSAFileCheckpointRepository {
  readonly binding: CheckpointNamespaceBinding
  readonly #store: MemoryCheckpointStore
  readonly #onRetire: (() => void) | undefined

  constructor(
    binding: CheckpointNamespaceBinding,
    store: MemoryCheckpointStore,
    onRetire?: () => void,
  ) {
    this.binding = binding
    this.#store = store
    this.#onRetire = onRetire
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

export class MemoryDirectory {
  readonly kind = 'directory' as const
  readonly name: string
  readonly #token = crypto.randomUUID()
  readonly #entries = new Map<string, MemoryDirectory | MemoryFile>()
  onFileCreated: (() => void) | undefined
  onRemoveEntry: ((name: string) => Promise<void>) | undefined

  constructor(name: string) {
    this.name = name
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return (other as MemoryDirectory).#token === this.#token
  }

  async queryPermission(): Promise<PermissionState> {
    return 'granted'
  }

  async requestPermission(): Promise<PermissionState> {
    return 'granted'
  }

  async getDirectoryHandle(
    name: string,
    options?: FileSystemGetDirectoryOptions,
  ): Promise<FileSystemDirectoryHandle> {
    const existing = this.#entries.get(name)
    if (existing instanceof MemoryDirectory) return existing as unknown as FileSystemDirectoryHandle
    if (existing !== undefined) throw domError('TypeMismatchError')
    if (options?.create !== true) throw domError('NotFoundError')
    const created = new MemoryDirectory(name)
    this.#entries.set(name, created)
    return created as unknown as FileSystemDirectoryHandle
  }

  async getFileHandle(
    name: string,
    options?: FileSystemGetFileOptions,
  ): Promise<FileSystemFileHandle> {
    const existing = this.#entries.get(name)
    if (existing instanceof MemoryFile) return existing as unknown as FileSystemFileHandle
    if (existing !== undefined) throw domError('TypeMismatchError')
    if (options?.create !== true) throw domError('NotFoundError')
    const created = new MemoryFile(name)
    this.#entries.set(name, created)
    this.onFileCreated?.()
    return created as unknown as FileSystemFileHandle
  }

  async removeEntry(name: string): Promise<void> {
    await this.onRemoveEntry?.(name)
    if (!this.#entries.delete(name)) throw domError('NotFoundError')
  }

  async *entries(): AsyncIterableIterator<[string, FileSystemHandle]> {
    for (const [name, handle] of [...this.#entries]) {
      yield [name, handle as unknown as FileSystemHandle]
    }
  }

  directoryNames(): string[] {
    return [...this.#entries.entries()]
      .filter((entry): entry is [string, MemoryDirectory] => entry[1] instanceof MemoryDirectory)
      .map(([name]) => name)
      .sort()
  }

  fileNames(): string[] {
    return [...this.#entries.entries()]
      .filter((entry): entry is [string, MemoryFile] => entry[1] instanceof MemoryFile)
      .map(([name]) => name)
      .sort()
  }

  entryNames(): string[] {
    return [...this.#entries.keys()].sort()
  }

  async fileBytes(name: string): Promise<Uint8Array> {
    const file = this.#entries.get(name)
    if (!(file instanceof MemoryFile)) throw new Error('memory file is missing')
    return file.bytes()
  }

  replaceFile(name: string, bytes: Uint8Array): MemoryFile {
    const file = new MemoryFile(name, Uint8Array.from(bytes))
    this.#entries.set(name, file)
    return file
  }
}

export class MemoryFile {
  readonly kind = 'file' as const
  readonly name: string
  readonly #token = crypto.randomUUID()
  #bytes: Uint8Array

  constructor(name: string, bytes = new Uint8Array()) {
    this.name = name
    this.#bytes = bytes.slice()
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other instanceof MemoryFile && other.#token === this.#token
  }

  async getFile(): Promise<File> {
    const copy = new Uint8Array(this.#bytes.byteLength)
    copy.set(this.#bytes)
    return new Blob([copy.buffer]) as File
  }

  async createWritable(
    options?: FileSystemCreateWritableOptions,
  ): Promise<FileSystemWritableFileStream> {
    if (options?.keepExistingData !== true) this.#bytes = new Uint8Array()
    return {
      write: async (data: FileSystemWriteChunkType) => {
        if (typeof data !== 'object' || data === null || !('type' in data) || data.type !== 'write') {
          throw new TypeError('memory writer requires positioned writes')
        }
        const command = data as WriteParams
        if (!(command.data instanceof Uint8Array)) {
          throw new TypeError('memory writer accepts Uint8Array writes')
        }
        const position = Number(command.position ?? 0)
        const source = command.data
        const next = new Uint8Array(Math.max(this.#bytes.byteLength, position + source.byteLength))
        next.set(this.#bytes)
        next.set(source, position)
        this.#bytes = next
      },
      close: async () => {},
    } as unknown as FileSystemWritableFileStream
  }

  async bytes(): Promise<Uint8Array> {
    return this.#bytes.slice()
  }
}

function domError(name: string): DOMException {
  return new DOMException(name, name)
}
