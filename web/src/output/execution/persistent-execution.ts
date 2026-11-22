import type { PerformanceSummaryObservations } from '../../diagnostics/performance/performance-summary'
import type { FinalFileCheckpointProof } from '../persistence/journal'
import type {
  AutomaticCheckpointAdmissionAuthority,
  PersistentMaterializationPort,
  PersistentOutputNamespaceClaimPort,
  PreservingWriterCapacityAuthority,
} from '../persistent-tree/contracts'
import type { MaterializedManifestEntry } from '../workspace/manifest'
import type {
  CompatibleNameRepairSummary,
} from '../../transfer/lifecycle/name-repair'
import {
  createDirectoryAdmissionScope,
  validateDirectorySettlement,
} from '../../transfer/directory-admission'
import { DirectoryAdmissionLedger } from '../../transfer/directory-admission-ledger'
import type {
  DirectTreePlan,
  OriginalFileArtifact,
  ReceiveIntent,
  WorkspaceThenPublishPlan,
} from '../../transfer/intent'
import {
  outputCapabilities,
  outputExecutionProfile,
  outputSessionIdentity,
  snapshotDirectoryMaterializationRequest,
  snapshotOutputFileRequest,
  VerifiedDurableRanges,
  type BeginOutputFileResult,
  type DirectTreeExecution,
  type ExactSingleFileEvidence,
  type IncrementalDirectoryOutput,
  type OutputCapabilities,
  type OutputFileOwnership,
  type OutputFileRequest,
  type OutputExecutionProfile,
  type OutputSession,
  type OutputSessionIdentity,
  type WorkspaceExecution,
} from '../../transfer/output/session'
import { OutputPausedError } from '../../transfer/job/contract'
import {
  createDirectTreeCoordinateContract,
  type DirectTreeCoordinateContract,
  type MaterializationRootRelativePath,
} from '../../transfer/job/coordinate/direct-tree'
import {
  snapshotExactSingleFileEvidence,
} from '../../transfer/settlement/plan-authority'
import {
  compareMaterializedEntries,
  persistentWorkspaceInterruption,
  requireCompleteWorkspaceMaterialization,
  requireMatchingMaterializationSummary,
  sameMaterializedEntry,
  samePath,
  type PersistentDirectTreeSettlementAuthority,
  type PersistentDirectTreeMaterializationEvidence,
  type PersistentWorkspaceMaterializationEvidence,
  type PersistentWorkspaceSettlementAuthority,
  type WorkspaceMaterializationEvidence,
} from './persistent-evidence'
import {
  PersistentOutputTransaction,
  type PersistentOutputTransactionNamespace,
} from './persistent-file-transaction'
import type { PersistentExecutionRecoveryPolicy } from './persistent-recovery-policy'
import { persistentFileRevision } from './persistent-file-revision'
import { PersistentSettlementCut } from './persistent-settlement-cut'
import { WorkspaceSettlementOwner } from './workspace-settlement-owner'

export type {
  PersistentDirectTreeDirectoryEvidence,
  PersistentDirectTreeMaterializationEvidence,
  PersistentDirectTreeSettlementAuthority,
  PersistentMaterializationEvidence,
  PersistentMaterializationSettlementCut,
  PersistentWorkspaceSettlementAuthority,
  WorkspaceMaterializationEvidence,
} from './persistent-evidence'

export type { PersistentDirectorySettlementEvidence } from './persistent-evidence'
export type { PersistentExecutionRecoveryPolicy } from './persistent-recovery-policy'

type DirectTreeIntent = ReceiveIntent & Readonly<{ plan: DirectTreePlan }>
type WorkspaceOriginalIntent = ReceiveIntent & Readonly<{
  plan: WorkspaceThenPublishPlan
  artifact: OriginalFileArtifact
}>

const TEMPORARY_PERSISTENT_MAXIMUM_CONCURRENT_FILE_PIPELINES = 4
const WORKSPACE_WRITE_BUDGET_BYTES = 8n * 1024n * 1024n

type PersistentCheckpointNamespaceEvidence = PersistentOutputTransactionNamespace

