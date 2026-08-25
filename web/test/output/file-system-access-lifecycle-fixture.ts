import {
  createCompleteDirectoryResultRoot,
  createDirectTreePlan,
  createReceiveIntent,
  createResultRootDirectoryTreeArtifact,
  createSelectionSpec,
  createSingleFileDirectoryTreeArtifact,
  deriveArtifactChoiceIdentity,
  type DirectoryTreeArtifact,
  type DirectTreePlan,
  type ReceiveIntent,
  type SelectionSpec,
} from '../../src/transfer/intent'
import type { DirectoryAdmission } from '../../src/transfer/directory-admission'
import { createDirectTreeCoordinateContract } from '../../src/transfer/job/coordinate/direct-tree'
import { fsaParentOffer } from '../../src/output/capability/acquisition'
import type { AcquiredFSAParentAuthority } from '../../src/output/capability/contract'
import {
  acquireFSARootMutationLease,
  type BrowserLockManagerRuntime,
} from '../../src/output/browser/namespace-mutation'
import {
  prepareFSAOperationBindingTransition,
  verifyFSAOperationBinding,
  FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SCRIPT,
  FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SIDECAR,
} from '../../src/output/browser/indexeddb-root-binding'
import {
  assembleNewFileSystemAccessOutput,
  reserveNewFileSystemAccessOutput,
  type FSAFileCheckpointRepositoryFactory,
  type FSAOutputTraceEvent,
  type FileSystemAccessOutputSession,
} from '../../src/output/file-system-access/session'
import { createFileSystemAccessSettlementAuthority } from '../../src/output/file-system-access/settlement'
import { memoryCheckpointFactory } from './fsa-memory-semantic-repository'
import type {
  CompatibleNameActivationLedger,
  CompatibleNameRootRepairFactory,
} from '../../src/output/file-system-access/compatible-name/coordinator'
import {
  compatibleNameMappingV1,
  compatibleNameOperationBootstrapV1,
  compatibleNameOperationHeaderV1,
  compatibleNameRepairSummary,
  type CompatibleNameMappingV1,
  type CompatibleNameOperationHeaderV1,
  type CompatibleNamePendingTerminalOutcomeV1,
  type CompatibleNameRepairSummary,
} from '../../src/output/file-system-access/compatible-name/model'
import {
  discardReopenedFileSystemAccessOutput,
  type ReopenedFileSystemAccessDiscardOperation,
} from '../../src/output/file-system-access/fresh-page-discard'
import type {
  PersistentOutputStageDiagnostics,
} from '../../src/output/persistent-tree/stage-diagnostics'
import type { OutputDiagnosticsPorts } from '../../src/output/diagnostics'
import {
  RECEIVE_RECORD_LIFECYCLE_STATE,
  receiveOperationHandleRecord,
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
import {
  disabledOutputExecutionProfile,
  outputSessionIdentity,
} from '../../src/transfer/output-session'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  transferWorkerSettlement,
} from '../../src/transfer/outcome'
import { createPersistentDirectTreeExecution } from '../../src/transfer/settlement/persistent-execution'
import type { PersistentMaterializationPort } from '../../src/output/persistent-tree/contracts'
import { identity } from './planning/fixture'
import {
  MemoryDirectory,
} from './file-system-access-memory-fs'

export {
  MemoryDirectory,
  MemoryFile,
  type MemoryDirectoryLookup,
} from './file-system-access-memory-fs'

export const SIGNAL = new AbortController().signal
export const SUCCESS = transferWorkerSettlement('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
export const PAUSED = transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY)


