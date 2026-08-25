import type { V2BlockRangeReader } from '../../src/content/v2-broker'
import {
  IndexedDbFileCheckpointRepository,
  IndexedDbReceiveOperationRepository,
} from '../../src/output/browser/indexeddb-repository'
import { IndexedDbCompatibleNameLedger } from '../../src/output/browser/indexeddb-compatible-name-ledger'
import {
  prepareFSAOperationBindingTransition,
  verifyFSAOperationBinding,
} from '../../src/output/browser/indexeddb-root-binding'
import { acquireFSARootMutationLease } from '../../src/output/browser/namespace-mutation'
import { acquireBrowserReceiveOperationLease } from '../../src/output/browser/session-lease'
import { fsaParentOffer } from '../../src/output/capability/acquisition'
import type { AcquiredFSAParentAuthority } from '../../src/output/capability/contract'
import {
  assembleNewFileSystemAccessOutput,
  reopenFileSystemAccessOutput,
  reserveNewFileSystemAccessOutput,
  type FileSystemAccessOutputSession,
} from '../../src/output/file-system-access/session'
import { createFileSystemAccessSettlementAuthority } from '../../src/output/file-system-access/settlement'
import { fsaCheckpointSetDigest, type DirectTreeIntent } from '../../src/output/file-system-access/settlement-proof'
import {
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  fileCheckpointIsComplete,
  type FileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import { durableCheckpointNamespaceIdentity } from '../../src/output/persistence/namespace'
import { scanAllFSAFileCheckpoints } from '../../src/output/file-system-access/checkpoint-repository'
import type { PersistentMaterializationPort } from '../../src/output/persistent-tree/contracts'
import { reduceReceiveLifecycle } from '../../src/output/workspace/lifecycle'
import { recoverAbandonedOperation } from '../../src/output/workspace/recovery'
import { decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import {
  initialReceiveLifecycleState,
  type ReceiveLifecycleState,
} from '../../src/output/workspace/state'
import {
  createDirectTreePlan,
  createReceiveIntent,
  createResultRootDirectoryTreeArtifact,
  createSelectionSpec,
  createSyntheticSelectionResultRoot,
  deriveArtifactChoiceIdentity,
  type DirectoryTreeArtifact,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import { snapshotMaterializationDirectory } from '../../src/transfer/directory-admission'
import {
  createDirectTreeCoordinateContract,
  type DirectTreeCoordinateContract,
  type DirectTreeFileProjection,
} from '../../src/transfer/job/coordinate/direct-tree'
import type { PendingFile } from '../../src/transfer/job/contract'
import { transferV2File } from '../../src/transfer/job/file-transfer'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  transferWorkerSettlement,
} from '../../src/transfer/outcome'
import {
  outputExecutionProfile,
  outputSessionIdentity,
  snapshotDirectoryMaterializationRequest,
  type BeginOutputFileResult,
  type DirectTreeExecution,
  type OutputFileRequest,
  type OutputSession,
} from '../../src/transfer/output-session'
import { createPersistentDirectTreeExecution } from '../../src/transfer/settlement/persistent-execution'
import {
  digestIdentity,
  fileEntry,
  identity,
  identityText,
  readerFixture,
} from '../transfer/v2-job-fixture'

const ACTIVE_SIGNAL = new AbortController().signal
const INITIAL_TIME_MILLISECONDS = 1_000
const RECOVERY_TIME_MILLISECONDS = 2_000
const PENDING_SMALL_NAME = 'pending-small.bin'
const COMPLETED_NAME = 'completed.bin'
const LARGE_NAME = 'large.bin'
const PENDING_SMALL_SIZE = 4n
const COMPLETED_SIZE = 4n
const LARGE_SIZE = 8n
const LARGE_DURABLE_PREFIX_BYTES = 4
const PENDING_ACCEPTED_BYTES = 2
const FILE_CONTENT_BYTE = 7
const OUTPUT_SESSION_ID = identityText(210)
const TRANSFER_JOB_ID = identityText(220)
const EXPIRY_RECEIPT_DIGEST = digestIdentity(222)
const RECOVERY_BUDGET_BYTES = 1_024n
const CHECKPOINT_TRIGGER_BYTES = 1_024n
const CHECKPOINT_TRIGGER_MILLISECONDS = 60_000
const OUTPUT_SETTLEMENT_TIMEOUT_MILLISECONDS = 20_000
const OUTPUT_IDENTITY = outputSessionIdentity({
  backend: 'fsa-direct-tree-periodic',
  outputSessionId: OUTPUT_SESSION_ID,
})

const FILES = Object.freeze([
  fileEntry(identity(81), COMPLETED_NAME, COMPLETED_SIZE),
  fileEntry(identity(82), LARGE_NAME, LARGE_SIZE),
  fileEntry(identity(83), PENDING_SMALL_NAME, PENDING_SMALL_SIZE),
])

interface FsaNamespaceFixture {
  readonly databaseName: string
  readonly parentName: string
}

export interface DirectTreeProcessRecoveryFixture {
  readonly databaseName: string
  readonly parentName: string
  readonly intent: ReceiveIntent
}

export interface DirectTreeProcessCrashCut {
  readonly fixture: DirectTreeProcessRecoveryFixture
  readonly storageEvidence: StorageEvidence
  readonly lifecycle: string
  readonly pendingAcceptedRange: string
  readonly checkpoints: readonly CheckpointSnapshot[]
}

export interface DirectTreeProcessRecoveryProof {
  readonly storageEvidence: StorageEvidence
  readonly lifecycle: Readonly<{
    readonly beforeRecovery: string
    readonly recoveryDecision: string
    readonly afterRecovery: string
    readonly afterResume: string
    readonly afterSettlement: string
    readonly durableAfterSettlement: string
  }>
  readonly workerStatus: string
  readonly revisionRequests: readonly string[]
  readonly durableRangesBeforeFetch: readonly FileRangeSnapshot[]
  readonly contentRangeRequests: readonly ContentRangeRequest[]
  readonly physicalFiles: readonly PhysicalFileSnapshot[]
}

interface StorageEvidence {
  readonly backingStore: 'opfs'
  readonly role: 'direct-tree-contract-surrogate'
  readonly nativeNtfsPerformanceClaim: false
}

interface CheckpointSnapshot {
  readonly file: string
  readonly ranges: readonly string[]
  readonly complete: boolean
}

interface FileRangeSnapshot {
  readonly file: string
  readonly ranges: readonly string[]
}

interface ContentRangeRequest {
  readonly file: string
  readonly range: string
}

interface PhysicalFileSnapshot {
  readonly file: string
  readonly bytes: readonly number[]
}

const STORAGE_EVIDENCE: StorageEvidence = Object.freeze({
  backingStore: 'opfs',
  role: 'direct-tree-contract-surrogate',
  nativeNtfsPerformanceClaim: false,
})

export async function createDirectTreeProcessCrashCut(
  key: string,
): Promise<DirectTreeProcessCrashCut> {
  const databaseName = `windshare-direct-tree-process-${key}`
  const parentName = `direct-tree-process-${key}`
  const namespaceFixture: FsaNamespaceFixture = Object.freeze({ databaseName, parentName })
  await resetFixture(namespaceFixture)

  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  const parent = await parentDirectory(parentName)
  const session = await bindTask(
    namespaceFixture,
    parent,
    repository,
    await recoveryArtifact(),
    151,
  )
  const lease = await acquireBrowserReceiveOperationLease(
    repository,
    session.intent.operationId,
    {
      clock: { now: () => INITIAL_TIME_MILLISECONDS },
      randomBytes: () => new Uint8Array(16).fill(0x51),
    },
  )
  const lifecycle = await startReceiving(repository, session.intent, lease.leaseId)

  const completed = await beginFixtureFile(session, COMPLETED_NAME)
  await completed.writeRange(0n, filledBytes(Number(COMPLETED_SIZE)), ACTIVE_SIGNAL)
  await completed.commit(ACTIVE_SIGNAL)

  const large = await beginFixtureFile(session, LARGE_NAME)
  await large.writeRange(
    0n,
    filledBytes(LARGE_DURABLE_PREFIX_BYTES),
    ACTIVE_SIGNAL,
  )
  await large.checkpoint(ACTIVE_SIGNAL)

  const pendingSmall = await beginFixtureFile(session, PENDING_SMALL_NAME)
  await pendingSmall.writeRange(
    0n,
    filledBytes(PENDING_ACCEPTED_BYTES),
    ACTIVE_SIGNAL,
  )

  const checkpoints = await readCheckpointSnapshots(session.intent, databaseName)
  ;(globalThis as Record<string, unknown>).__windshareDirectTreeProcessCrashCut = {
    repository,
    session,
    lease,
    completed,
    large,
    pendingSmall,
  }
  return Object.freeze({
    fixture: Object.freeze({
      databaseName,
      parentName,
      intent: session.intent,
    }),
    storageEvidence: STORAGE_EVIDENCE,
    lifecycle: lifecycle.kind,
    pendingAcceptedRange: `0:${PENDING_ACCEPTED_BYTES}`,
    checkpoints,
  })
}

export async function recoverDirectTreeAfterProcessTermination(
  fixture: DirectTreeProcessRecoveryFixture,
): Promise<DirectTreeProcessRecoveryProof> {
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const lease = await acquireBrowserReceiveOperationLease(
    repository,
    fixture.intent.operationId,
    {
      clock: { now: () => RECOVERY_TIME_MILLISECONDS },
      randomBytes: () => new Uint8Array(16).fill(0x61),
    },
  )
  let session: FileSystemAccessOutputSession | undefined
  try {
    const beforeRecovery = await readLifecycle(repository, fixture.intent.operationId)
    const checkpoints = await readCheckpoints(fixture.intent, fixture.databaseName)
    const checkpointSetDigest = await fsaCheckpointSetDigest(
      requireDirectTreeIntent(fixture.intent),
      checkpoints,
    )
    const lastVerified = checkpoints.at(-1)
    if (lastVerified === undefined) {
      throw new TypeError('DirectTree process recovery lacks checkpoint evidence')
    }
    const recovery = recoverAbandonedOperation(beforeRecovery, {
      kind: 'verified-receive',
      checkpointSetDigest,
      completedFileCount: 1n,
      completedBytes: COMPLETED_SIZE,
      lastVerifiedRecordDigest: lastVerified.checksum,
    }, {
      planKind: 'direct-tree',
      nowMilliseconds: RECOVERY_TIME_MILLISECONDS,
      expiryReceiptDigest: EXPIRY_RECEIPT_DIGEST,
    })
    await repository.commitTransition({
      operationId: fixture.intent.operationId,
      expectedLifecycleGeneration: beforeRecovery.generation,
      expectedLeaseId: lease.leaseId,
      lifecycle: recovery.state,
    })
    const resumed = resumeReceiving(recovery.state, lease.leaseId)
    await repository.commitTransition({
      operationId: fixture.intent.operationId,
      expectedLifecycleGeneration: recovery.state.generation,
      expectedLeaseId: lease.leaseId,
      lifecycle: resumed,
    })

    const reopenedSession = await reopenFileSystemAccessOutput({
      intent: fixture.intent,
      operationRepository: repository,
      databaseName: fixture.databaseName,
    })
    session = reopenedSession
    const durableRangesBeforeFetch: FileRangeSnapshot[] = []
    const rangeRequests: ContentRangeRequest[] = []
    const readers = readerFixture(FILES)
    const pathsByFileId = new Map(FILES.map(file => [file.idText, file.name]))
    const settlement = await createFileSystemAccessSettlementAuthority({
      intent: fixture.intent,
      repository,
      lifecycleLeaseId: lease.leaseId,
      transferJobId: TRANSFER_JOB_ID,
      clock: () => RECOVERY_TIME_MILLISECONDS,
    })
    const persistentExecution = await createPersistentDirectTreeExecution({
      intent: requireDirectTreeIntent(fixture.intent),
      materialization: confirmedRecoveryMaterialization(reopenedSession),
      executionProfile: recoveryExecutionProfile(),
      namespaceClaims: reopenedSession,
      repairSummary: () => reopenedSession.repairSummary(),
      outputIdentity: OUTPUT_IDENTITY,
      settlement: settlement.bindMaterialization(reopenedSession),
    })
    const execution = observeInitialDurableRanges(
      persistentExecution,
      durableRangesBeforeFetch,
    )
    const coordinates = await createDirectTreeCoordinateContract(fixture.intent)
    const generation = identityText(90)
    const parent = await admitRecoveryRoot(execution, coordinates, generation)
    const descriptor = {
      shareInstance: identity(1),
      syntheticRoot: identity(2),
      syntheticRootId: identityText(2),
      chunkSize: 2,
    } as never
    const broker = observeRangeRequests(readers.broker, pathsByFileId, rangeRequests)
    for (const file of FILES) {
      await transferV2File({
        descriptor,
        revisions: readers.revisions,
        broker,
        output: execution.output,
        directTreeCoordinates: coordinates,
        signal: ACTIVE_SIGNAL,
        outputSettlementTimeoutMilliseconds: OUTPUT_SETTLEMENT_TIMEOUT_MILLISECONDS,
        onWriteAcknowledged: () => undefined,
        onRecoverableAcknowledged: () => undefined,
        onComplete: () => undefined,
        now: () => RECOVERY_TIME_MILLISECONDS,
      }, recoveryPendingFile(file, coordinates.projectFile([file.name]), parent))
    }
    const directorySettlement = await execution.directories.finalizeDirectory(
      parent.admission,
      ACTIVE_SIGNAL,
    )
    if (directorySettlement.kind !== 'finalized') {
      throw new Error('DirectTree recovery root did not finalize')
    }
    const worker = transferWorkerSettlement('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
    execution.beginTerminal('settle')
    const resultLifecycle = await execution.settle({
      transferJobId: TRANSFER_JOB_ID,
      worker,
      materialization: {
        entryCount: BigInt(FILES.length),
        fileCount: BigInt(FILES.length),
        directoryCount: 0n,
        rawBytes: FILES.reduce((total, file) => total + file.expectedSize, 0n),
      },
    }, ACTIVE_SIGNAL)
    const durableAfterSettlement = await readLifecycle(
      repository,
      fixture.intent.operationId,
    )
    const physicalFiles = await readPhysicalFiles(
      fixture.parentName,
      reopenedSession.reservation.physicalName,
    )

    return Object.freeze({
      storageEvidence: STORAGE_EVIDENCE,
      lifecycle: Object.freeze({
        beforeRecovery: beforeRecovery.kind,
        recoveryDecision: recovery.decision,
        afterRecovery: recovery.state.kind,
        afterResume: resumed.kind,
        afterSettlement: resultLifecycle.kind,
        durableAfterSettlement: durableAfterSettlement.kind,
      }),
      workerStatus: worker.status,
      revisionRequests: Object.freeze(readers.revisionRequests
        .map(fileId => requireFileName(pathsByFileId, fileId))
        .sort()),
      durableRangesBeforeFetch: Object.freeze(sortFileRanges(durableRangesBeforeFetch)),
      contentRangeRequests: Object.freeze(sortContentRequests(rangeRequests)),
      physicalFiles,
    })
  } finally {
    await session?.close().catch(() => undefined)
    await lease.release().catch(() => undefined)
    repository.close()
    await resetFixture({
      databaseName: fixture.databaseName,
      parentName: fixture.parentName,
    })
  }
}

async function bindTask(
  fixture: FsaNamespaceFixture,
  parent: FileSystemDirectoryHandle,
  repository: IndexedDbReceiveOperationRepository,
  artifact: DirectoryTreeArtifact,
  seed: number,
): Promise<FileSystemAccessOutputSession> {
  const selection = await createSelectionSpec({
    shareInstance: identityText(1),
    syntheticRoot: identityText(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const authority = acquiredParent(parent)
  const rootLease = await acquireFSARootMutationLease(parent)
  const reserved = await reserveNewFileSystemAccessOutput({
    authority,
    artifact,
    rootLease,
    operationId: identityText(seed),
    reservationId: identityText(seed + 1),
    authorityRef: digestIdentity(seed + 2),
  })
  if (reserved.compatibleNameRepair !== undefined) {
    throw new TypeError('OPFS DirectTree recovery unexpectedly required compatible-name repair')
  }
  const intent = await createReceiveIntent({
    selection,
    artifact,
    plan: await createDirectTreePlan(artifact, reserved.reservation),
  })
  const prepared = await prepareFSAOperationBindingTransition({
    repository,
    intent,
    parent,
    preClickRanking: [(await deriveArtifactChoiceIdentity(intent.artifact, intent.plan)).id],
  })
  await repository.commitTransition({
    operationId: intent.operationId,
    ...prepared.transition,
  })
  const binding = await verifyFSAOperationBinding({
    repository,
    intent,
    expectedParent: parent,
  })
  const session = await assembleNewFileSystemAccessOutput({
    binding,
    operationRepository: repository,
    rootLease,
    databaseName: fixture.databaseName,
    checkpointRepositoryFactory: checkpointBinding =>
      IndexedDbFileCheckpointRepository.open(checkpointBinding, fixture.databaseName),
    openCompatibleNameLedger: () => IndexedDbCompatibleNameLedger.open(fixture.databaseName),
    compatibleNamePreparation: { platform: 'windows', randomBits: () => 0 },
  })
  await session.activate()
  return session
}

function acquiredParent(
  parent: FileSystemDirectoryHandle,
): AcquiredFSAParentAuthority {
  const offer = fsaParentOffer()
  return Object.freeze({
    kind: 'fsa-parent-directory-authority',
    targetRouteId: offer.routeId,
    offer,
    parent,
  })
}

async function beginFixtureFile(
  session: FileSystemAccessOutputSession,
  name: string,
) {
  const file = FILES.find(candidate => candidate.name === name)
  if (file === undefined) throw new TypeError(`unknown DirectTree recovery file ${name}`)
  const revisionSeed = (file.id[0] ?? 1) + 40
  return session.beginFile({
    materializationRelativePath: [name],
    shareInstance: session.intent.shareInstance,
    outputSession: OUTPUT_IDENTITY,
    openRevision: async () => Object.freeze({
      fileId: file.idText,
      fileRevision: identityText(revisionSeed),
      exactSize: file.expectedSize,
    }),
  })
}

function recoveryExecutionProfile() {
  return outputExecutionProfile({
    maximumConcurrentFilePipelines: FILES.length,
    maximumOutstandingWriteBytes: RECOVERY_BUDGET_BYTES,
    maximumBufferedBytes: RECOVERY_BUDGET_BYTES,
    automaticCheckpoint: {
      kind: 'bounded',
      trigger: {
        pendingBytes: CHECKPOINT_TRIGGER_BYTES,
        pendingMilliseconds: CHECKPOINT_TRIGGER_MILLISECONDS,
      },
      costBudget: {
        maximumPrefixCopyBytes: RECOVERY_BUDGET_BYTES,
        maximumCumulativeWriteAmplificationBytes: RECOVERY_BUDGET_BYTES,
        maximumPeakTemporaryBytes: RECOVERY_BUDGET_BYTES,
      },
    },
  })
}

type PersistentBeginFileRequest =
  Parameters<PersistentMaterializationPort['beginFile']>[0]
type PersistentDirectoryPath =
  Parameters<PersistentMaterializationPort['ensureDirectory']>[0]
type PersistentDirectoryRequest =
  Parameters<NonNullable<PersistentMaterializationPort['materializeDirectory']>>[0]
type PersistentDirectoryFinalizationParameters =
  Parameters<NonNullable<PersistentMaterializationPort['finalizeDirectory']>>

function confirmedRecoveryMaterialization(
  session: FileSystemAccessOutputSession,
): PersistentMaterializationPort {
  return Object.freeze({
    beginFile: (request: PersistentBeginFileRequest) => {
      if (request.recovery?.kind !== 'preserve') return session.beginFile(request)
      return session.beginFile({
        ...request,
        recovery: Object.freeze({
          ...request.recovery,
          confirmTemporarySpace: () => true,
        }),
      })
    },
    ensureDirectory: (path: PersistentDirectoryPath) => session.ensureDirectory(path),
    materializeDirectory: (request: PersistentDirectoryRequest) =>
      session.materializeDirectory(request),
    finalizeDirectory: (
      admission: PersistentDirectoryFinalizationParameters[0],
      outcome: PersistentDirectoryFinalizationParameters[1],
    ) => session.finalizeDirectory(admission, outcome),
    closeForTerminalSettlement: () => session.closeForTerminalSettlement(),
    close: () => session.close(),
  })
}

async function admitRecoveryRoot(
  execution: DirectTreeExecution,
  coordinates: DirectTreeCoordinateContract,
  generation: string,
): Promise<Extract<PendingFile['parent'], { kind: 'materialized' }>> {
  const projection = coordinates.projectDirectory([])
  if (projection.kind !== 'materialize') {
    throw new TypeError('DirectTree recovery root is not materialized')
  }
  const directory = snapshotMaterializationDirectory({
    directoryId: coordinates.intent.syntheticRoot,
    generation,
    path: projection.relativePath,
  })
  const admission = await execution.directories.admitDirectory(
    snapshotDirectoryMaterializationRequest({
      directory,
      sourceAuthenticationPath: projection.sourceAuthenticationPath,
      logicalArtifactPath: projection.logicalArtifactPath,
      logicalSiblingMembership: {
        directoryId: coordinates.intent.syntheticRoot,
        generation,
        hasCommittedName: async candidate =>
          FILES.some(file => file.name === candidate),
      },
    }),
    ACTIVE_SIGNAL,
  )
  return Object.freeze({
    kind: 'materialized',
    directoryId: coordinates.intent.syntheticRoot,
    generation,
    sourceAuthenticationPath: projection.sourceAuthenticationPath,
    logicalArtifactPath: projection.logicalArtifactPath,
    materializationRelativePath: projection.relativePath,
    admission,
  })
}

function recoveryPendingFile(
  entry: typeof FILES[number],
  projection: DirectTreeFileProjection,
  parent: Extract<PendingFile['parent'], { kind: 'materialized' }>,
): PendingFile {
  return Object.freeze({
    entry,
    sourceAuthenticationPath: projection.sourceAuthenticationPath,
    logicalArtifactPath: projection.logicalArtifactPath,
    materializationRelativePath: projection.relativePath,
    parent,
    ready: Promise.resolve(),
  })
}

function observeInitialDurableRanges(
  execution: DirectTreeExecution,
  snapshots: FileRangeSnapshot[],
): DirectTreeExecution {
  const output: OutputSession = Object.freeze({
    identity: execution.output.identity,
    capabilities: execution.output.capabilities,
    executionProfile: execution.output.executionProfile,
    beginFile: async (request: OutputFileRequest, signal: AbortSignal) => {
      const begun = await execution.output.beginFile(request, signal)
      snapshots.push(Object.freeze({
        file: request.materializationRelativePath.join('/'),
        ranges: rangeTexts(begun),
      }))
      return begun
    },
  })
  return Object.freeze({ ...execution, output })
}

type V2RangeReadParameters = Parameters<V2BlockRangeReader['readRange']>

function observeRangeRequests(
  broker: V2BlockRangeReader,
  pathsByFileId: ReadonlyMap<string, string>,
  requests: ContentRangeRequest[],
): V2BlockRangeReader {
  return Object.freeze({
    readRange: async function* (
      descriptor: V2RangeReadParameters[0],
      leaseId: V2RangeReadParameters[1],
      range: V2RangeReadParameters[2],
      request: V2RangeReadParameters[3],
    ) {
      requests.push(Object.freeze({
        file: requireFileName(pathsByFileId, descriptor.fileIdText),
        range: rangeText(range),
      }))
      yield* broker.readRange(descriptor, leaseId, range, request)
    },
  })
}

async function startReceiving(
  repository: IndexedDbReceiveOperationRepository,
  intent: ReceiveIntent,
  leaseId: string,
): Promise<ReceiveLifecycleState> {
  const initial = initialReceiveLifecycleState({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
  })
  await repository.commitTransition({
    operationId: intent.operationId,
    expectedLeaseId: leaseId,
    lifecycle: initial,
  })
  const receiving = reduceReceiveLifecycle(initial, {
    kind: 'receive-started',
    expectedGeneration: initial.generation,
    leaseId,
  }, {
    planKind: 'direct-tree',
    preparationRequired: false,
    activeLeaseId: leaseId,
    nowMilliseconds: INITIAL_TIME_MILLISECONDS,
  }).state
  await repository.commitTransition({
    operationId: intent.operationId,
    expectedLifecycleGeneration: initial.generation,
    expectedLeaseId: leaseId,
    lifecycle: receiving,
  })
  return receiving
}

function resumeReceiving(
  state: ReceiveLifecycleState,
  leaseId: string,
): ReceiveLifecycleState {
  return reduceReceiveLifecycle(state, {
    kind: 'resume-started',
    expectedGeneration: state.generation,
    leaseId,
  }, {
    planKind: 'direct-tree',
    preparationRequired: false,
    activeLeaseId: leaseId,
    nowMilliseconds: RECOVERY_TIME_MILLISECONDS,
  }).state
}

async function readLifecycle(
  repository: IndexedDbReceiveOperationRepository,
  operationId: string,
): Promise<ReceiveLifecycleState> {
  const record = await repository.readLifecycle(operationId)
  if (record === undefined) throw new TypeError('DirectTree recovery lifecycle is missing')
  return decodeStoredReceiveLifecycleState(record)
}

async function readCheckpointSnapshots(
  intent: ReceiveIntent,
  databaseName: string,
): Promise<readonly CheckpointSnapshot[]> {
  const checkpoints = await readCheckpoints(intent, databaseName)
  return Object.freeze(checkpoints
    .map(checkpoint => Object.freeze({
      file: checkpoint.canonicalPath.join('/'),
      ranges: Object.freeze(checkpoint.verifiedRanges.map(rangeText)),
      complete: fileCheckpointIsComplete(checkpoint),
    }))
    .sort((left, right) => left.file.localeCompare(right.file)))
}

async function readCheckpoints(
  intent: ReceiveIntent,
  databaseName: string,
): Promise<readonly FileCheckpointV2[]> {
  const directTree = requireDirectTreeIntent(intent)
  const repository = await IndexedDbFileCheckpointRepository.open(
    durableCheckpointNamespaceIdentity({
      operationId: directTree.operationId,
      receiveIntentDigest: directTree.digest,
      materializationBindingDigest: directTree.plan.reservation.digest,
      materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
      authorityRef: directTree.plan.reservation.authorityRef,
    }),
    databaseName,
  )
  try {
    return await scanAllFSAFileCheckpoints(repository, 'committed')
  } finally {
    repository.close()
  }
}

async function readPhysicalFiles(
  parentName: string,
  resultRootName: string,
): Promise<readonly PhysicalFileSnapshot[]> {
  const parent = await (await originPrivateRoot()).getDirectoryHandle(parentName)
  const resultRoot = await parent.getDirectoryHandle(resultRootName)
  const files: PhysicalFileSnapshot[] = []
  for (const expected of FILES) {
    const handle = await resultRoot.getFileHandle(expected.name)
    const bytes = new Uint8Array(await (await handle.getFile()).arrayBuffer())
    files.push(Object.freeze({
      file: expected.name,
      bytes: Object.freeze([...bytes]),
    }))
  }
  return Object.freeze(files.sort((left, right) => left.file.localeCompare(right.file)))
}

async function recoveryArtifact(): Promise<DirectoryTreeArtifact> {
  return createResultRootDirectoryTreeArtifact(createSyntheticSelectionResultRoot())
}

function requireDirectTreeIntent(intent: ReceiveIntent): DirectTreeIntent {
  if (intent.plan.kind !== 'direct-tree' || intent.artifact.kind !== 'directory-tree') {
    throw new TypeError('process recovery fixture requires a DirectTree directory intent')
  }
  return intent as DirectTreeIntent
}

function rangeTexts(result: BeginOutputFileResult): readonly string[] {
  return Object.freeze(result.durableRanges.ranges.map(rangeText))
}

function rangeText(range: { readonly start: bigint; readonly end: bigint }): string {
  return `${range.start}:${range.end}`
}

function filledBytes(length: number): Uint8Array {
  return new Uint8Array(length).fill(FILE_CONTENT_BYTE)
}

function requireFileName(
  pathsByFileId: ReadonlyMap<string, string>,
  fileId: string,
): string {
  const name = pathsByFileId.get(fileId)
  if (name === undefined) throw new TypeError(`unknown recovery file identity ${fileId}`)
  return name
}

function sortFileRanges(values: readonly FileRangeSnapshot[]): FileRangeSnapshot[] {
  return [...values].sort((left, right) => left.file.localeCompare(right.file))
}

function sortContentRequests(values: readonly ContentRangeRequest[]): ContentRangeRequest[] {
  return [...values].sort((left, right) =>
    left.file.localeCompare(right.file) || left.range.localeCompare(right.range))
}

async function parentDirectory(name: string): Promise<FileSystemDirectoryHandle> {
  return (await originPrivateRoot()).getDirectoryHandle(name, { create: true })
}

async function originPrivateRoot(): Promise<FileSystemDirectoryHandle> {
  const storage = navigator.storage as StorageManager & {
    getDirectory(): Promise<FileSystemDirectoryHandle>
  }
  return storage.getDirectory()
}

async function resetFixture(fixture: FsaNamespaceFixture): Promise<void> {
  const root = await originPrivateRoot()
  await root.removeEntry(fixture.parentName, { recursive: true }).catch(() => undefined)
  await deleteDatabase(fixture.databaseName)
}

function deleteDatabase(name: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error('IndexedDB deletion failed'))
    request.onblocked = () => reject(new DOMException('IndexedDB deletion blocked', 'InvalidStateError'))
  })
}