export async function createPersistentDirectTreeExecution(input: {
  readonly intent: DirectTreeIntent
  readonly materialization: PersistentMaterializationPort
  readonly outputIdentity: OutputSessionIdentity
  readonly executionProfile: OutputExecutionProfile
  readonly settlement: PersistentDirectTreeSettlementAuthority
  readonly automaticCheckpointAdmission: AutomaticCheckpointAdmissionAuthority
  readonly preservingWriterCapacity: PreservingWriterCapacityAuthority
  readonly namespaceClaims?: PersistentOutputNamespaceClaimPort
  readonly capabilities?: Partial<OutputCapabilities>
  readonly repairSummary?: () => CompatibleNameRepairSummary | undefined
  readonly recovery?: PersistentExecutionRecoveryPolicy
  readonly performance?: PerformanceSummaryObservations
}): Promise<DirectTreeExecution> {
  const scope = await createDirectoryAdmissionScope(input.intent)
  const coordinates = await createDirectTreeCoordinateContract(input.intent)
  const adapter = new PersistentMaterializationOutput({
    materialization: input.materialization,
    checkpointNamespace: checkpointNamespace(input.intent),
    outputIdentity: input.outputIdentity,
    executionProfile: input.executionProfile,
    capabilities: persistentCapabilities({
      fileFailureIsolation: true,
      ...input.capabilities,
    }),
    directoryLedger: new DirectoryAdmissionLedger(scope),
    directTreeCoordinates: coordinates,
    automaticCheckpointAdmission: input.automaticCheckpointAdmission,
    preservingWriterCapacity: input.preservingWriterCapacity,
    ...(input.recovery === undefined ? {} : { recovery: input.recovery }),
    ...(input.namespaceClaims === undefined ? {} : { namespaceClaims: input.namespaceClaims }),
    ...(input.performance === undefined ? {} : { performance: input.performance }),
  })
  let terminalSettlementInitiated = false
  const stop = input.settlement.stop
  const recoverySummary = input.settlement.recoverySummary
  const execution: DirectTreeExecution = {
    planKind: 'direct-tree',
    output: adapter,
    directories: adapter.directories(),
    ...(input.performance === undefined ? {} : { performance: input.performance }),
    beginTerminal: kind => {
      terminalSettlementInitiated = true
      adapter.closeCheckpointAuthorities()
      input.settlement.beginTerminal(kind)
    },
    ...(input.repairSummary === undefined ? {} : { repairSummary: input.repairSummary }),
    ...(recoverySummary === undefined
      ? {}
      : { recoverySummary }),
    terminalSettlementInitiated: () => terminalSettlementInitiated,
    pause: async (request, signal) => {
      const cut = new PersistentSettlementCut(
        adapter.directTreeEvidence(),
        () => adapter.closeForTerminalSettlement(),
      )
      const state = await input.settlement.pause(request, cut, signal)
      await cut.validateReturnedState(state)
      return state
    },
    ...(stop === undefined ? {} : {
      stop: async (request, signal) => {
        const cut = new PersistentSettlementCut(
          adapter.directTreeEvidence(),
          committedState => committedState?.kind === 'partial-directory' && committedState.reason === 'stopped' &&
            committedState.operationId === input.intent.operationId && committedState.receiveIntentDigest === input.intent.digest
            ? adapter.closeForStopSettlement()
            : adapter.closeForTerminalSettlement(),
        )
        const state = await stop(request, cut, signal)
        await cut.validateReturnedState(state)
        return state
      },
    }),
    settle: async (request, signal) => {
      const cut = new PersistentSettlementCut(
        adapter.directTreeEvidence(),
        () => adapter.closeForTerminalSettlement(),
      )
      const state = await input.settlement.settle(request, cut, signal)
      await cut.validateReturnedState(state)
      return state
    },
  }
  return Object.freeze(execution)
}

interface PersistentWorkspaceExecutionInputBase {
  readonly materialization: PersistentMaterializationPort
  readonly outputIdentity: OutputSessionIdentity
  readonly settlement: PersistentWorkspaceSettlementAuthority
  readonly signal: AbortSignal
  readonly capabilities?: Partial<OutputCapabilities>
}

export type PersistentWorkspaceExecutionInput = PersistentWorkspaceExecutionInputBase & Readonly<{
  intent: WorkspaceOriginalIntent
  admission: Readonly<{ kind: 'single-file'; evidence: ExactSingleFileEvidence }>
}>