export interface FreshDiscardFixture {
  readonly parent: MemoryDirectory
  readonly repository: MemoryOperationRepository
  readonly checkpointFactory: FSAFileCheckpointRepositoryFactory
  readonly locks: MemoryLockManager
  readonly intent: ReceiveIntent
  readonly physicalName: string
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
    parentAdmission = await execution.directories.admitDirectory(
      await rootDirectoryMaterializationRequest(session.intent, identity(input.seed + 6)),
      SIGNAL,
    )
    const complete = await execution.output.beginFile(await outputFileRequest({
      intent: session.intent,
      fileId: identity(input.seed + 7),
      fileRevision: identity(input.seed + 8),
      sourceRelativePath: ['kept.bin'],
      exactSize: 2n,
      parentAdmission,
    }), SIGNAL)
    await complete.transaction.writeRange(0n, Uint8Array.of(7, 8), SIGNAL)
    await complete.transaction.commit(SIGNAL)
  }

  const unfinished = await execution.output.beginFile(await outputFileRequest({
    intent: session.intent,
    fileId: input.successfulFile ? identity(input.seed + 9) : identity(3),
    fileRevision: identity(input.seed + 10),
    ...(input.successfulFile
      ? { sourceRelativePath: ['unfinished.bin'] }
      : {}),
    exactSize: 2n,
    ...(parentAdmission === undefined ? {} : { parentAdmission }),
  }), SIGNAL)
  await unfinished.transaction.writeRange(0n, Uint8Array.of(1), SIGNAL)
  await unfinished.transaction.pause('fresh-page-discard')
  if (parentAdmission !== undefined) {
    await execution.directories.finalizeDirectory(parentAdmission, SIGNAL)
  }
  await execution.pause({
    worker: PAUSED,
    materialization: input.successfulFile
      ? { entryCount: 1n, fileCount: 1n, directoryCount: 0n, rawBytes: 2n }
      : { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
    reason: new DOMException('page closed', 'AbortError'),
  }, SIGNAL)
  await session.releaseRootLease()

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
    physicalName: session.reservation.physicalName,
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
    openCompatibleNameLedger: async () => new AbsentCompatibleNameLedger(),
    clock: () => nowMilliseconds,
  })
}

export function absentCompatibleNameLedgerFactory(): Promise<CompatibleNameActivationLedger> {
  return Promise.resolve(new AbsentCompatibleNameLedger())
}

class AbsentCompatibleNameLedger implements CompatibleNameActivationLedger {
  async readHeader(): Promise<undefined> { return undefined }
  async loadOperation(): Promise<undefined> { return undefined }
  async bootstrapOperation(): Promise<never> { throw new Error('unexpected compatible bootstrap') }
  async claimMapping(): Promise<never> { throw new Error('unexpected compatible mapping') }
  async recordPairOwnership(): Promise<never> { throw new Error('unexpected pair ownership') }
  async recordCompatibleTargetCreated(): Promise<never> { throw new Error('unexpected target activation') }
  async recordVerifiedDirectoryOwnership(): Promise<never> {
    throw new Error('unexpected directory ownership')
  }
  async commitMapping(): Promise<never> { throw new Error('unexpected compatible commit') }
  async scanCommittedMappings(): Promise<readonly never[]> { return Object.freeze([]) }
  async persistRepairSummary(): Promise<never> { throw new Error('unexpected compatible summary') }
  async persistPendingTerminalOutcome(): Promise<never> { throw new Error('unexpected terminal outcome') }
  async readPendingTerminalOutcome(): Promise<undefined> { return undefined }
  async clearPendingTerminalOutcome(): Promise<never> { throw new Error('unexpected terminal clear') }
  async readRepairSummary(): Promise<undefined> { return undefined }
  async removeVerifiedEmptyOperation(): Promise<never> { throw new Error('unexpected repair cleanup') }
  close(): void {}
}

export class MemoryCompatibleNameLedger implements CompatibleNameActivationLedger {
  header: CompatibleNameOperationHeaderV1 | undefined
  readonly mappings = new Map<string, CompatibleNameMappingV1>()
  closeCount = 0
  #nextOrdinal = 1
  readonly #repository: MemoryOperationRepository

  constructor(repository: MemoryOperationRepository) {
    this.#repository = repository
  }

