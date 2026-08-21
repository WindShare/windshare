import { vi } from 'vitest'

import { prepareFSAOperationBindingTransition } from '../../src/output/browser/indexeddb-root-binding'
import {
  createDirectTreePlan,
  createFSANamedEntryReservation,
  createOriginalFileArtifact,
  createReceiveIntent,
  createSelectionSpec,
  createSingleFileDirectoryTreeArtifact,
  createSyntheticSelectionResultRoot,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
  createZipArchiveArtifact,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import type { BrowserLockManagerRuntime } from '../../src/output/browser/session-lease'
import { openOriginPrivateWorkspaceNamespace } from '../../src/output/origin-private/namespace'
import type { OriginPrivateWorkspaceCleanupAuthority } from '../../src/output/origin-private/cleanup-port'
import type { OriginPrivateRetainedArtifactBackend } from '../../src/output/origin-private/session'
import { originPrivatePackageHandleId } from '../../src/output/origin-private/package-store'
import {
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  newFileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import { finalFileCheckpointProof } from '../../src/output/persistence/journal'
import { PersistedReceiveOperationReopenAuthority } from '../../src/output/resume/reopen-authority'
import { receiveOperationResumeDescriptor } from '../../src/output/resume/descriptor'
import {
  admitWorkspaceBudget,
  createPreparedZipWorkspaceBudget,
  createSingleFileWorkspaceBudget,
  type WorkspaceCapacitySnapshot,
} from '../../src/output/workspace/budget'
import {
  createRawWorkspaceReceipt,
  createPackageTemporaryCleanupReceipt,
  createPreparationAdmissionReceipt,
  createWorkspaceSealReceipt,
  persistedReceiptRecord,
} from '../../src/output/workspace/receipts'
import {
  createMaterializedManifestPages,
  materializedGenerationTableDigest,
  sealMaterializedManifest,
} from '../../src/output/workspace/manifest'
import { sealWorkspaceMaterialization } from '../../src/output/workspace/aggregate'
import {
  createPreparationManifestPages,
  sealWorkspaceZipPreparation,
} from '../../src/output/workspace/preparation'
import {
  RECEIVE_RECORD_LIFECYCLE_STATE,
  RECEIVE_RECORD_MATERIALIZED_MANIFEST,
  RECEIVE_RECORD_PREPARATION,
  RECEIVE_RECORD_SEALED_MATERIALIZATION,
  createPersistedReceiveRecord,
  operationRecordId,
  receiveOperationHandleRecord,
  type ManifestPageRecord,
  type PersistedReceiveRecord,
  type ReceiveOperationHandleRecord,
  type ReceiveOperationLeaseRecord,
  type ReceiveRecordKind,
} from '../../src/output/workspace/records'
import {
  prepareReceiveOperationTransition,
  type ReceiveOperationRepository,
  type ReceiveOperationTransition,
} from '../../src/output/workspace/repository'
import {
  decodeStoredReceiveLifecycleState,
  storedReceiveLifecycleState,
} from '../../src/output/workspace/state-codec'
import {
  STABLE_RETENTION_MILLISECONDS,
  type ReceiveLifecycleState,
} from '../../src/output/workspace/state'
import {
  WORKSPACE_HANDLE_ZIP_LAYOUT,
  workspaceZipLayoutHandleId,
} from '../../src/output/workspace/stages'
import { identity } from './planning/fixture'

const ENTERED_AT = 10_000
const WORKSPACE_CAPACITY = Object.freeze({
  jobLimitBytes: 1_000_000n,
  processLimitBytes: 2_000_000n,
  otherActiveJobPeakBytes: 0n,
  estimatedQuotaBytes: 3_000_000n,
  currentUsageBytes: 0n,
  minimumReserveBytes: 0n,
  verifiedAlreadyOwnedBytes: 0n,
}) satisfies WorkspaceCapacitySnapshot

export function retainedCleanupBackend(
  checkpointObservation: 'clean' | 'ownership-unknown',
): OriginPrivateRetainedArtifactBackend & { readonly close: ReturnType<typeof vi.fn> } {
  const close = vi.fn(async () => undefined)
  const cleanup: OriginPrivateWorkspaceCleanupAuthority = {
    removeOwnedObject: async () => Object.freeze({ kind: 'already-absent' }),
    removeFileCheckpoints: async () => checkpointObservation === 'clean'
      ? Object.freeze({ kind: 'clean' as const, removedRecordDigests: Object.freeze([]) })
      : Object.freeze({ kind: 'ownership-unknown' as const }),
    cleanupRequest: async () => Object.freeze({
      targets: Object.freeze([]),
      metadataHandleIds: Object.freeze([]),
      port: cleanup,
    }),
  }
  return Object.freeze({
    packagedArtifacts: {
      readPackagedArtifact: async () => new Blob(),
    },
    cleanup,
    close,
  })
}

export function reopenAuthority(
  state: MemoryRepositoryState,
  now: number,
  randomSeed: number,
): PersistedReceiveOperationReopenAuthority {
  return new PersistedReceiveOperationReopenAuthority({
    repositoryFactory: async () => new MemoryOperationRepository(state),
    clock: { now: () => now },
    leaseOptions: {
      manager: new MemoryLockManager(),
      randomBytes: bytesFilled(randomSeed),
    },
  })
}

export async function seedFSAOperationBinding(
  repository: ReceiveOperationRepository,
  intent: ReceiveIntent,
  parent: FileSystemDirectoryHandle,
): Promise<void> {
  const prepared = await prepareFSAOperationBindingTransition({ repository, intent, parent })
  await repository.commitTransition({
    operationId: intent.operationId,
    ...prepared.transition,
  })
}

export function requiredDescriptor(lifecycle: ReceiveLifecycleState, now: number) {
  const descriptor = receiveOperationResumeDescriptor(lifecycle, now)
  if (descriptor === undefined) throw new Error('lifecycle fixture has no descriptor')
  return descriptor
}

export function resumableReceive(intent: ReceiveIntent, generation: bigint): Extract<
  ReceiveLifecycleState,
  { kind: 'resumable-receive' }
> {
  return Object.freeze({
    kind: 'resumable-receive',
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    generation,
    checkpointSetDigest: identity(60, 32),
    completedFileCount: 1n,
    completedBytes: 16n,
    expiresAt: ENTERED_AT + STABLE_RETENTION_MILLISECONDS,
  })
}

export async function directTreeIntent(): Promise<ReceiveIntent> {
  const selection = await createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const artifact = await createSingleFileDirectoryTreeArtifact({
    fileId: identity(3),
    sourcePath: 'report.bin',
    outputName: 'report.bin',
  })
  const reservation = await createFSANamedEntryReservation({
    operationId: identity(4),
    reservationId: identity(5),
    artifact,
    authorityRef: identity(6, 32),
    logicalReservedName: 'report.bin',
    physicalName: 'report.bin',
    collisionIndex: 0,
  })
  return createReceiveIntent({
    selection,
    artifact,
    plan: await createDirectTreePlan(artifact, reservation),
  })
}

export async function seedWorkspaceAdmission(
  state: MemoryRepositoryState,
  intent: ReceiveIntent,
) {
  if (intent.plan.kind !== 'workspace-then-publish' ||
      intent.artifact.kind !== 'original-file') {
    throw new TypeError('workspace admission fixture requires an original-file workspace')
  }
  const budget = await createSingleFileWorkspaceBudget({
    receiveIntent: intent,
    fileId: intent.artifact.fileId,
    containingDirectoryId: identity(27),
    generation: identity(28),
    catalogSize: 5n,
    durableMetadataBytes: 32n,
  })
  const admission = admitWorkspaceBudget(budget, WORKSPACE_CAPACITY)
  if (admission.kind !== 'accepted') throw new Error('workspace admission fixture was rejected')
  const receipt = await createPreparationAdmissionReceipt({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    workspaceBudget: budget,
    contentRequestCountAtAdmission: 0n,
    jobLimitBytes: WORKSPACE_CAPACITY.jobLimitBytes,
    processLimitBytes: WORKSPACE_CAPACITY.processLimitBytes,
    estimatedQuotaBytes: WORKSPACE_CAPACITY.estimatedQuotaBytes,
    currentUsageBytes: WORKSPACE_CAPACITY.currentUsageBytes,
    minimumReserveBytes: WORKSPACE_CAPACITY.minimumReserveBytes,
    incrementalPhysicalPeakBytes: admission.incrementalPhysicalPeakBytes,
  })
  const record = await persistedReceiptRecord(receipt)
  state.records.set(record.id, record)
  return Object.freeze({ budget, receipt })
}

export async function seedWorkspaceZipAdmission(
  state: MemoryRepositoryState,
  intent: ReceiveIntent,
  preparation: Awaited<ReturnType<typeof sealWorkspaceZipPreparation>>,
) {
  const budget = await createPreparedZipWorkspaceBudget({
    receiveIntent: intent,
    preparation,
    durableMetadataBytes: preparation.manifest.canonicalMetadataBytes,
  })
  const admission = admitWorkspaceBudget(budget, WORKSPACE_CAPACITY)
  if (admission.kind !== 'accepted') throw new Error('workspace ZIP admission fixture was rejected')
  const receipt = await createPreparationAdmissionReceipt({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    preparationManifestDigest: preparation.manifest.digest,
    sealedZipLayoutDigest: preparation.zipLayout.digest,
    workspaceBudget: budget,
    contentRequestCountAtAdmission: 0n,
    jobLimitBytes: WORKSPACE_CAPACITY.jobLimitBytes,
    processLimitBytes: WORKSPACE_CAPACITY.processLimitBytes,
    estimatedQuotaBytes: WORKSPACE_CAPACITY.estimatedQuotaBytes,
    currentUsageBytes: WORKSPACE_CAPACITY.currentUsageBytes,
    minimumReserveBytes: WORKSPACE_CAPACITY.minimumReserveBytes,
    incrementalPhysicalPeakBytes: admission.incrementalPhysicalPeakBytes,
  })
  const records = await Promise.all([
    createPersistedReceiveRecord({
      operationId: intent.operationId,
      kind: RECEIVE_RECORD_PREPARATION,
      canonicalBytes: preparation.manifest.canonicalBytes,
    }),
    persistedReceiptRecord(receipt),
  ])
  for (const record of records) state.records.set(record.id, record)
  for (const page of await createPreparationManifestPages(preparation.manifest)) {
    state.pages.set(page.id, page)
  }
  if (intent.plan.kind !== 'workspace-then-publish') {
    throw new Error('workspace ZIP admission fixture lost its binding')
  }
  const handle = receiveOperationHandleRecord({
    id: workspaceZipLayoutHandleId(intent.operationId, preparation.manifest.preparationId),
    operationId: intent.operationId,
    kind: WORKSPACE_HANDLE_ZIP_LAYOUT,
    authorityRef: intent.plan.workspace.repositoryRef,
    handle: preparation.zipLayout,
  })
  state.handles.set(handle.id, handle)
  return Object.freeze({ budget, receipt })
}

export async function seedWorkspacePackage(state: MemoryRepositoryState, intent: ReceiveIntent) {
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind !== 'original-file') {
    throw new Error('package fixture requires an original-file workspace')
  }
  const admission = await seedWorkspaceAdmission(state, intent)
  const checkpoint = newFileCheckpointV2({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.workspace.digest,
    fileId: intent.artifact.fileId,
    fileRevision: identity(97),
    canonicalPath: [intent.artifact.suggestedName],
    exactSize: 5n,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
    authorityRef: intent.plan.workspace.repositoryRef,
    ownedObjectId: identity(98, 32),
    stateGeneration: 2n,
    checkpointGeneration: 3n,
    verifiedRanges: [{ start: 0n, end: 5n }],
    phase: FILE_CHECKPOINT_PHASE_ACTIVE,
    commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
  const proof = finalFileCheckpointProof(checkpoint)
  const manifest = await sealMaterializedManifest({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.workspace.digest,
    preparationBinding: { kind: 'absent' },
    generations: [],
    entries: [{
      kind: 'file',
      artifactPath: checkpoint.canonicalPath,
      fileId: checkpoint.fileId,
      fileRevision: checkpoint.fileRevision,
      exactSize: checkpoint.exactSize,
      ownedObjectId: checkpoint.ownedObjectId,
      checkpoint: {
        recordId: checkpoint.recordId,
        recordDigest: checkpoint.checksum,
        checkpointGeneration: checkpoint.checkpointGeneration,
      },
    }],
    checkpoints: { readFinalCheckpoint: async () => proof },
  })
  const rawReceipt = await createRawWorkspaceReceipt({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    workspaceBindingDigest: intent.plan.workspace.digest,
    materializedManifestDigest: manifest.digest,
    ownedObjects: [{ ownedObjectId: checkpoint.ownedObjectId, exactBytes: checkpoint.exactSize }],
  })
  const seal = await sealWorkspaceMaterialization({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    workspaceBindingDigest: intent.plan.workspace.digest,
    preparationBinding: { kind: 'absent' },
    materializedManifestDigest: manifest.digest,
    generationTableDigest: await materializedGenerationTableDigest(manifest.generations),
    artifactVersion: intent.artifact.version,
    layoutVersion: 1,
    rawWorkspaceReceiptDigest: rawReceipt.digest,
  })
  const sealReceipt = await createWorkspaceSealReceipt({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    workspaceBindingDigest: intent.plan.workspace.digest,
    sealedMaterializationDigest: seal.digest,
    rawWorkspaceReceipt: rawReceipt,
  })
  const temporaryOwnedObjectId = identity(99, 32)
  const cleanupReceipt = await createPackageTemporaryCleanupReceipt({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    sealedMaterializationDigest: seal.digest,
    packageOwnedObjectId: temporaryOwnedObjectId,
    packageHandleId: originPrivatePackageHandleId(intent.operationId, temporaryOwnedObjectId),
    cleanupResult: 'removed',
    cleanupProofDigest: identity(100, 32),
  })
  const records = await Promise.all([
    createPersistedReceiveRecord({
      operationId: intent.operationId,
      kind: RECEIVE_RECORD_MATERIALIZED_MANIFEST,
      canonicalBytes: manifest.canonicalBytes,
    }),
    createPersistedReceiveRecord({
      operationId: intent.operationId,
      kind: RECEIVE_RECORD_SEALED_MATERIALIZATION,
      canonicalBytes: seal.canonicalBytes,
    }),
    persistedReceiptRecord(sealReceipt),
    persistedReceiptRecord(cleanupReceipt),
  ])
  for (const record of records) state.records.set(record.id, record)
  for (const page of await createMaterializedManifestPages(manifest)) state.pages.set(page.id, page)
  const lifecycle = Object.freeze({
    kind: 'resumable-package' as const,
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    generation: 9n,
    sealedMaterializationDigest: seal.digest,
    tempCleanupProofDigest: cleanupReceipt.digest,
    expiresAt: ENTERED_AT + STABLE_RETENTION_MILLISECONDS,
  })
  await state.seedLifecycle(lifecycle)
  return Object.freeze({ admission, proof, lifecycle })
}

export async function workspaceIntent(): Promise<ReceiveIntent> {
  const artifact = await createOriginalFileArtifact({
    fileId: identity(21),
    sourcePath: 'root/file.bin',
    suggestedName: 'file.bin',
  })
  const workspace = await createWorkspaceBinding({
    operationId: identity(22),
    workspaceId: identity(23),
    artifact,
    repositoryRef: identity(24, 32),
  })
  return createReceiveIntent({
    selection: await createSelectionSpec({
      shareInstance: identity(25),
      syntheticRoot: identity(26),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    }),
    artifact,
    plan: await createWorkspaceThenPublishPlan(artifact, workspace),
  })
}

export async function workspaceZipIntent(): Promise<ReceiveIntent> {
  const artifact = await createZipArchiveArtifact(createSyntheticSelectionResultRoot())
  const workspace = await createWorkspaceBinding({
    operationId: identity(101),
    workspaceId: identity(102),
    artifact,
    repositoryRef: identity(103, 32),
  })
  return createReceiveIntent({
    selection: await createSelectionSpec({
      shareInstance: identity(104),
      syntheticRoot: identity(105),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    }),
    artifact,
    plan: await createWorkspaceThenPublishPlan(artifact, workspace),
  })
}

export function workspaceZipPreparationInput(intent: ReceiveIntent) {
  if (intent.artifact.kind !== 'zip-archive') throw new Error('ZIP fixture lost its artifact')
  const directoryId = intent.selection.syntheticRoot
  const generation = identity(106)
  return {
    receiveIntent: intent,
    preparationId: identity(107),
    generations: [{ directoryId, generation }],
    entries: [
      {
        kind: 'directory' as const,
        sourcePath: [],
        artifactPath: [intent.artifact.layout.name],
        directoryId,
        generation,
        role: 'result-root' as const,
      },
      {
        kind: 'file' as const,
        sourcePath: ['file.bin'],
        artifactPath: [intent.artifact.layout.name, 'file.bin'],
        fileId: identity(108),
        containingDirectoryId: directoryId,
        generation,
        exactSize: 5n,
      },
    ],
  }
}

export class MemoryRepositoryState {
  readonly records = new Map<string, PersistedReceiveRecord>()
  readonly pages = new Map<string, ManifestPageRecord>()
  readonly handles = new Map<string, ReceiveOperationHandleRecord>()
  lease: ReceiveOperationLeaseRecord | undefined

  async seedLifecycle(lifecycle: ReceiveLifecycleState): Promise<void> {
    this.records.set(
      operationRecordId(lifecycle.operationId, RECEIVE_RECORD_LIFECYCLE_STATE),
      await storedReceiveLifecycleState(lifecycle),
    )
  }

  async lifecycle(): Promise<ReceiveLifecycleState> {
    const record = [...this.records.values()].find(
      (candidate) => candidate.kind === RECEIVE_RECORD_LIFECYCLE_STATE,
    )
    if (record === undefined) throw new Error('lifecycle fixture is missing')
    return decodeStoredReceiveLifecycleState(record)
  }
}

export class MemoryOperationRepository implements ReceiveOperationRepository {
  readonly #state: MemoryRepositoryState
  #closed = false

  constructor(state: MemoryRepositoryState) {
    this.#state = state
  }

  async commitTransition(transition: ReceiveOperationTransition): Promise<void> {
    this.#requireOpen()
    const prepared = await prepareReceiveOperationTransition(transition)
    const lifecycle = await this.#lifecycle(prepared.operationId)
    if (prepared.expectedLifecycleGeneration !== undefined &&
        lifecycle?.generation !== prepared.expectedLifecycleGeneration) {
      throw new DOMException('receive lifecycle changed concurrently', 'InvalidStateError')
    }
    if (prepared.expectedLeaseId !== undefined &&
        this.#state.lease?.leaseId !== prepared.expectedLeaseId) {
      throw new DOMException('receive lease changed concurrently', 'InvalidStateError')
    }
    if (prepared.lease?.kind === 'put' && prepared.expectedLeaseId === undefined &&
        this.#state.lease !== undefined) {
      throw new DOMException('receive lease already exists', 'InvalidStateError')
    }
    for (const record of prepared.records) {
      const existing = this.#state.records.get(record.id)
      if (record.kind !== RECEIVE_RECORD_LIFECYCLE_STATE && existing !== undefined &&
          (existing.digest !== record.digest || !sameBytes(existing.canonicalBytes, record.canonicalBytes))) {
        throw new DOMException('immutable receive record changed', 'DataError')
      }
    }
    for (const id of prepared.deleteRecordIds) this.#state.records.delete(id)
    for (const id of prepared.deleteManifestPageIds) this.#state.pages.delete(id)
    for (const id of prepared.deleteHandleIds) this.#state.handles.delete(id)
    for (const record of prepared.records) this.#state.records.set(record.id, record)
    for (const page of prepared.manifestPages) this.#state.pages.set(page.id, page)
    for (const handle of prepared.handles) this.#state.handles.set(handle.id, handle)
    if (prepared.lease?.kind === 'put') this.#state.lease = prepared.lease.record
    if (prepared.lease?.kind === 'delete') this.#state.lease = undefined
  }

  async readRecord(id: string): Promise<PersistedReceiveRecord | undefined> {
    this.#requireOpen()
    return this.#state.records.get(id)
  }

  readLifecycle(operationId: string): Promise<PersistedReceiveRecord | undefined> {
    return this.readRecord(operationRecordId(operationId, RECEIVE_RECORD_LIFECYCLE_STATE))
  }

  async listRecords(
    operationId: string,
    kind?: ReceiveRecordKind,
  ): Promise<readonly PersistedReceiveRecord[]> {
    this.#requireOpen()
    return [...this.#state.records.values()].filter((record) =>
      record.operationId === operationId && (kind === undefined || record.kind === kind))
  }

  async listManifestPages(
    operationId: string,
    kind?: ReceiveRecordKind,
  ): Promise<readonly ManifestPageRecord[]> {
    this.#requireOpen()
    return [...this.#state.pages.values()].filter((page) =>
      page.operationId === operationId && (kind === undefined || page.kind === kind))
  }

  async readHandle<T = unknown>(
    id: string,
  ): Promise<ReceiveOperationHandleRecord<T> | undefined> {
    this.#requireOpen()
    return this.#state.handles.get(id) as ReceiveOperationHandleRecord<T> | undefined
  }

  async readLease(operationId: string): Promise<ReceiveOperationLeaseRecord | undefined> {
    this.#requireOpen()
    return this.#state.lease?.operationId === operationId ? this.#state.lease : undefined
  }

  close(): void {
    this.#closed = true
  }

  async #lifecycle(operationId: string): Promise<ReceiveLifecycleState | undefined> {
    const record = this.#state.records.get(
      operationRecordId(operationId, RECEIVE_RECORD_LIFECYCLE_STATE),
    )
    return record === undefined ? undefined : decodeStoredReceiveLifecycleState(record)
  }

  #requireOpen(): void {
    if (this.#closed) throw new DOMException('memory repository is closed', 'InvalidStateError')
  }
}

export class MemoryLockManager implements BrowserLockManagerRuntime {
  readonly #held = new Set<string>()

  async request(
    name: string,
    _options: { readonly mode: 'exclusive'; readonly ifAvailable?: true },
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
    }
  }
}