export async function createPersistentWorkspaceExecution(
  input: PersistentWorkspaceExecutionInput,
): Promise<WorkspaceExecution> {
  const admission = Object.freeze({
    kind: 'single-file' as const,
    evidence: snapshotExactSingleFileEvidence(input.intent, input.admission.evidence),
  })
  const adapter = new PersistentMaterializationOutput({
    materialization: input.materialization,
    checkpointNamespace: checkpointNamespace(input.intent),
    outputIdentity: input.outputIdentity,
    executionProfile: outputExecutionProfile({
      maximumConcurrentFilePipelines: TEMPORARY_PERSISTENT_MAXIMUM_CONCURRENT_FILE_PIPELINES,
      maximumOutstandingWriteBytes: WORKSPACE_WRITE_BUDGET_BYTES,
      maximumBufferedBytes: WORKSPACE_WRITE_BUDGET_BYTES,
    }),
    capabilities: persistentCapabilities({
      fileFailureIsolation: false,
      ...input.capabilities,
    }),
  })
  const generations = Object.freeze([Object.freeze({
    directoryId: admission.evidence.containingDirectoryId,
    generation: admission.evidence.generation,
  })])
  const evidence = (): WorkspaceMaterializationEvidence => {
    const materialized = adapter.workspaceEvidence()
    return Object.freeze({
      kind: 'workspace-manifest' as const,
      generations,
      entries: materialized.entries,
      directorySettlements: materialized.directorySettlements,
    })
  }
  const owner = new WorkspaceSettlementOwner()
  const execution: WorkspaceExecution = {
    planKind: 'workspace-then-publish',
    output: adapter,
    pause: (request, signal) => owner.pause(async () => {
      const cut = new PersistentSettlementCut(evidence(), () => adapter.close())
      const state = await input.settlement.interrupt(persistentWorkspaceInterruption(request), cut, signal)
      await cut.validateReturnedState(state)
      return state
    }),
    settle: async (request, signal) => {
      const snapshot = evidence()
      requireCompleteWorkspaceMaterialization(input.intent, admission, snapshot)
      requireMatchingMaterializationSummary(request, snapshot)
      return owner.settle(async () => {
        const cut = new PersistentSettlementCut(snapshot, () => adapter.close())
        const state = await input.settlement.settle(request, cut, signal)
        await cut.validateReturnedState(state)
        return state
      })
    },
  }
  return Object.freeze(execution)
}