  async readHeader(operationId: string): Promise<CompatibleNameOperationHeaderV1 | undefined> {
    return this.header?.operationId === operationId ? this.header : undefined
  }

  async loadOperation(operationId: string) {
    if (this.header?.operationId !== operationId) return undefined
    return Object.freeze({
      header: this.header,
      mappings: Object.freeze([...this.mappings.values()]),
    })
  }

  async bootstrapOperation(input: Parameters<
    CompatibleNameActivationLedger['bootstrapOperation']
  >[0]) {
    const bootstrap = compatibleNameOperationBootstrapV1(input)
    if (this.header !== undefined) throw new Error('memory compatible operation already exists')
    this.header = bootstrap.header
    this.mappings.set(bootstrap.initialMapping.id, bootstrap.initialMapping)
    return (await this.loadOperation(bootstrap.header.operationId))!
  }

  async claimMapping(input: Parameters<CompatibleNameActivationLedger['claimMapping']>[0]) {
    const mapping = compatibleNameMappingV1(input)
    const existing = this.mappings.get(mapping.id)
    if (existing !== undefined) return existing
    this.mappings.set(mapping.id, mapping)
    return mapping
  }

  async recordPairOwnership(input: Parameters<
    CompatibleNameActivationLedger['recordPairOwnership']
  >[0]) {
    const header = this.#requireHeader(input.operationId)
    const identity = header.pair[input.pairKind]
    const kind = input.pairKind === 'script'
      ? FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SCRIPT
      : FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SIDECAR
    await this.#repository.commitTransition({
      operationId: input.operationId,
      handles: [receiveOperationHandleRecord({
        id: identity.handleId,
        operationId: input.operationId,
        kind,
        authorityRef: header.authorityRef,
        ownedObjectId: identity.ownedObjectId,
        handle: input.handle,
      })],
    })
    const pair = Object.freeze({
      ...header.pair,
      [input.pairKind]: Object.freeze({ ...identity, ownershipState: 'owned' as const }),
    })
    this.header = compatibleNameOperationHeaderV1({
      ...header,
      pair,
      activationState: pair.script.ownershipState === 'owned' &&
        pair.sidecar.ownershipState === 'owned' ? 'pair-ready' : 'prepared',
    })
    return this.header
  }

  async recordCompatibleTargetCreated(input: Parameters<
    CompatibleNameActivationLedger['recordCompatibleTargetCreated']
  >[0]) {
    const header = this.#requireHeader(input.operationId)
    this.#mapping(input.operationId, input.logicalPath, input.entryKind)
    if (header.activationState === 'active') return header
    this.header = compatibleNameOperationHeaderV1({
      ...header,
      activationState: 'active',
      repairSummary: input.repairSummary,
    })
    return this.header
  }

  async recordVerifiedDirectoryOwnership(input: Parameters<
    CompatibleNameActivationLedger['recordVerifiedDirectoryOwnership']
  >[0]) {
    const mapping = this.#mapping(input.operationId, input.logicalPath, input.entryKind)
    const owned = compatibleNameMappingV1({
      ...mapping,
      ownershipState: 'owned',
      ownedObjectId: input.ownedObjectId,
    })
    this.mappings.set(owned.id, owned)
    return owned
  }

  async commitMapping(input: Parameters<CompatibleNameActivationLedger['commitMapping']>[0]) {
    const mapping = this.#mapping(input.operationId, input.logicalPath, input.entryKind)
    if (mapping.commitState === 'committed') return mapping
    const committed = compatibleNameMappingV1({
      ...mapping,
      ownershipState: 'owned',
      ownedObjectId: input.ownedObjectId,
      commitState: 'committed',
      commitOrdinal: this.#nextOrdinal++,
    })
    this.mappings.set(committed.id, committed)
    this.header = compatibleNameOperationHeaderV1({
      ...this.#requireHeader(input.operationId),
      ...(this.header?.repairSummary === undefined
        ? {}
        : {
            repairSummary: compatibleNameRepairSummary({
              ...this.header.repairSummary,
              committedCount: committed.commitOrdinal!,
              logicalPathSample: this.header.repairSummary.logicalPathSample.length === 0
                ? [committed.logicalPath]
                : this.header.repairSummary.logicalPathSample,
              pendingCatchUp: true,
            }),
          }),
    })
    return committed
  }

  async scanCommittedMappings(operationId: string, afterOrdinal = 0) {
    this.#requireHeader(operationId)
    return Object.freeze([...this.mappings.values()]
      .filter(mapping => (mapping.commitOrdinal ?? 0) > afterOrdinal)
      .sort((left, right) => left.commitOrdinal! - right.commitOrdinal!))
  }

  async persistRepairSummary(
    operationId: string,
    summary: CompatibleNameRepairSummary,
  ): Promise<CompatibleNameOperationHeaderV1> {
    this.header = compatibleNameOperationHeaderV1({
      ...this.#requireHeader(operationId),
      repairSummary: summary,
    })
    return this.header
  }

  async persistPendingTerminalOutcome(input: {
    operationId: string
    outcome: CompatibleNamePendingTerminalOutcomeV1
    repairSummary: CompatibleNameRepairSummary
  }): Promise<CompatibleNameOperationHeaderV1> {
    this.header = compatibleNameOperationHeaderV1({
      ...this.#requireHeader(input.operationId),
      pendingTerminalOutcome: input.outcome,
      repairSummary: input.repairSummary,
    })
    return this.header
  }

  async readPendingTerminalOutcome(
    operationId: string,
  ): Promise<CompatibleNamePendingTerminalOutcomeV1 | undefined> {
    return this.#requireHeader(operationId).pendingTerminalOutcome
  }

  async clearPendingTerminalOutcome(input: {
    operationId: string
    repairSummary: CompatibleNameRepairSummary
  }): Promise<CompatibleNameOperationHeaderV1> {
    const header = { ...this.#requireHeader(input.operationId), repairSummary: input.repairSummary }
    delete header.pendingTerminalOutcome
    this.header = compatibleNameOperationHeaderV1(header)
    return this.header
  }

  async readRepairSummary(operationId: string): Promise<CompatibleNameRepairSummary | undefined> {
    return this.#requireHeader(operationId).repairSummary
  }

  async removeVerifiedEmptyOperation(
    expectedHeader: CompatibleNameOperationHeaderV1,
  ): Promise<void> {
    if (this.header === undefined) return
    const committed = [...this.mappings.values()].filter(mapping => mapping.commitState === 'committed')
    if (this.header.operationId !== expectedHeader.operationId || committed.length !== 0) {
      throw new Error('memory compatible repair is not empty')
    }
    await this.#repository.commitTransition({
      operationId: expectedHeader.operationId,
      deleteHandleIds: [expectedHeader.pair.script.handleId, expectedHeader.pair.sidecar.handleId],
    })
    this.mappings.clear()
    this.header = undefined
  }

  close(): void { this.closeCount += 1 }

  #requireHeader(operationId: string): CompatibleNameOperationHeaderV1 {
    if (this.header?.operationId !== operationId) throw new Error('memory compatible header is missing')
    return this.header
  }

  #mapping(
    operationId: string,
    logicalPath: readonly string[],
    entryKind: 'file' | 'directory',
  ): CompatibleNameMappingV1 {
    const mapping = [...this.mappings.values()].find(value =>
      value.operationId === operationId && value.entryKind === entryKind &&
      value.logicalPath.length === logicalPath.length &&
      value.logicalPath.every((component, index) => component === logicalPath[index]))
    if (mapping === undefined) throw new Error('memory compatible mapping is missing')
    return mapping
  }
}

