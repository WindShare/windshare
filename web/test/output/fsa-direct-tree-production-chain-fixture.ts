import type { V2CatalogClient } from '../../src/catalog/v2-client'
import {
  V2_CATALOG_PAGE_ENTRIES,
  type V2CatalogEntry,
} from '../../src/catalog/v2-records'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { FileCheckpointV2 } from '../../src/output/persistence/checkpoint'
import type { OutputDiagnosticsPorts } from '../../src/output/diagnostics'
import {
  type DirectoryAdmission,
} from '../../src/transfer/directory-admission'
import {
  createCompleteDirectoryResultRoot,
  createResultRootDirectoryTreeArtifact,
  createSelectionSpec,
  createSingleFileDirectoryTreeArtifact,
  createSyntheticSelectionResultRoot,
  type DirectoryTreeArtifact,
  type DirectTreePlan,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import {
  outputExecutionProfile,
  outputSessionIdentity,
  type BeginOutputFileResult,
  type DirectoryMaterializationRequest,
  type DirectTreeExecution,
  type IncrementalDirectoryOutput,
  type AutomaticCheckpointTrigger,
  type OutputCheckpointCostBudget,
  type OutputFileRequest,
  type OutputFileTransaction,
  type OutputSession,
  type V2PlanExecutionAuthority,
} from '../../src/transfer/output-session'
import {
  createPersistentDirectTreeExecution,
  type PersistentDirectTreeSettlementAuthority,
  type PersistentDirectTreeMaterializationEvidence,
} from '../../src/transfer/settlement/persistent-execution'
import { createV2PlanExecutionAuthority } from '../../src/transfer/settlement/v2-plan-authority'
import { TransferJob } from '../../src/transfer/v2-job'
import {
  reopenFileSystemAccessOutput,
  type FSAFileCheckpointRepositoryFactory,
} from '../../src/output/file-system-access/session'
import type { FSASemanticOutputRepository } from '../../src/output/file-system-access/checkpoint-repository'
import type { PersistentMaterializationPort } from '../../src/output/persistent-tree/contracts'
import { createFileSystemAccessSettlementAuthority } from '../../src/output/file-system-access/settlement'
import { RECEIVE_RECORD_RECEIPT } from '../../src/output/workspace/records'
import type { ReceiveLifecycleState } from '../../src/output/workspace/state'
import {
  MemoryDirectory,
  MemoryFile,
  MemoryCompatibleNameLedger,
  MemoryLockManager,
  MemoryOperationRepository,
  absentCompatibleNameLedgerFactory,
  bindTask,
  memoryCheckpointFactory,
  resumeReceiving,
  startReceiving,
} from './file-system-access-lifecycle-fixture'
import {
  catalogFixture,
  directoryEntry,
  fileEntry,
  identity,
  identityText,
  readerFixture,
  type CatalogDirectoryFixture,
} from '../transfer/v2-job-fixture'

export type FSAProductionChainScenarioKind =
  | 'single-file'
  | 'directory-anchor'
  | 'synthetic-root'

export type FSAProductionPersistenceVariant =
  | 'pause-resume'
  | 'collision-reserved-root'
  | 'compatible-name-restoration'

type ProductionPhase = 'initial' | 'reopened'

export interface ObservedDirectoryAdmission {
  readonly directoryId: string
  readonly path: readonly string[]
}

export interface FSAProductionChainObservation {
  readonly scenario: FSAProductionChainScenarioKind
  readonly directoryAdmissionAttempts: number
  readonly directoryAdmissions: readonly ObservedDirectoryAdmission[]
  readonly physicalEntries: readonly string[]
  readonly lifecycleKind?: string
  readonly workerStatus?: string
  readonly settlementFailure?: Readonly<{
    readonly name: string
    readonly message: string
  }>
}

export interface FSAProductionChainOptions {
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly syntheticRootFileCount?: number
  readonly maximumConcurrentFilePipelines?: number
  readonly maximumActiveNativeWriters?: number
}

export interface ObservedPhaseCoordinate {
  readonly phase: ProductionPhase
  readonly path: readonly string[]
}

export interface ObservedPhysicalLookup {
  readonly phase: ProductionPhase
  readonly kind: 'file' | 'directory'
  readonly name: string
  readonly create: boolean
}

export interface FSAProductionPersistenceObservation {
  readonly scenario: Exclude<FSAProductionChainScenarioKind, 'single-file'>
  readonly variant: FSAProductionPersistenceVariant
  readonly firstLifecycleKind: string
  readonly firstWorkerStatus: string
  readonly resumedLifecycleKind: string
  readonly resumedWorkerStatus: string
  readonly reservation: Readonly<{
    readonly requestedName: string
    readonly logicalReservedName: string
    readonly physicalName: string
    readonly collisionIndex: number
  }>
  readonly directoryAdmissions: readonly Readonly<ObservedDirectoryAdmission & {
    readonly phase: ProductionPhase
  }>[]
  readonly fileRequests: readonly ObservedPhaseCoordinate[]
  readonly checkpointLookups: readonly ObservedPhaseCoordinate[]
  readonly checkpointWrites: readonly ObservedPhaseCoordinate[]
  readonly reopenedDurableRanges: readonly Readonly<{
    readonly path: readonly string[]
    readonly ranges: readonly Readonly<{ readonly start: bigint; readonly end: bigint }>[]
  }>[]
  readonly pauseEvidenceKind: PersistentDirectTreeMaterializationEvidence['kind']
  readonly settlementEvidenceKind: PersistentDirectTreeMaterializationEvidence['kind']
  readonly materializationSealCount: number
  readonly settlementFinalProofReadCount: number
  readonly settlementReceiptPersisted: boolean
  readonly physicalLookups: readonly ObservedPhysicalLookup[]
  readonly parentPhysicalEntries: readonly string[]
  readonly taskRootPhysicalEntries: readonly string[]
  readonly compatibleNameMapping?: Readonly<{
    readonly logicalPath: readonly string[]
    readonly physicalComponent: string
    readonly commitState: string
  }>
}

interface ProductionScenario {
  readonly artifact: DirectoryTreeArtifact
  readonly catalogDirectories: readonly CatalogDirectoryFixture[]
  readonly files: readonly Extract<V2CatalogEntry, { kind: 'file' }>[]
}

interface AdmissionRecorder {
  attempts: number
  readonly admitted: ObservedDirectoryAdmission[]
}

interface FileCoordinateRecorder {
  readonly phase: ProductionPhase
  readonly requests: ObservedPhaseCoordinate[]
  readonly durableRanges: Array<Readonly<{
    readonly phase: ProductionPhase
    readonly path: readonly string[]
    readonly ranges: readonly Readonly<{ readonly start: bigint; readonly end: bigint }>[]
  }>>
  readonly pauseAfterFirstCheckpoint?: AbortController
  transactionFailure?: unknown
  pauseRequested: boolean
}

interface SettlementRecorder {
  pauseEvidence?: PersistentDirectTreeMaterializationEvidence
  pauseFailure?: unknown
  final?: Readonly<{
    readonly request: Parameters<PersistentDirectTreeSettlementAuthority['settle']>[0]
    readonly evidence: PersistentDirectTreeMaterializationEvidence
  }>
}

interface CheckpointRecorder {
  phase: ProductionPhase
  readonly lookups: ObservedPhaseCoordinate[]
  readonly writes: ObservedPhaseCoordinate[]
  readonly committed: FileCheckpointV2[]
  sealCount: number
  finalProofReadCount: number
}

const DESCRIPTOR_SHARE_INSTANCE_SEED = 1
const DESCRIPTOR_SYNTHETIC_ROOT_SEED = 2
const DIRECTORY_ANCHOR_SEED = 30
const DIRECTORY_ANCHOR_FILE_SEED = 31
const SYNTHETIC_ROOT_FILE_SEED = 40
const OPERATION_SEED = 120
const LIFECYCLE_LEASE_SEED = 123
const TRANSFER_JOB_SEED = 124
const OUTPUT_SESSION_SEED = 125
const PROTOCOL_SESSION_SEED = 126
const CATALOG_CHUNK_BYTES = 2
const TEST_RECOVERY_BUDGET_BYTES = 1_024n
const TEST_CHECKPOINT_TRIGGER_BYTES = 1_024n
const TEST_CHECKPOINT_TRIGGER_MILLISECONDS = 60_000
const PERSISTENCE_PAUSE_RESUME_ORDINAL = 10
const PERSISTENCE_COLLISION_ORDINAL = 20
const PERSISTENCE_COMPATIBLE_NAME_ORDINAL = 30
export const FSA_PRODUCTION_PARTIAL_FILE_BYTES = 2n
const COMPATIBLE_NAME_LOGICAL_FILE = 'blocked.txt'

export async function runFSAProductionChain(
  scenarioKind: FSAProductionChainScenarioKind,
  options: FSAProductionChainOptions = {},
): Promise<FSAProductionChainObservation> {
  const scenario = await productionScenario(
    scenarioKind,
    undefined,
    options.syntheticRootFileCount,
  )
  const parent = new MemoryDirectory(`downloads-${scenarioKind}`)
  const repository = new MemoryOperationRepository()
  const locks = new MemoryLockManager()
  const session = await bindTask({
    parent,
    repository,
    checkpointFactory: memoryCheckpointFactory(),
    locks,
    artifact: scenario.artifact,
    operationSeed: OPERATION_SEED + scenarioOrdinal(scenarioKind),
    selection: await createSelectionSpec({
      shareInstance: identityText(DESCRIPTOR_SHARE_INSTANCE_SEED),
      syntheticRoot: identityText(DESCRIPTOR_SYNTHETIC_ROOT_SEED),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    }),
    openCompatibleNameLedger: absentCompatibleNameLedgerFactory,
    ...(options.diagnostics === undefined ? {} : { diagnostics: options.diagnostics }),
    ...(options.maximumActiveNativeWriters === undefined
      ? {}
      : { maximumActiveWriters: options.maximumActiveNativeWriters }),
  })
  const lifecycleLeaseId = identityText(LIFECYCLE_LEASE_SEED + scenarioOrdinal(scenarioKind))
  const transferJobId = identityText(TRANSFER_JOB_SEED + scenarioOrdinal(scenarioKind))
  const outputSessionId = identityText(OUTPUT_SESSION_SEED + scenarioOrdinal(scenarioKind))
  await startReceiving(repository, session.intent, lifecycleLeaseId)

  const admissionRecorder: AdmissionRecorder = { attempts: 0, admitted: [] }
  const plans = await productionPlanAuthority({
    intent: directTreeIntent(session.intent),
    session,
    repository,
    lifecycleLeaseId,
    transferJobId,
    outputSessionId,
    admissionRecorder,
    ...(options.diagnostics === undefined ? {} : { diagnostics: options.diagnostics }),
    ...(options.maximumConcurrentFilePipelines === undefined
      ? {}
      : { maximumConcurrentFilePipelines: options.maximumConcurrentFilePipelines }),
  })
  const catalog = catalogWithSiblingAuthority(scenario.catalogDirectories)
  const readers = readerFixture(
    scenario.files,
    [],
    options.syntheticRootFileCount === undefined
      ? {}
      : { deriveRevisionAuthorityFromFileIdentity: true },
  )
  const job = productionTransferJob({
    catalog,
    revisions: readers.revisions,
    broker: readers.broker,
    plans,
    intent: session.intent,
    transferJobId,
    scenarioOrdinal: scenarioOrdinal(scenarioKind),
    protocolGeneration: 1,
  })

  let lifecycleKind: string | undefined
  let workerStatus: string | undefined
  let settlementFailure: FSAProductionChainObservation['settlementFailure']
  try {
    const result = await job.run()
    lifecycleKind = result.lifecycle.kind
    workerStatus = result.worker.status
  } catch (error) {
    settlementFailure = errorSnapshot(error)
  } finally {
    try {
      await session.close()
    } catch (error) {
      settlementFailure ??= errorSnapshot(error)
    }
  }

  return Object.freeze({
    scenario: scenarioKind,
    directoryAdmissionAttempts: admissionRecorder.attempts,
    directoryAdmissions: Object.freeze([...admissionRecorder.admitted]),
    physicalEntries: Object.freeze(await physicalEntries(parent)),
    ...(lifecycleKind === undefined ? {} : { lifecycleKind }),
    ...(workerStatus === undefined ? {} : { workerStatus }),
    ...(settlementFailure === undefined ? {} : { settlementFailure }),
  })
}

export async function runFSAProductionPersistenceChain(
  scenarioKind: Exclude<FSAProductionChainScenarioKind, 'single-file'>,
  variant: FSAProductionPersistenceVariant,
): Promise<FSAProductionPersistenceObservation> {
  const scenario = await productionScenario(
    scenarioKind,
    variant === 'compatible-name-restoration' ? COMPATIBLE_NAME_LOGICAL_FILE : undefined,
  )
  const parent = new MemoryDirectory(`downloads-${scenarioKind}-${variant}`)
  if (variant === 'collision-reserved-root') {
    await parent.getDirectoryHandle(resultRootName(scenario.artifact), { create: true })
  }
  const repository = new MemoryOperationRepository()
  const locks = new MemoryLockManager()
  const checkpointRecorder: CheckpointRecorder = {
    phase: 'initial',
    lookups: [],
    writes: [],
    committed: [],
    sealCount: 0,
    finalProofReadCount: 0,
  }
  const checkpointFactory = observeCheckpointPersistence(
    memoryCheckpointFactory(),
    checkpointRecorder,
  )
  const compatibleNames = variant === 'compatible-name-restoration'
    ? new MemoryCompatibleNameLedger(repository)
    : undefined
  const openCompatibleNameLedger = compatibleNames === undefined
    ? absentCompatibleNameLedgerFactory
    : async () => compatibleNames
  const ordinal = scenarioOrdinal(scenarioKind) + persistenceVariantOrdinal(variant)
  const session = await bindTask({
    parent,
    repository,
    checkpointFactory,
    locks,
    artifact: scenario.artifact,
    operationSeed: OPERATION_SEED + ordinal,
    selection: await createSelectionSpec({
      shareInstance: identityText(DESCRIPTOR_SHARE_INSTANCE_SEED),
      syntheticRoot: identityText(DESCRIPTOR_SYNTHETIC_ROOT_SEED),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    }),
    openCompatibleNameLedger,
  })
  const root = await parent.getDirectoryHandle(
    session.reservation.physicalName,
  ) as unknown as MemoryDirectory
  let physicalLookupPhase: ProductionPhase = 'initial'
  const physicalLookups: ObservedPhysicalLookup[] = []
  root.onEntryLookup = lookup => {
    physicalLookups.push(Object.freeze({ phase: physicalLookupPhase, ...lookup }))
    if (variant === 'compatible-name-restoration' && lookup.kind === 'file' &&
        lookup.name === COMPATIBLE_NAME_LOGICAL_FILE && !lookup.create) {
      throw new TypeError('simulated native compatible-name refusal')
    }
  }

  const lifecycleLeaseId = identityText(LIFECYCLE_LEASE_SEED + ordinal)
  const transferJobId = identityText(TRANSFER_JOB_SEED + ordinal)
  await startReceiving(repository, session.intent, lifecycleLeaseId)
  const initialAdmissionRecorder: AdmissionRecorder = { attempts: 0, admitted: [] }
  const pauseController = new AbortController()
  const initialFileRecorder: FileCoordinateRecorder = {
    phase: 'initial',
    requests: [],
    durableRanges: [],
    pauseAfterFirstCheckpoint: pauseController,
    pauseRequested: false,
  }
  const settlementRecorder: SettlementRecorder = {}
  const initialPlans = await productionPlanAuthority({
    intent: directTreeIntent(session.intent),
    session,
    repository,
    lifecycleLeaseId,
    transferJobId,
    outputSessionId: identityText(OUTPUT_SESSION_SEED + ordinal),
    admissionRecorder: initialAdmissionRecorder,
    fileRecorder: initialFileRecorder,
    settlementRecorder,
  })
  const catalog = catalogWithSiblingAuthority(scenario.catalogDirectories)
  const initialReaders = readerFixture(scenario.files)
  let initialResult: Awaited<ReturnType<TransferJob['run']>>
  try {
    initialResult = await productionTransferJob({
      catalog,
      revisions: initialReaders.revisions,
      broker: initialReaders.broker,
      plans: initialPlans,
      intent: session.intent,
      transferJobId,
      scenarioOrdinal: ordinal,
      protocolGeneration: 1,
    }).run(pauseController.signal)
  } finally {
    await session.close().catch(() => undefined)
  }

  if (initialResult.lifecycle.kind !== 'resumable-receive') {
    throw new TypeError(
      `production checkpoint pause settled as ${initialResult.lifecycle.kind}/${initialResult.worker.status}: ` +
      JSON.stringify(initialResult.lifecycle, (_key, value) =>
        typeof value === 'bigint' ? value.toString() : value) +
      `; pause failure: ${errorChain(settlementRecorder.pauseFailure)}`,
    )
  }
  await resumeReceiving(repository, session.intent, lifecycleLeaseId)
  checkpointRecorder.phase = 'reopened'
  physicalLookupPhase = 'reopened'
  const reopened = await reopenFileSystemAccessOutput({
    intent: session.intent,
    operationRepository: repository,
    lockManager: locks,
    checkpointRepositoryFactory: checkpointFactory,
    openCompatibleNameLedger,
    ...(compatibleNames === undefined ? {} : {
      compatibleNamePreparation: compatibleNamePreparation(),
    }),
  })
  const reopenedAdmissionRecorder: AdmissionRecorder = { attempts: 0, admitted: [] }
  const reopenedFileRecorder: FileCoordinateRecorder = {
    phase: 'reopened',
    requests: [],
    durableRanges: [],
    pauseRequested: false,
  }
  const reopenedPlans = await productionPlanAuthority({
    intent: directTreeIntent(reopened.intent),
    session: reopened,
    repository,
    lifecycleLeaseId,
    transferJobId,
    outputSessionId: identityText(OUTPUT_SESSION_SEED + ordinal + 1),
    admissionRecorder: reopenedAdmissionRecorder,
    fileRecorder: reopenedFileRecorder,
    settlementRecorder,
  })
  const reopenedReaders = readerFixture(scenario.files)
  let reopenedResult: Awaited<ReturnType<TransferJob['run']>>
  try {
    reopenedResult = await productionTransferJob({
      catalog,
      revisions: reopenedReaders.revisions,
      broker: reopenedReaders.broker,
      plans: reopenedPlans,
      intent: reopened.intent,
      transferJobId,
      scenarioOrdinal: ordinal,
      protocolGeneration: 2,
    }).run()
  } finally {
    await reopened.close().catch(() => undefined)
  }

  const finalSettlement = settlementRecorder.final
  if (finalSettlement === undefined) {
    throw new TypeError(
      `production resume did not expose final settlement evidence: ` +
      `${reopenedResult.lifecycle.kind}/${reopenedResult.worker.status}; ` +
      `${JSON.stringify(reopenedResult.worker.failures)}; ` +
      `requests=${JSON.stringify(reopenedFileRecorder.requests)}; ` +
      `ranges=${JSON.stringify(reopenedFileRecorder.durableRanges, (_key, value) =>
        typeof value === 'bigint' ? value.toString() : value)}; ` +
      `transactionFailure=${errorChain(reopenedFileRecorder.transactionFailure)}; ` +
      `pauseFailure=${errorChain(settlementRecorder.pauseFailure)}`,
    )
  }
  const settlementReceiptPersisted = await verifyPersistedSettlementReceipt({
    repository,
    lifecycle: reopenedResult.lifecycle,
  })
  const compatibleNameMapping = compatibleNames === undefined
    ? undefined
    : [...compatibleNames.mappings.values()].find(mapping => mapping.entryKind === 'file')

  return Object.freeze({
    scenario: scenarioKind,
    variant,
    firstLifecycleKind: initialResult.lifecycle.kind,
    firstWorkerStatus: initialResult.worker.status,
    resumedLifecycleKind: reopenedResult.lifecycle.kind,
    resumedWorkerStatus: reopenedResult.worker.status,
    reservation: Object.freeze({
      requestedName: session.reservation.requestedName,
      logicalReservedName: session.reservation.logicalReservedName,
      physicalName: session.reservation.physicalName,
      collisionIndex: session.reservation.collisionIndex,
    }),
    directoryAdmissions: Object.freeze([
      ...phaseAdmissions('initial', initialAdmissionRecorder.admitted),
      ...phaseAdmissions('reopened', reopenedAdmissionRecorder.admitted),
    ]),
    fileRequests: Object.freeze([
      ...initialFileRecorder.requests,
      ...reopenedFileRecorder.requests,
    ]),
    checkpointLookups: Object.freeze([...checkpointRecorder.lookups]),
    checkpointWrites: Object.freeze([...checkpointRecorder.writes]),
    reopenedDurableRanges: Object.freeze(reopenedFileRecorder.durableRanges.map(value =>
      Object.freeze({ path: value.path, ranges: value.ranges }))),
    pauseEvidenceKind: requireDirectTreeEvidence(settlementRecorder.pauseEvidence).kind,
    settlementEvidenceKind: finalSettlement.evidence.kind,
    materializationSealCount: checkpointRecorder.sealCount,
    settlementFinalProofReadCount: checkpointRecorder.finalProofReadCount,
    settlementReceiptPersisted,
    physicalLookups: Object.freeze([...physicalLookups]),
    parentPhysicalEntries: Object.freeze(await physicalEntries(parent)),
    taskRootPhysicalEntries: Object.freeze(await physicalEntries(root)),
    ...(compatibleNameMapping === undefined ? {} : {
      compatibleNameMapping: Object.freeze({
        logicalPath: Object.freeze([...compatibleNameMapping.logicalPath]),
        physicalComponent: compatibleNameMapping.physicalComponent,
        commitState: compatibleNameMapping.commitState,
      }),
    }),
  })
}

async function productionPlanAuthority(input: Readonly<{
  intent: ReceiveIntent & Readonly<{ plan: DirectTreePlan }>
  session: Awaited<ReturnType<typeof bindTask>>
  repository: MemoryOperationRepository
  lifecycleLeaseId: string
  transferJobId: string
  outputSessionId: string
  admissionRecorder: AdmissionRecorder
  fileRecorder?: FileCoordinateRecorder
  settlementRecorder?: SettlementRecorder
  diagnostics?: OutputDiagnosticsPorts
  maximumConcurrentFilePipelines?: number
}>): Promise<V2PlanExecutionAuthority> {
  const settlement = await createFileSystemAccessSettlementAuthority({
    intent: input.intent,
    repository: input.repository,
    lifecycleLeaseId: input.lifecycleLeaseId,
    transferJobId: input.transferJobId,
    clock: () => 1_000,
    ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
  })
  return createV2PlanExecutionAuthority({
    intent: input.intent,
    routes: {
      directTree: {
        open: async (intent, signal) => {
          signal.throwIfAborted()
          const persistentSettlement = settlement.bindMaterialization(input.session)
          const execution = await createPersistentDirectTreeExecution({
            intent,
            materialization: confirmedRecoveryMaterialization(input.session),
            executionProfile: outputExecutionProfile({
              maximumConcurrentFilePipelines: input.maximumConcurrentFilePipelines ?? 1,
              maximumOutstandingWriteBytes: TEST_RECOVERY_BUDGET_BYTES,
              maximumBufferedBytes: TEST_RECOVERY_BUDGET_BYTES,
              automaticCheckpoint: {
                kind: 'bounded',
                trigger: {
                  pendingBytes: TEST_CHECKPOINT_TRIGGER_BYTES,
                  pendingMilliseconds: TEST_CHECKPOINT_TRIGGER_MILLISECONDS,
                },
                costBudget: {
                  maximumPrefixCopyBytes: TEST_RECOVERY_BUDGET_BYTES,
                  maximumCumulativeWriteAmplificationBytes: TEST_RECOVERY_BUDGET_BYTES,
                  maximumPeakTemporaryBytes: TEST_RECOVERY_BUDGET_BYTES,
                },
              },
            }),
            namespaceClaims: input.session,
            repairSummary: () => input.session.repairSummary(),
            outputIdentity: outputSessionIdentity({
              backend: 'fsa-production-chain-test',
              outputSessionId: input.outputSessionId,
            }),
            settlement: input.settlementRecorder === undefined
              ? persistentSettlement
              : observeSettlement(persistentSettlement, input.settlementRecorder),
            ...(input.diagnostics?.performance === undefined
              ? {}
              : { performance: input.diagnostics.performance }),
          })
          return observeProductionExecution(
            execution,
            input.admissionRecorder,
            input.fileRecorder,
          )
        },
      },
      lifecycle: settlement,
    },
  })
}

function confirmedRecoveryMaterialization(
  session: Awaited<ReturnType<typeof bindTask>>,
): PersistentMaterializationPort {
  return Object.freeze({
    beginFile: (request: Parameters<PersistentMaterializationPort['beginFile']>[0]) => {
      const recovery = request.recovery?.kind === 'preserve'
        ? Object.freeze({
            ...request.recovery,
            confirmTemporarySpace: () => true,
          })
        : request.recovery
      return session.beginFile({
        ...request,
        ...(recovery === undefined ? {} : { recovery }),
      })
    },
    ensureDirectory: (path: Parameters<PersistentMaterializationPort['ensureDirectory']>[0]) =>
      session.ensureDirectory(path),
    materializeDirectory: (
      request: Parameters<NonNullable<PersistentMaterializationPort['materializeDirectory']>>[0],
    ) => session.materializeDirectory(request),
    finalizeDirectory: (
      admission: Parameters<NonNullable<PersistentMaterializationPort['finalizeDirectory']>>[0],
      outcome: Parameters<NonNullable<PersistentMaterializationPort['finalizeDirectory']>>[1],
    ) => session.finalizeDirectory(admission, outcome),
    closeForTerminalSettlement: () => session.closeForTerminalSettlement(),
    close: () => session.close(),
  })
}

function observeProductionExecution(
  execution: DirectTreeExecution,
  recorder: AdmissionRecorder,
  fileRecorder?: FileCoordinateRecorder,
): DirectTreeExecution {
  const directories = execution.directories
  const observedDirectories: IncrementalDirectoryOutput = Object.freeze({
    admitDirectory: async (
      request: DirectoryMaterializationRequest,
      signal: AbortSignal,
    ) => {
      recorder.attempts += 1
      const admission = await directories.admitDirectory(request, signal)
      recorder.admitted.push(observedAdmission(admission))
      return admission
    },
    finalizeDirectory: (admission: DirectoryAdmission, signal: AbortSignal) =>
      directories.finalizeDirectory(admission, signal),
  })
  const observedOutput = fileRecorder === undefined
    ? execution.output
    : observeFileCoordinates(execution.output, fileRecorder)
  return Object.freeze({
    ...execution,
    output: observedOutput,
    directories: observedDirectories,
  })
}

function observeFileCoordinates(
  output: OutputSession,
  recorder: FileCoordinateRecorder,
): OutputSession {
  return Object.freeze({
    identity: output.identity,
    capabilities: output.capabilities,
    executionProfile: output.executionProfile,
    beginFile: async (request: OutputFileRequest, signal: AbortSignal) => {
      const path = Object.freeze([...request.materializationRelativePath])
      recorder.requests.push(Object.freeze({ phase: recorder.phase, path }))
      const begun = await output.beginFile(request, signal)
      recorder.durableRanges.push(Object.freeze({
        phase: recorder.phase,
        path,
        ranges: snapshotRanges(begun),
      }))
      return Object.freeze({
        ...begun,
        transaction: observeCheckpointBoundary(begun.transaction, recorder),
      })
    },
  })
}

function observeCheckpointBoundary(
  transaction: OutputFileTransaction,
  recorder: FileCoordinateRecorder,
): OutputFileTransaction {
  return Object.freeze({
    writeRange: async (offset: bigint, data: Uint8Array, signal: AbortSignal) => {
      try {
        await transaction.writeRange(offset, data, signal)
      } catch (error) {
        recorder.transactionFailure = error
        throw error
      }
      const controller = recorder.pauseAfterFirstCheckpoint
      if (controller !== undefined && !recorder.pauseRequested) {
        recorder.pauseRequested = true
        controller.abort(new DOMException('production persistence write reached', 'AbortError'))
      }
    },
    automaticCheckpoint: (
      trigger: AutomaticCheckpointTrigger,
      budget: OutputCheckpointCostBudget,
      signal: AbortSignal,
    ) =>
      transaction.automaticCheckpoint(trigger, budget, signal),
    commit: async (signal: AbortSignal) => {
      try {
        return await transaction.commit(signal)
      } catch (error) {
        recorder.transactionFailure = error
        throw error
      }
    },
    retire: (reason: unknown) => transaction.retire(reason),
    pause: (reason: unknown) => transaction.pause(reason),
  })
}

function snapshotRanges(
  begun: BeginOutputFileResult,
): readonly Readonly<{ readonly start: bigint; readonly end: bigint }>[] {
  return Object.freeze(begun.durableRanges.ranges.map(range => Object.freeze({
    start: range.start,
    end: range.end,
  })))
}

function observeSettlement(
  settlement: PersistentDirectTreeSettlementAuthority,
  recorder: SettlementRecorder,
): PersistentDirectTreeSettlementAuthority {
  return Object.freeze({
    beginTerminal: (kind: Parameters<PersistentDirectTreeSettlementAuthority['beginTerminal']>[0]) =>
      settlement.beginTerminal(kind),
    pause: async (
      request: Parameters<PersistentDirectTreeSettlementAuthority['pause']>[0],
      cut: Parameters<PersistentDirectTreeSettlementAuthority['pause']>[1],
      signal: AbortSignal,
    ) => {
      try {
        const state = await settlement.pause(request, cut, signal)
        recorder.pauseEvidence = cut.evidence
        return state
      } catch (error) {
        recorder.pauseFailure = error
        throw error
      }
    },
    settle: async (
      request: Parameters<PersistentDirectTreeSettlementAuthority['settle']>[0],
      cut: Parameters<PersistentDirectTreeSettlementAuthority['settle']>[1],
      signal: AbortSignal,
    ) => {
      const state = await settlement.settle(request, cut, signal)
      recorder.final = Object.freeze({ request, evidence: cut.evidence })
      return state
    },
    ...(settlement.stop === undefined ? {} : {
      stop: async (
        request: Parameters<NonNullable<PersistentDirectTreeSettlementAuthority['stop']>>[0],
        cut: Parameters<NonNullable<PersistentDirectTreeSettlementAuthority['stop']>>[1],
        signal: AbortSignal,
      ) => {
        return settlement.stop!(request, cut, signal)
      },
    }),
  })
}

async function productionScenario(
  kind: FSAProductionChainScenarioKind,
  fileNameOverride?: string,
  syntheticRootFileCount?: number,
): Promise<ProductionScenario> {
  switch (kind) {
    case 'single-file': {
      const file = fileEntry(identity(3), 'report.bin', 4n)
      return Object.freeze({
        artifact: await createSingleFileDirectoryTreeArtifact({
          fileId: file.idText,
          sourcePath: 'report.bin',
          outputName: 'report.bin',
        }),
        catalogDirectories: Object.freeze([{
          id: identity(DESCRIPTOR_SYNTHETIC_ROOT_SEED),
          entries: Object.freeze([file]),
        }]),
        files: Object.freeze([file]),
      })
    }
    case 'directory-anchor': {
      const anchor = directoryEntry(identity(DIRECTORY_ANCHOR_SEED), 'photos')
      const file = fileEntry(
        identity(DIRECTORY_ANCHOR_FILE_SEED),
        fileNameOverride ?? 'image.bin',
        4n,
      )
      return Object.freeze({
        artifact: await createResultRootDirectoryTreeArtifact(
          createCompleteDirectoryResultRoot(anchor.idText, 'photos'),
        ),
        catalogDirectories: Object.freeze([
          {
            id: identity(DESCRIPTOR_SYNTHETIC_ROOT_SEED),
            entries: Object.freeze([anchor]),
          },
          {
            id: identity(DIRECTORY_ANCHOR_SEED),
            entries: Object.freeze([file]),
          },
        ]),
        files: Object.freeze([file]),
      })
    }
    case 'synthetic-root': {
      if (syntheticRootFileCount === undefined) {
        const file = fileEntry(
          identity(SYNTHETIC_ROOT_FILE_SEED),
          fileNameOverride ?? 'payload.bin',
          4n,
        )
        return Object.freeze({
          artifact: await createResultRootDirectoryTreeArtifact(
            createSyntheticSelectionResultRoot(),
          ),
          catalogDirectories: Object.freeze([{
            id: identity(DESCRIPTOR_SYNTHETIC_ROOT_SEED),
            entries: Object.freeze([file]),
          }]),
          files: Object.freeze([file]),
        })
      }
      const files = Array.from({ length: syntheticRootFileCount }, (_, ordinal) => fileEntry(
        performanceFileIdentity(ordinal),
        `payload-${ordinal.toString().padStart(4, '0')}.bin`,
        1n,
      ))
      const rootEntries: V2CatalogEntry[] = []
      const catalogDirectories: CatalogDirectoryFixture[] = []
      for (let offset = 0; offset < files.length; offset += V2_CATALOG_PAGE_ENTRIES) {
        const ordinal = offset / V2_CATALOG_PAGE_ENTRIES
        const directoryId = performanceDirectoryIdentity(ordinal)
        rootEntries.push(directoryEntry(
          directoryId,
          `batch-${ordinal.toString().padStart(4, '0')}`,
        ))
        catalogDirectories.push(Object.freeze({
          id: directoryId,
          entries: Object.freeze(files.slice(offset, offset + V2_CATALOG_PAGE_ENTRIES)),
        }))
      }
      return Object.freeze({
        artifact: await createResultRootDirectoryTreeArtifact(
          createSyntheticSelectionResultRoot(),
        ),
        catalogDirectories: Object.freeze([
          Object.freeze({
            id: identity(DESCRIPTOR_SYNTHETIC_ROOT_SEED),
            entries: Object.freeze(rootEntries),
          }),
          ...catalogDirectories,
        ]),
        files: Object.freeze(files),
      })
    }
  }
}

function performanceFileIdentity(ordinal: number): Uint8Array<ArrayBuffer> {
  const identity = new Uint8Array(16)
  identity[0] = 0x80
  identity[1] = ordinal >>> 8
  identity[2] = ordinal
  return identity
}

function performanceDirectoryIdentity(ordinal: number): Uint8Array<ArrayBuffer> {
  const identity = performanceFileIdentity(ordinal)
  identity[0] = 0x70
  return identity
}

function productionTransferJob(input: Readonly<{
  catalog: V2CatalogClient
  revisions: ReturnType<typeof readerFixture>['revisions']
  broker: ReturnType<typeof readerFixture>['broker']
  plans: V2PlanExecutionAuthority
  intent: ReceiveIntent
  transferJobId: string
  scenarioOrdinal: number
  protocolGeneration: number
}>): TransferJob {
  return new TransferJob({
    descriptor: {
      shareInstance: identity(DESCRIPTOR_SHARE_INSTANCE_SEED),
      syntheticRoot: identity(DESCRIPTOR_SYNTHETIC_ROOT_SEED),
      syntheticRootId: identityText(DESCRIPTOR_SYNTHETIC_ROOT_SEED),
      chunkSize: CATALOG_CHUNK_BYTES,
    } as never,
    catalog: input.catalog,
    selection: new V2SelectionPolicy(true),
    revisions: input.revisions,
    broker: input.broker,
    lanes: { size: 1 },
    revisionCapacity: {
      generation: {
        waitForProtocolSessionReplacement: (_identity, signal) => waitForAbort(signal),
      },
    },
    plans: input.plans,
    intent: input.intent,
    transferJobId: input.transferJobId,
    protocol: {
      sessionId: identityText(PROTOCOL_SESSION_SEED + input.scenarioOrdinal),
      generation: input.protocolGeneration,
    },
  })
}

function observeCheckpointPersistence(
  factory: FSAFileCheckpointRepositoryFactory,
  recorder: CheckpointRecorder,
): FSAFileCheckpointRepositoryFactory {
  return async binding => {
    const repository = await factory(binding)
    const semantic = repository as FSASemanticOutputRepository
    return new Proxy(repository, {
      get(target, property) {
        if (property === 'lookupLineage') {
          return async (request: Parameters<typeof target.lookupLineage>[0]) => {
            recorder.lookups.push(phaseCoordinate(recorder.phase, request.canonicalPath))
            return target.lookupLineage(request)
          }
        }
        if (property === 'classifyLineages') {
          return async (...args: Parameters<typeof semantic.classifyLineages>) => {
            for (const request of args[0]) {
              recorder.lookups.push(phaseCoordinate(recorder.phase, request.canonicalPath))
            }
            return semantic.classifyLineages(...args)
          }
        }
        if (property === 'commitCheckpointCandidate') {
          return async (
            candidate: Parameters<typeof target.commitCheckpointCandidate>[0],
            committed: Parameters<typeof target.commitCheckpointCandidate>[1],
          ) => {
            await target.commitCheckpointCandidate(candidate, committed)
            recorder.writes.push(phaseCoordinate(recorder.phase, committed.canonicalPath))
            recorder.committed.push(committed)
          }
        }
        if (property === 'commitCreatedFile') {
          return async (...args: Parameters<typeof semantic.commitCreatedFile>) => {
            await semantic.commitCreatedFile(...args)
            recorder.writes.push(phaseCoordinate(recorder.phase, args[0].committed.canonicalPath))
            recorder.committed.push(args[0].committed)
          }
        }
        if (property === 'commitDurableCut') {
          return async (...args: Parameters<typeof semantic.commitDurableCut>) => {
            await semantic.commitDurableCut(...args)
            recorder.writes.push(phaseCoordinate(recorder.phase, args[1].canonicalPath))
            recorder.committed.push(args[1])
          }
        }
        if (property === 'resumePausedCheckpoint') {
          return async (...args: Parameters<typeof semantic.resumePausedCheckpoint>) => {
            await semantic.resumePausedCheckpoint(...args)
            recorder.writes.push(phaseCoordinate(recorder.phase, args[1].canonicalPath))
            recorder.committed.push(args[1])
          }
        }
        if (property === 'commitFinalFile') {
          return async (...args: Parameters<typeof semantic.commitFinalFile>) => {
            const result = await semantic.commitFinalFile(...args)
            recorder.writes.push(phaseCoordinate(
              recorder.phase,
              args[0].records.finalCheckpoint.canonicalPath,
            ))
            recorder.committed.push(args[0].records.finalCheckpoint)
            return result
          }
        }
        if (property === 'sealMaterializationLedger') {
          return async (...args: Parameters<typeof semantic.sealMaterializationLedger>) => {
            recorder.sealCount += 1
            return semantic.sealMaterializationLedger(...args)
          }
        }
        if (property === 'readMaterializationFinalProof') {
          return async (...args: Parameters<typeof semantic.readMaterializationFinalProof>) => {
            recorder.finalProofReadCount += 1
            return semantic.readMaterializationFinalProof(...args)
          }
        }
        const value = Reflect.get(target, property, target) as unknown
        return typeof value === 'function' ? value.bind(target) : value
      },
    })
  }
}

async function verifyPersistedSettlementReceipt(input: Readonly<{
  repository: MemoryOperationRepository
  lifecycle: ReceiveLifecycleState
}>): Promise<boolean> {
  if (!('receiptDigest' in input.lifecycle) || typeof input.lifecycle.receiptDigest !== 'string') {
    return false
  }
  const receiptDigest = input.lifecycle.receiptDigest
  return input.repository.recordsOfKind(RECEIVE_RECORD_RECEIPT).some(record =>
    record.digest === receiptDigest)
}

function phaseCoordinate(
  phase: ProductionPhase,
  path: readonly string[],
): ObservedPhaseCoordinate {
  return Object.freeze({ phase, path: Object.freeze([...path]) })
}

function phaseAdmissions(
  phase: ProductionPhase,
  admissions: readonly ObservedDirectoryAdmission[],
): readonly Readonly<ObservedDirectoryAdmission & { readonly phase: ProductionPhase }>[] {
  return Object.freeze(admissions.map(admission => Object.freeze({ phase, ...admission })))
}

function requireDirectTreeEvidence(
  evidence: PersistentDirectTreeMaterializationEvidence | undefined,
): PersistentDirectTreeMaterializationEvidence {
  if (evidence === undefined) {
    throw new TypeError('production pause did not expose its immutable ledger locator')
  }
  return evidence
}

function resultRootName(artifact: DirectoryTreeArtifact): string {
  switch (artifact.layout.kind) {
    case 'single-file': return artifact.layout.outputName
    case 'result-root': return artifact.layout.root.name
    case 'catalog-root': throw new TypeError('production persistence fixture requires a named result root')
  }
}

function compatibleNamePreparation() {
  return Object.freeze({
    platform: 'windows' as const,
    templateProvider: Object.freeze({
      select: () => Object.freeze({
        id: 'windows-powershell-v1',
        scriptFileExtension: '.ps1',
        source: '# test restoration script\n',
      }),
    }),
    randomBits: () => 0,
  })
}

function catalogWithSiblingAuthority(
  directories: readonly CatalogDirectoryFixture[],
): V2CatalogClient {
  const fixture = catalogFixture(directories)
  const entriesByDirectory = new Map(directories.map(directory => [
    encodeBase64Url(directory.id),
    directory.entries,
  ]))
  return Object.freeze({
    ...fixture.catalog,
    hasCommittedName: async (
      directory: Readonly<{ directoryIdText: string }>,
      name: string,
      signal?: AbortSignal,
    ) => {
      signal?.throwIfAborted()
      return entriesByDirectory.get(directory.directoryIdText)
        ?.some(entry => entry.name === name) === true
    },
  }) as unknown as V2CatalogClient
}

async function physicalEntries(
  root: MemoryDirectory,
  prefix = '',
): Promise<readonly string[]> {
  const observed: string[] = []
  for await (const [name, entry] of root.entries()) {
    const path = prefix.length === 0 ? name : `${prefix}/${name}`
    if (entry instanceof MemoryDirectory) {
      observed.push(`${path}/`)
      observed.push(...await physicalEntries(entry, path))
    } else if (entry instanceof MemoryFile) {
      observed.push(path)
    } else {
      throw new TypeError('memory FSA fixture exposed an unknown entry kind')
    }
  }
  return observed.sort()
}

function observedAdmission(input: unknown): ObservedDirectoryAdmission {
  const record = objectRecord(input)
  const directoryId = record.directoryId
  if (typeof directoryId !== 'string') {
    throw new TypeError('production directory admission omitted its directory identity')
  }
  return Object.freeze({
    directoryId,
    path: coordinateSegments(record.path),
  })
}

function coordinateSegments(input: unknown): readonly string[] {
  if (Array.isArray(input) && input.every(segment => typeof segment === 'string')) {
    return Object.freeze([...input])
  }
  const record = objectRecord(input)
  for (const field of ['segments', 'path', 'value', 'relativePath']) {
    if (field in record) return coordinateSegments(record[field])
  }
  throw new TypeError('production directory admission omitted its relative path')
}

function objectRecord(input: unknown): Record<string, unknown> {
  return typeof input === 'object' && input !== null
    ? input as Record<string, unknown>
    : {}
}

function directTreeIntent(
  intent: ReceiveIntent,
): ReceiveIntent & Readonly<{ plan: DirectTreePlan; artifact: DirectoryTreeArtifact }> {
  if (intent.plan.kind !== 'direct-tree' || intent.artifact.kind !== 'directory-tree') {
    throw new TypeError('production fixture requires a DirectTree directory artifact')
  }
  return intent as ReceiveIntent & Readonly<{
    plan: DirectTreePlan
    artifact: DirectoryTreeArtifact
  }>
}

function errorSnapshot(error: unknown): Readonly<{ name: string; message: string }> {
  if (error instanceof Error) {
    return Object.freeze({ name: error.name, message: error.message })
  }
  return Object.freeze({ name: 'NonError', message: String(error) })
}

function errorChain(error: unknown): string {
  if (!(error instanceof Error)) return String(error)
  const cause = error.cause === undefined ? '' : ` <- ${errorChain(error.cause)}`
  return `${error.name}: ${error.message}${cause}`
}

function scenarioOrdinal(kind: FSAProductionChainScenarioKind): number {
  switch (kind) {
    case 'single-file': return 0
    case 'directory-anchor': return 1
    case 'synthetic-root': return 2
  }
}

function persistenceVariantOrdinal(variant: FSAProductionPersistenceVariant): number {
  switch (variant) {
    case 'pause-resume': return PERSISTENCE_PAUSE_RESUME_ORDINAL
    case 'collision-reserved-root': return PERSISTENCE_COLLISION_ORDINAL
    case 'compatible-name-restoration': return PERSISTENCE_COMPATIBLE_NAME_ORDINAL
  }
}

function waitForAbort(signal: AbortSignal): Promise<void> {
  signal.throwIfAborted()
  return new Promise((_resolve, reject) => {
    const abort = () => reject(signal.reason ?? new DOMException('wait aborted', 'AbortError'))
    signal.addEventListener('abort', abort, { once: true })
    if (signal.aborted) abort()
  })
}