class PersistentMaterializationOutput implements OutputSession {
  readonly identity: OutputSessionIdentity
  readonly capabilities: OutputCapabilities
  readonly executionProfile: OutputExecutionProfile
  readonly #materialization: PersistentMaterializationPort
  readonly #checkpointNamespace: PersistentCheckpointNamespaceEvidence
  readonly #directoryLedger: DirectoryAdmissionLedger | undefined
  readonly #namespaceClaims: PersistentOutputNamespaceClaimPort | undefined
  readonly #directTreeCoordinates: DirectTreeCoordinateContract | undefined
  readonly #recovery: PersistentExecutionRecoveryPolicy
  readonly #automaticCheckpointAdmission: AutomaticCheckpointAdmissionAuthority | undefined
  readonly #preservingWriterCapacity: PreservingWriterCapacityAuthority | undefined
  readonly #performance: PerformanceSummaryObservations | undefined
  readonly #entries = new Map<string, MaterializedManifestEntry>()
  readonly #directoryPathByAdmission = new Map<string, MaterializationRootRelativePath>()
  readonly #settledDirectoryByAdmission = new Map<string, 'finalized' | 'isolated-failure'>()
  #rootDirectory: Readonly<{
    readonly directoryId: string
    readonly relativePath: MaterializationRootRelativePath
    finalized: boolean
  }> | undefined
  #closePromise: Promise<void> | undefined

  constructor(input: {
    readonly materialization: PersistentMaterializationPort
    readonly checkpointNamespace: PersistentCheckpointNamespaceEvidence
    readonly outputIdentity: OutputSessionIdentity
    readonly executionProfile: OutputExecutionProfile
    readonly capabilities: OutputCapabilities
    readonly directoryLedger?: DirectoryAdmissionLedger
    readonly namespaceClaims?: PersistentOutputNamespaceClaimPort
    readonly directTreeCoordinates?: DirectTreeCoordinateContract
    readonly recovery?: PersistentExecutionRecoveryPolicy
    readonly automaticCheckpointAdmission?: AutomaticCheckpointAdmissionAuthority
    readonly preservingWriterCapacity?: PreservingWriterCapacityAuthority
    readonly performance?: PerformanceSummaryObservations
  }) {
    this.#materialization = input.materialization
    this.#checkpointNamespace = Object.freeze({ ...input.checkpointNamespace })
    this.identity = outputSessionIdentity(input.outputIdentity)
    this.capabilities = outputCapabilities(input.capabilities)
    this.executionProfile = outputExecutionProfile(input.executionProfile)
    this.#directoryLedger = input.directoryLedger
    this.#namespaceClaims = input.namespaceClaims
    this.#directTreeCoordinates = input.directTreeCoordinates
    this.#recovery = input.recovery ?? Object.freeze({ pausedFile: 'preserve' as const })
    this.#automaticCheckpointAdmission = input.automaticCheckpointAdmission
    this.#preservingWriterCapacity = input.preservingWriterCapacity
    if ((this.#automaticCheckpointAdmission === undefined) !==
        (this.#preservingWriterCapacity === undefined)) {
      throw new TypeError('Persistent checkpoint authorities must be bound as one attempt pair')
    }
    this.#performance = input.performance
  }

  directories(): IncrementalDirectoryOutput {
    const ledger = this.#directoryLedger
    if (ledger === undefined) throw new TypeError('persistent output has no incremental directory authority')
    const directories: IncrementalDirectoryOutput = {
      admitDirectory: async (request, signal) => {
        const snapshot = snapshotDirectoryMaterializationRequest(request)
        const materializationRelativePath = snapshot.directory.path
        const admission = await ledger.admitDirectory(
          snapshot.directory,
          signal,
          async () => {
            if (this.#namespaceClaims !== undefined) {
              if (snapshot.logicalSiblingMembership === undefined) {
                throw new TypeError(
                  'persistent namespace claims require authenticated logical-sibling membership',
                )
              }
              this.#namespaceClaims.bindDirectoryNamespace(Object.freeze({
                materializationRelativePath,
                logicalSiblingMembership: snapshot.logicalSiblingMembership,
              }))
            }
            await this.#materialization.ensureDirectory(materializationRelativePath)
              .catch((cause: unknown) => {
                // Directory writes need the same output-wide state-I/O boundary as file writes;
                // otherwise an untyped browser rejection is mistaken for a collaborator contract bug.
                throw new OutputPausedError('Persistent directory materialization failed', { cause })
              })
          },
        )
        if (!samePath(admission.path, materializationRelativePath)) {
          throw new TypeError('directory admission path disagrees with materialized relative path')
        }
        const existingPath = this.#directoryPathByAdmission.get(admission.admissionId)
        if (existingPath !== undefined && !samePath(existingPath, materializationRelativePath)) {
          throw new TypeError('directory admission changed its materialized relative path')
        }
        this.#directoryPathByAdmission.set(admission.admissionId, materializationRelativePath)
        if (admission.parentAdmissionId === undefined && this.#rootDirectory === undefined) {
          this.#rootDirectory = {
            directoryId: admission.directoryId,
            relativePath: materializationRelativePath,
            finalized: false,
          }
        }
        return admission
      },
      finalizeDirectory: async (admission, signal) => {
        const settlement = validateDirectorySettlement(
          admission,
          await ledger.finalizeDirectory(admission, signal),
        )
        const materializationRelativePath = this.#directoryPathByAdmission.get(admission.admissionId)
        if (materializationRelativePath === undefined) {
          throw new TypeError('directory settlement has no materialized relative-path binding')
        }
        if (!samePath(admission.path, materializationRelativePath)) {
          throw new TypeError('directory settlement admission changed its materialized relative path')
        }
        this.#settledDirectoryByAdmission.set(admission.admissionId, settlement.kind)
        if (admission.parentAdmissionId === undefined && this.#rootDirectory !== undefined &&
            settlement.kind === 'finalized') {
          this.#rootDirectory = { ...this.#rootDirectory, finalized: true }
        }
        return settlement
      },
    }
    return Object.freeze(directories)
  }

  beginFile(input: OutputFileRequest, signal: AbortSignal): Promise<BeginOutputFileResult> {
    return this.#beginFile(snapshotOutputFileRequest(input, this.#directTreeCoordinates), this.#recovery, false, signal)
  }

  async readmitFile(input: OutputFileRequest, paused: VerifiedDurableRanges, signal: AbortSignal): Promise<BeginOutputFileResult> {
    const request = snapshotOutputFileRequest(input, this.#directTreeCoordinates)
    if (!(paused instanceof VerifiedDurableRanges) ||
        paused.ownership.backend !== this.identity.backend ||
        paused.ownership.outputSessionId !== this.identity.outputSessionId ||
        paused.source.shareInstance !== request.source.shareInstance ||
        paused.source.fileId !== request.source.fileId ||
        paused.fileSize !== request.expectedSize ||
        !samePath(paused.ownership.canonicalPath, request.materializationRelativePath)) {
      throw new TypeError('paused attempt does not belong to this output file')
    }
    return this.#beginFile(request, { pausedFile: 'preserve' }, true, signal)
  }

  async #beginFile(
    request: OutputFileRequest, recovery: PersistentExecutionRecoveryPolicy,
    requireRevisionOpen: boolean, signal: AbortSignal,
  ): Promise<BeginOutputFileResult> {
    signal.throwIfAborted()
    const automaticCheckpointAdmission = this.#automaticCheckpointAdmission?.enrollFile(
      request.materializationRelativePath,
    )
    const mustOpenSource = requireRevisionOpen || this.capabilities.durability === 'None'
    const admission = persistentFileRevision(request, signal, this.#performance)
    const mutation = request.parentAdmission === undefined || this.#directoryLedger === undefined
      ? undefined
      : this.#directoryLedger.acquireFileMutation({
          path: request.materializationRelativePath,
          parentAdmission: request.parentAdmission,
        })
    try {
      const transaction = await this.#materialization.beginFile({
        fileId: request.source.fileId,
        expectedSize: request.expectedSize,
        sourceAuthenticationPath: request.sourceAuthenticationPath,
        materializationRelativePath: request.materializationRelativePath,
        shareInstance: request.source.shareInstance,
        outputSession: this.identity,
        recovery,
        ...(mustOpenSource ? { requireRevisionOpen: true } : {}),
        ...(automaticCheckpointAdmission === undefined
          ? {}
          : { automaticCheckpointAdmission }),
        ...(this.#preservingWriterCapacity === undefined
          ? {}
          : { preservingWriterCapacity: this.#preservingWriterCapacity }),
        ...(request.performancePipeline === undefined
          ? {}
          : { performancePipeline: request.performancePipeline }),
        openRevision: admission.openRevision,
      })
      // Return the opened transaction even if cancellation won the storage
      // race so its owner can settle and drain it.
      try {
        const ownership: OutputFileOwnership = Object.freeze({
          ...this.identity,
          canonicalPath: request.materializationRelativePath,
          ownedFileIdentity: transaction.ownedObjectId,
        })
        const { revision, durableRanges } = admission.bind(transaction, ownership, !mustOpenSource)
        return Object.freeze({
          revision,
          durableRanges,
          ...(transaction.checkpointPolicy === undefined ? {} : { checkpoint: {
            objectId: transaction.checkpointObjectId ?? transaction.ownedObjectId,
            policy: transaction.checkpointPolicy,
          } }),
          transaction: new PersistentOutputTransaction({
            transaction,
            revision,
            ownership,
            checkpointNamespace: this.#checkpointNamespace,
            isolated: this.capabilities.fileFailureIsolation,
            releaseMutation: () => mutation?.release(),
            recordProof: proof => this.#recordFile(proof),
          }),
        })
      } catch (error) {
        await transaction.retire(error).catch(() => undefined)
        throw error
      }
    } catch (error) {
      automaticCheckpointAdmission?.retire('unused')
      mutation?.release()
      throw error
    }
  }

  directTreeEvidence(): PersistentDirectTreeMaterializationEvidence {
    if (this.#directTreeCoordinates === undefined) {
      throw new TypeError('Workspace output cannot expose DirectTree checkpoint evidence')
    }
    let visibleCount = 0n
    for (const path of this.#directoryPathByAdmission.values()) {
      if (path.length > 0) visibleCount += 1n
    }
    let finalizedCount = 0n
    let isolatedCount = 0n
    for (const kind of this.#settledDirectoryByAdmission.values()) {
      if (kind === 'finalized') finalizedCount += 1n
      else isolatedCount += 1n
    }
    return Object.freeze({
      kind: 'direct-tree-checkpoints' as const,
      materializationBindingDigest: this.#checkpointNamespace.materializationBindingDigest,
      directories: Object.freeze({
        materializedCount: BigInt(this.#directoryPathByAdmission.size),
        visibleCount,
        finalizedCount,
        isolatedCount,
        ...(this.#rootDirectory === undefined ? {} : {
          rootDirectory: Object.freeze({ ...this.#rootDirectory }),
        }),
      }),
    })
  }

  workspaceEvidence(): PersistentWorkspaceMaterializationEvidence {
    if (this.#directTreeCoordinates !== undefined) {
      throw new TypeError('DirectTree output cannot expose Workspace manifest evidence')
    }
    const entries = [...this.#entries.values()].sort(compareMaterializedEntries)
    return Object.freeze({
      kind: 'workspace-manifest' as const,
      entries: Object.freeze(entries),
      directorySettlements: Object.freeze([]),
    })
  }

  close(): Promise<void> {
    this.closeCheckpointAuthorities()
    this.#closePromise ??= this.#materialization.close()
    return this.#closePromise
  }

  closeForTerminalSettlement(): Promise<void> {
    this.closeCheckpointAuthorities()
    this.#closePromise ??= this.#materialization.closeForTerminalSettlement?.() ??
      this.#materialization.close()
    return this.#closePromise
  }

  closeForStopSettlement(): Promise<void> {
    this.closeCheckpointAuthorities()
    this.#closePromise ??= this.#materialization.closeForStopSettlement?.() ??
      this.#materialization.closeForTerminalSettlement?.() ?? this.#materialization.close()
    return this.#closePromise
  }

  closeCheckpointAuthorities(): void {
    this.#automaticCheckpointAdmission?.close('terminal-drain')
    this.#preservingWriterCapacity?.close('terminal-drain')
  }

  #recordFile(proof: FinalFileCheckpointProof): void {
    if (this.#directTreeCoordinates !== undefined) return
    this.#recordEntry(Object.freeze({
      kind: 'file',
      artifactPath: Object.freeze([...proof.canonicalPath]),
      fileId: proof.fileId,
      fileRevision: proof.fileRevision,
      exactSize: proof.exactSize,
      ownedObjectId: proof.ownedObjectId,
      checkpoint: Object.freeze({
        recordId: proof.recordId,
        recordDigest: proof.recordDigest,
        checkpointGeneration: proof.checkpointGeneration,
      }),
    }))
  }

  #recordEntry(entry: MaterializedManifestEntry): void {
    const key = JSON.stringify(entry.artifactPath)
    const existing = this.#entries.get(key)
    if (existing !== undefined) {
      if (!sameMaterializedEntry(existing, entry)) {
        throw new TypeError('persistent materialization path changed ownership')
      }
      return
    }
    this.#entries.set(key, entry)
  }
}


function persistentCapabilities(
  input: Partial<OutputCapabilities> & Pick<OutputCapabilities, 'fileFailureIsolation'>,
): OutputCapabilities {
  return outputCapabilities({
    durability: input.durability ?? 'ProcessRestart',
    randomWrite: input.randomWrite ?? true,
    fileFailureIsolation: input.fileFailureIsolation,
    modificationTime: input.modificationTime ?? false,
  })
}

function checkpointNamespace(
  intent: DirectTreeIntent | WorkspaceOriginalIntent,
): PersistentCheckpointNamespaceEvidence {
  return Object.freeze({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.kind === 'direct-tree'
      ? intent.plan.reservation.digest
      : intent.plan.workspace.digest,
  })
}