export async function persistFreshFixtureExpiry(
  fixture: FreshDiscardFixture,
): Promise<Extract<ReceiveLifecycleState, { kind: 'expired' }>> {
  const current = await lifecycleState(fixture.repository, fixture.intent.operationId)
  if (current.kind !== 'resumable-receive' || current.payloadKind !== 'file-set') {
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
  selection?: SelectionSpec
  activate?: boolean
  prepareCompatibleNameRootRepair?: CompatibleNameRootRepairFactory
  openCompatibleNameLedger?: () => Promise<CompatibleNameActivationLedger>
  stageDiagnostics?: PersistentOutputStageDiagnostics
  diagnostics?: OutputDiagnosticsPorts
  maximumActiveWriters?: number
  trace?: (event: FSAOutputTraceEvent) => void
}>) {
  const selection = input.selection ?? await selectionSpec()
  const authority = acquiredParent(input.parent)
  const rootLease = await acquireFSARootMutationLease(
    input.parent as unknown as FileSystemDirectoryHandle,
    input.locks,
    input.maximumActiveWriters,
    input.diagnostics?.performance,
  )
  const reserved = await reserveNewFileSystemAccessOutput({
    authority,
    artifact: input.artifact,
    rootLease,
    operationId: identity(input.operationSeed),
    reservationId: identity(input.operationSeed + 1),
    authorityRef: identity(input.operationSeed + 2, 32),
    ...(input.prepareCompatibleNameRootRepair === undefined
      ? {}
      : { prepareCompatibleNameRootRepair: input.prepareCompatibleNameRootRepair }),
    ...(input.trace === undefined ? {} : { trace: input.trace }),
    ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
  })
  const intent = await createReceiveIntent({
    selection,
    artifact: input.artifact,
    plan: await createDirectTreePlan(input.artifact, reserved.reservation),
  })
  const prepared = await prepareFSAOperationBindingTransition({
    repository: input.repository,
    intent,
    parent: authority.parent,
    preClickRanking: [(await deriveArtifactChoiceIdentity(intent.artifact, intent.plan)).id],
  })
  await input.repository.commitTransition({ operationId: intent.operationId, ...prepared.transition })
  const binding = await verifyFSAOperationBinding({
    repository: input.repository,
    intent,
    expectedParent: authority.parent,
  })
  const session = await assembleNewFileSystemAccessOutput({
    binding,
    operationRepository: input.repository,
    rootLease,
    checkpointRepositoryFactory: input.checkpointFactory,
    ...(input.openCompatibleNameLedger === undefined
      ? {}
      : { openCompatibleNameLedger: input.openCompatibleNameLedger }),
    ...(input.openCompatibleNameLedger === undefined
      ? {}
      : {
          compatibleNamePreparation: {
            platform: 'windows',
            templateProvider: {
              select: () => Object.freeze({
                id: 'windows-powershell-v1',
                scriptFileExtension: '.ps1',
                source: '# test restoration script\n',
              }),
            },
            randomBits: () => 0,
          },
        }),
    ...(input.stageDiagnostics === undefined
      ? {}
       : { stageDiagnostics: input.stageDiagnostics }),
    ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
    ...(input.trace === undefined ? {} : { trace: input.trace }),
  })
  if (input.activate !== false) await session.activate()
  return session
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
    materialization: confirmedRecoveryMaterialization(session),
    executionProfile: disabledOutputExecutionProfile(1),
    outputIdentity: outputSessionIdentity({ backend: 'fsa-test', outputSessionId }),
    settlement: settlement.bindMaterialization(session),
  })
}