type MemoryFileSystemEntry = MemoryDirectoryHandle | MemoryFileHandle

export class MemoryDirectoryHandle {
  readonly kind = 'directory' as const
  readonly name: string
  readonly #entries = new Map<string, MemoryFileSystemEntry>()
  creationCount = 0

  constructor(name: string) {
    this.name = name
  }

  asHandle(): FileSystemDirectoryHandle {
    return this as unknown as FileSystemDirectoryHandle
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other === this
  }

  async getDirectoryHandle(
    name: string,
    options?: FileSystemGetDirectoryOptions,
  ): Promise<FileSystemDirectoryHandle> {
    const existing = this.#entries.get(name)
    if (existing instanceof MemoryDirectoryHandle) return existing.asHandle()
    if (existing !== undefined) {
      throw new DOMException('entry is not a directory', 'TypeMismatchError')
    }
    if (options?.create !== true) {
      throw new DOMException('directory is absent', 'NotFoundError')
    }
    const directory = new MemoryDirectoryHandle(name)
    this.#entries.set(name, directory)
    this.creationCount += 1
    return directory.asHandle()
  }

  async getFileHandle(
    name: string,
    options?: FileSystemGetFileOptions,
  ): Promise<FileSystemFileHandle> {
    const existing = this.#entries.get(name)
    if (existing instanceof MemoryFileHandle) return existing.asHandle()
    if (existing !== undefined) {
      throw new DOMException('entry is not a file', 'TypeMismatchError')
    }
    if (options?.create !== true) {
      throw new DOMException('file is absent', 'NotFoundError')
    }
    const file = new MemoryFileHandle(name)
    this.#entries.set(name, file)
    return file.asHandle()
  }