export function confirmedRecoveryMaterialization(
  session: FileSystemAccessOutputSession,
): PersistentMaterializationPort {
  return Object.freeze({
    beginFile: (request: Parameters<PersistentMaterializationPort['beginFile']>[0]) => {
      const recovery = request.recovery?.pausedFile === 'preserve'
        ? Object.freeze({
            ...request.recovery,
            costBudget: request.recovery.costBudget ?? Object.freeze({
              maximumPrefixCopyBytes: 1_024n,
              maximumCumulativeWriteAmplificationBytes: 4_096n,
              maximumPeakTemporaryBytes: 1_024n,
            }),
            confirmTemporarySpace: () => true,
          })
        : request.recovery
      return session.beginFile({
        ...request,
        ...(recovery === undefined ? {} : { recovery }),
      })
    },
    ensureDirectory: (path: Parameters<FileSystemAccessOutputSession['ensureDirectory']>[0]) =>
      session.ensureDirectory(path),
    materializeDirectory: (
      request: Parameters<FileSystemAccessOutputSession['materializeDirectory']>[0],
    ) => session.materializeDirectory(request),
    finalizeDirectory: (
      admission: Parameters<FileSystemAccessOutputSession['finalizeDirectory']>[0],
      outcome: Parameters<FileSystemAccessOutputSession['finalizeDirectory']>[1],
    ) => session.finalizeDirectory(admission, outcome),
    closeForTerminalSettlement: () => session.closeForTerminalSettlement(),
    close: () => session.close(),
  })
}