  async removeEntry(name: string, options?: FileSystemRemoveOptions): Promise<void> {
    const existing = this.#entries.get(name)
    if (existing === undefined) {
      throw new DOMException('entry is absent', 'NotFoundError')
    }
    if (existing instanceof MemoryDirectoryHandle &&
        existing.#entries.size !== 0 &&
        options?.recursive !== true) {
      throw new DOMException('directory is not empty', 'InvalidModificationError')
    }
    this.#entries.delete(name)
  }
}

class MemoryFileHandle {
  readonly kind = 'file' as const
  readonly name: string
  #bytes = new Uint8Array()

  constructor(name: string) {
    this.name = name
  }

  asHandle(): FileSystemFileHandle {
    return this as unknown as FileSystemFileHandle
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other === this
  }

  async getFile(): Promise<File> {
    const copy = this.#bytes.slice()
    return new Blob([copy.buffer]) as File
  }

  async createWritable(
    options?: FileSystemCreateWritableOptions,
  ): Promise<FileSystemWritableFileStream> {
    let staged = options?.keepExistingData === true ? this.#bytes.slice() : new Uint8Array()
    let position = 0
    let open = true
    const requireOpen = () => {
      if (!open) throw new DOMException('writable is closed', 'InvalidStateError')
    }
    const writeAt = (offset: number, bytes: Uint8Array) => {
      const length = Math.max(staged.byteLength, offset + bytes.byteLength)
      const next = new Uint8Array(length)
      next.set(staged)
      next.set(bytes, offset)
      staged = next
      position = offset + bytes.byteLength
    }
    const write = async (chunk: FileSystemWriteChunkType): Promise<void> => {
      requireOpen()
      if (!isWriteCommand(chunk)) {
        writeAt(position, await memoryWriteBytes(chunk))
        return
      }
      if (chunk.type === 'write') {
        if (chunk.data === undefined || chunk.data === null) {
          throw new TypeError('write command requires data')
        }
        const offset = chunk.position === undefined || chunk.position === null
          ? position
          : requireFileOffset(chunk.position)
        writeAt(offset, await memoryWriteBytes(chunk.data))
        return
      }
      if (chunk.type === 'seek') {
        if (chunk.position === undefined || chunk.position === null) {
          throw new TypeError('seek command requires a position')
        }
        position = requireFileOffset(chunk.position)
        return
      }
      if (chunk.size === undefined || chunk.size === null) {
        throw new TypeError('truncate command requires a size')
      }
      const size = requireFileOffset(chunk.size)
      const resized = new Uint8Array(size)
      resized.set(staged.subarray(0, size))
      staged = resized
      position = Math.min(position, size)
    }
    return {
      write,
      close: async () => {
        requireOpen()
        this.#bytes = staged
        open = false
      },
      abort: async () => {
        requireOpen()
        open = false
      },
    } as unknown as FileSystemWritableFileStream
  }
}