export async function rootDirectoryMaterializationRequest(
  intent: ReceiveIntent,
  generation: string,
) {
  const contract = await createDirectTreeCoordinateContract(intent)
  const expected = contract.rootExpectation
  if (expected.kind !== 'materialized-directory') {
    throw new TypeError('test intent has no materialized directory root')
  }
  const layout = contract.intent.artifact.layout
  const sourcePath = layout.kind === 'result-root' && layout.root.anchor.kind === 'directory'
    ? layout.root.anchor.sourcePath.split('/')
    : []
  const projection = contract.projectDirectory(sourcePath)
  if (projection.kind !== 'materialize' ||
      projection.relativePath.length !== expected.relativePath.length ||
      !projection.relativePath.every((segment, index) => segment === expected.relativePath[index])) {
    throw new TypeError('test root projection disagrees with its intent-derived expectation')
  }
  return Object.freeze({
    directory: Object.freeze({
      directoryId: expected.directoryId,
      generation,
      path: projection.relativePath,
    }),
    sourceAuthenticationPath: projection.sourceAuthenticationPath,
    logicalArtifactPath: projection.logicalArtifactPath,
  })
}

export async function outputFileRequest(input: Readonly<{
  intent: ReceiveIntent
  fileId: string
  fileRevision: string
  sourceRelativePath?: readonly string[]
  exactSize: bigint
  parentAdmission?: DirectoryAdmission
}>) {
  const contract = await createDirectTreeCoordinateContract(input.intent)
  const layout = contract.intent.artifact.layout
  let sourcePath: readonly string[]
  if (layout.kind === 'single-file') {
    if (input.sourceRelativePath !== undefined) {
      throw new TypeError('single-file source identity comes from its frozen receive intent')
    }
    sourcePath = layout.sourcePath.split('/')
  } else {
    if (input.sourceRelativePath === undefined) {
      throw new TypeError('directory-tree file fixture requires a source-relative path')
    }
    sourcePath = layout.kind === 'result-root' && layout.root.anchor.kind === 'directory'
      ? [...layout.root.anchor.sourcePath.split('/'), ...input.sourceRelativePath]
      : [...input.sourceRelativePath]
  }
  const projection = contract.projectFile(sourcePath)
  return Object.freeze({
    source: Object.freeze({ shareInstance: input.intent.shareInstance, fileId: input.fileId }),
    sourceAuthenticationPath: projection.sourceAuthenticationPath,
    logicalArtifactPath: projection.logicalArtifactPath,
    materializationRelativePath: projection.relativePath,
    expectedSize: input.exactSize,
    recovery: Object.freeze({
      pausedFile: 'preserve' as const,
      costBudget: Object.freeze({
        maximumPrefixCopyBytes: 1_024n,
        maximumCumulativeWriteAmplificationBytes: 4_096n,
        maximumPeakTemporaryBytes: 1_024n,
      }),
      confirmTemporarySpace: () => true,
    }),
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
    targetRouteId: offer.routeId,
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


export { memoryCheckpointFactory }