function isWriteCommand(
  chunk: FileSystemWriteChunkType,
): chunk is WriteParams {
  return typeof chunk === 'object' && chunk !== null &&
    'type' in chunk &&
    (chunk.type === 'write' || chunk.type === 'seek' || chunk.type === 'truncate')
}

async function memoryWriteBytes(
  data: BufferSource | Blob | string,
): Promise<Uint8Array> {
  if (typeof data === 'string') return new TextEncoder().encode(data)
  if (data instanceof Blob) return new Uint8Array(await data.arrayBuffer())
  if (data instanceof ArrayBuffer) return new Uint8Array(data.slice(0))
  if (ArrayBuffer.isView(data)) {
    return new Uint8Array(data.buffer, data.byteOffset, data.byteLength).slice()
  }
  throw new TypeError('unsupported memory file write')
}

function requireFileOffset(value: number): number {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new DOMException('file offset is invalid', 'TypeError')
  }
  return value
}

export function memoryStorage(root: MemoryDirectoryHandle): NonNullable<
  Parameters<typeof openOriginPrivateWorkspaceNamespace>[0]['storage']
> {
  return Object.freeze({
    getDirectory: async () => root.asHandle(),
  }) as NonNullable<Parameters<typeof openOriginPrivateWorkspaceNamespace>[0]['storage']>
}

export function bytesFilled(seed: number): (length: number) => Uint8Array {
  return (length) => new Uint8Array(length).fill(seed)
}

export function sameBytes(left: Uint8Array, right: Uint8Array): boolean {
  return left.byteLength === right.byteLength && left.every((byte, index) => byte === right[index])
}
