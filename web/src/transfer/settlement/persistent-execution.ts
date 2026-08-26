import type { FinalFileCheckpointProof } from '../../output/persistence/journal'
import {
  observePerformance,
  performanceElapsedMilliseconds,
  performanceNowMilliseconds,
  type PerformanceSummaryObservations,
} from '../../output/diagnostics/performance-summary'
import { beginPerformanceRevisionOpen } from '../../output/diagnostics/performance-runtime-observations'
import type {
  AutomaticCheckpointAdmissionAuthority,
  PersistentDirectoryLedgerMaterialization,
  PersistentFileTransactionPort,
  PersistentMaterializationPort,
  PersistentOutputNamespaceClaimPort,
  PreservingWriterCapacityAuthority,
} from '../../output/persistent-tree/contracts'
import { MaterializationLedgerDirectoryOutcome } from '../../output/materialization-ledger/model'
import type { MaterializedManifestEntry } from '../../output/workspace/manifest'
import type { PreparationManifestEntry } from '../../output/workspace/preparation'
import type { CompatibleNameRepairSummary } from '../../output/file-system-access/compatible-name/model'
import {
  createDirectoryAdmissionScope,
  validateDirectorySettlement,
} from '../directory-admission'
import { DirectoryAdmissionLedger } from '../directory-admission-ledger'
import type {
  DirectTreePlan,
  OriginalFileArtifact,
  ReceiveIntent,
  WorkspaceThenPublishPlan,
  ZipArchiveArtifact,
} from '../intent'
import {
  disabledOutputExecutionProfile,
  outputCapabilities,
  outputExecutionProfile,
  outputSessionIdentity,
  snapshotOpenedOutputRevision,
  snapshotDirectoryMaterializationRequest,
  snapshotOutputFileRequest,
  type BeginOutputFileResult,
  type DirectTreeExecution,
  type ExactPreparationEvidence,
  type ExactSingleFileEvidence,
  type IncrementalDirectoryOutput,
  type OpenedOutputRevision,
  type OutputCapabilities,
  type OutputFileOwnership,
  type OutputFileRequest,
  type OutputExecutionProfile,
  type OutputSession,
  type OutputSessionIdentity,
  type WorkspaceExecution,
} from '../output-session'
import { V2OutputPausedError } from '../job/contract'
import {
  createDirectTreeCoordinateContract,
  snapshotMaterializationRootRelativePath,
  type DirectTreeCoordinateContract,
  type MaterializationRootRelativePath,
} from '../job/coordinate/direct-tree'
import {
  snapshotExactPreparationEvidence,
  snapshotExactSingleFileEvidence,
} from './v2-plan-authority'
import {
  compareMaterializedEntries,
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
  verifiedPersistentRanges,
  type PersistentOutputTransactionNamespace,
} from './persistent-file-transaction'
import type { PersistentExecutionRecoveryPolicy } from './persistent-recovery-policy'
import { PersistentSettlementCut } from './persistent-settlement-cut'

export type {
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
type WorkspaceZipIntent = ReceiveIntent & Readonly<{
  plan: WorkspaceThenPublishPlan
  artifact: ZipArchiveArtifact
}>

const TEMPORARY_PERSISTENT_MAXIMUM_CONCURRENT_FILE_PIPELINES = 4

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
          () => adapter.closeForTerminalSettlement(),
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

export type PersistentWorkspaceExecutionInput = PersistentWorkspaceExecutionInputBase & (
  | Readonly<{
      intent: WorkspaceOriginalIntent
      admission: Readonly<{ kind: 'single-file'; evidence: ExactSingleFileEvidence }>
    }>
  | Readonly<{
      intent: WorkspaceZipIntent
      admission: Readonly<{ kind: 'prepared'; evidence: ExactPreparationEvidence }>
    }>
)

export async function createPersistentWorkspaceExecution(
  input: PersistentWorkspaceExecutionInput,
): Promise<WorkspaceExecution> {
  let admission:
    | Readonly<{ kind: 'single-file'; evidence: ExactSingleFileEvidence }>
    | Readonly<{ kind: 'prepared'; evidence: ExactPreparationEvidence }>
  if (input.intent.artifact.kind === 'original-file') {
    if (input.admission.kind !== 'single-file') {
      throw new TypeError('Workspace OriginalFile requires exact single-file admission')
    }
    admission = Object.freeze({
      kind: 'single-file' as const,
      evidence: snapshotExactSingleFileEvidence(
        input.intent as WorkspaceOriginalIntent,
        input.admission.evidence,
      ),
    })
  } else {
    if (input.admission.kind !== 'prepared') {
      throw new TypeError('Workspace ZIP requires sealed preparation evidence')
    }
    admission = Object.freeze({
      kind: 'prepared' as const,
      evidence: snapshotExactPreparationEvidence(input.admission.evidence),
    })
  }
  const adapter = new PersistentMaterializationOutput({
    materialization: input.materialization,
    checkpointNamespace: checkpointNamespace(input.intent),
    outputIdentity: input.outputIdentity,
    executionProfile: disabledOutputExecutionProfile(
      TEMPORARY_PERSISTENT_MAXIMUM_CONCURRENT_FILE_PIPELINES,
    ),
    capabilities: persistentCapabilities({
      fileFailureIsolation: false,
      ...input.capabilities,
    }),
  })
  const generations = admission.kind === 'prepared'
    ? admission.evidence.generations
    : Object.freeze([Object.freeze({
        directoryId: admission.evidence.containingDirectoryId,
        generation: admission.evidence.generation,
      })])
  if (admission.kind === 'prepared') {
    try {
      await adapter.materializePreparedDirectories(admission.evidence.entries, input.signal)
    } catch (cause) {
      try {
        await adapter.close()
      } catch (releaseFailure) {
        throw new AggregateError(
          [cause, releaseFailure],
          'prepared workspace materialization and resource release both failed',
          { cause: releaseFailure },
        )
      }
      throw cause
    }
  }
  const evidence = (): WorkspaceMaterializationEvidence => {
    const materialized = adapter.workspaceEvidence()
    return Object.freeze({
      kind: 'workspace-manifest' as const,
      generations,
      entries: materialized.entries,
      directorySettlements: materialized.directorySettlements,
    })
  }
  const execution: WorkspaceExecution = {
    planKind: 'workspace-then-publish',
    output: adapter,
    pause: async (request, signal) => {
      const cut = new PersistentSettlementCut(evidence(), () => adapter.close())
      const state = await input.settlement.pause(request, cut, signal)
      await cut.validateReturnedState(state)
      return state
    },
    settle: async (request, signal) => {
      const snapshot = evidence()
      requireCompleteWorkspaceMaterialization(input.intent, admission, snapshot)
      requireMatchingMaterializationSummary(request, snapshot)
      const cut = new PersistentSettlementCut(snapshot, () => adapter.close())
      const state = await input.settlement.settle(request, cut, signal)
      await cut.validateReturnedState(state)
      return state
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
  readonly #directoryLedgerByAdmission = new Map<string, PersistentDirectoryLedgerMaterialization>()
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
    if (this.#directTreeCoordinates !== undefined &&
        (input.materialization.materializeDirectory === undefined ||
         input.materialization.finalizeDirectory === undefined)) {
      throw new TypeError('DirectTree persistent output requires durable directory ledger authority')
    }
  }

  directories(): IncrementalDirectoryOutput {
    const ledger = this.#directoryLedger
    if (ledger === undefined) throw new TypeError('persistent output has no incremental directory authority')
    const directories: IncrementalDirectoryOutput = {
      admitDirectory: async (request, signal) => {
        const snapshot = snapshotDirectoryMaterializationRequest(request)
        const materializationRelativePath = snapshot.directory.path
        let durableDirectory: PersistentDirectoryLedgerMaterialization | undefined
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
            const parent = snapshot.directory.parentAdmission === undefined
              ? undefined
              : this.#directoryLedgerByAdmission.get(snapshot.directory.parentAdmission.token)
            if (snapshot.directory.parentAdmission !== undefined && parent === undefined) {
              throw new TypeError('directory ledger parent has no durable admission')
            }
            const materialized = await this.#materialization.materializeDirectory!({
              relativePath: materializationRelativePath,
              directoryId: snapshot.directory.directoryId,
              generation: snapshot.directory.generation,
              ...(snapshot.directory.modifiedTime === undefined
                ? {}
                : { modifiedTime: snapshot.directory.modifiedTime }),
              ...(parent === undefined
                ? {}
                : {
                    parent: Object.freeze({
                      relativePath: parent.ledgerAdmission.relativePath,
                      directoryId: parent.ledgerAdmission.directoryId,
                      generation: parent.ledgerAdmission.generation,
                      ownedObjectId: parent.ownedObjectId,
                    }),
                  }),
            })
              .catch((cause: unknown) => {
                // Directory writes need the same output-wide state-I/O boundary as file writes;
                // otherwise an untyped browser rejection is mistaken for a collaborator contract bug.
                throw new V2OutputPausedError('Persistent directory materialization failed', { cause })
              })
            durableDirectory = materialized
          },
        )
        if (durableDirectory !== undefined) {
          this.#directoryLedgerByAdmission.set(admission.token, durableDirectory)
        } else if (!this.#directoryLedgerByAdmission.has(admission.token)) {
          throw new TypeError('directory admission has no durable ledger entry')
        }
        if (!samePath(admission.path, materializationRelativePath)) {
          throw new TypeError('directory admission path disagrees with materialized relative path')
        }
        const existingPath = this.#directoryPathByAdmission.get(admission.token)
        if (existingPath !== undefined && !samePath(existingPath, materializationRelativePath)) {
          throw new TypeError('directory admission changed its materialized relative path')
        }
        this.#directoryPathByAdmission.set(admission.token, materializationRelativePath)
        return admission
      },
      finalizeDirectory: async (admission, signal) => {
        const settlement = validateDirectorySettlement(
          admission,
          await ledger.finalizeDirectory(admission, signal),
        )
        const durableDirectory = this.#directoryLedgerByAdmission.get(admission.token)
        if (durableDirectory === undefined) {
          throw new TypeError('directory finalization has no durable ledger admission')
        }
        await this.#materialization.finalizeDirectory!(
          durableDirectory.ledgerAdmission,
          settlement.kind === 'finalized'
            ? Object.freeze({ kind: MaterializationLedgerDirectoryOutcome.Finalized })
            : Object.freeze({
                kind: MaterializationLedgerDirectoryOutcome.IsolatedFailure,
                fault: settlement.fault,
              }),
        )
        const materializationRelativePath = this.#directoryPathByAdmission.get(admission.token)
        if (materializationRelativePath === undefined) {
          throw new TypeError('directory settlement has no materialized relative-path binding')
        }
        if (!samePath(admission.path, materializationRelativePath)) {
          throw new TypeError('directory settlement admission changed its materialized relative path')
        }
        if (durableDirectory.ledgerAdmission.directoryId !== admission.directoryId ||
            durableDirectory.ledgerAdmission.generation !== admission.generation ||
            !samePath(durableDirectory.ledgerAdmission.relativePath, materializationRelativePath)) {
          throw new TypeError('directory settlement escaped its materialized ownership proof')
        }
        return settlement
      },
    }
    return Object.freeze(directories)
  }

  async materializePreparedDirectories(
    entries: readonly PreparationManifestEntry[],
    signal: AbortSignal,
  ): Promise<void> {
    for (const entry of entries) {
      if (entry.kind !== 'directory') continue
      signal.throwIfAborted()
      const materializationRelativePath = snapshotMaterializationRootRelativePath(
        entry.artifactPath,
      )
      const materialized = await this.#materialization.ensureDirectory(materializationRelativePath)
      this.#recordDirectory({
        kind: 'directory',
        artifactPath: materializationRelativePath,
        directoryId: entry.directoryId,
        generation: entry.generation,
        ownedObjectId: materialized.ownedObjectId,
      })
    }
  }

  async beginFile(input: OutputFileRequest, signal: AbortSignal): Promise<BeginOutputFileResult> {
    signal.throwIfAborted()
    const request = snapshotOutputFileRequest(input, this.#directTreeCoordinates)
    const automaticCheckpointAdmission = this.#automaticCheckpointAdmission?.enrollFile(
      request.materializationRelativePath,
    )
    const revisionQueuedAtMilliseconds = performanceNowMilliseconds(this.#performance)
    const mutation = request.parentAdmission === undefined || this.#directoryLedger === undefined
      ? undefined
      : this.#directoryLedger.acquireFileMutation({
          path: request.materializationRelativePath,
          parentAdmission: request.parentAdmission,
        })
    let callbackInvoked = false
    let opened: OpenedOutputRevision | undefined
    try {
      const transaction = await this.#materialization.beginFile({
        materializationRelativePath: request.materializationRelativePath,
        shareInstance: request.source.shareInstance,
        outputSession: this.identity,
        recovery: Object.freeze({
          pausedFile: this.#recovery.pausedFile,
        }),
        ...(automaticCheckpointAdmission === undefined
          ? {}
          : { automaticCheckpointAdmission }),
        ...(this.#preservingWriterCapacity === undefined
          ? {}
          : { preservingWriterCapacity: this.#preservingWriterCapacity }),
        ...(request.performancePipeline === undefined
          ? {}
          : { performancePipeline: request.performancePipeline }),
        openRevision: async () => {
          if (callbackInvoked) {
            throw new TypeError('persistent materializer invoked revision authority more than once')
          }
          callbackInvoked = true
          observePerformance(this.#performance, summary =>
            summary.markMilestone('first_content_request'))
          const waitMilliseconds = performanceElapsedMilliseconds(
            revisionQueuedAtMilliseconds,
            performanceNowMilliseconds(this.#performance),
          )
          const revisionObservation = beginPerformanceRevisionOpen(
            this.#performance,
            waitMilliseconds,
          )
          let succeeded = false
          try {
            opened = snapshotOpenedOutputRevision(await request.openRevision(signal))
            requireMatchingOpenedRevision(request, opened)
            succeeded = true
            return Object.freeze({
              fileId: opened.fileId,
              fileRevision: opened.fileRevision,
              exactSize: opened.exactSize,
            })
          } finally {
            revisionObservation?.finish(succeeded)
          }
        },
      })
      signal.throwIfAborted()
      if (!callbackInvoked || opened === undefined) {
        throw new TypeError('persistent materializer bypassed authenticated revision authority')
      }
      requireMatchingPersistentRevision(opened, transaction)
      const ownership: OutputFileOwnership = Object.freeze({
        ...this.identity,
        canonicalPath: request.materializationRelativePath,
        ownedFileIdentity: transaction.ownedObjectId,
      })
      const durableRanges = verifiedPersistentRanges(
        ownership,
        opened,
        transaction.initialDurableRanges,
      )
      return Object.freeze({
        revision: opened,
        durableRanges,
        transaction: new PersistentOutputTransaction({
          transaction,
          revision: opened,
          ownership,
          checkpointNamespace: this.#checkpointNamespace,
          isolated: this.capabilities.fileFailureIsolation,
          releaseMutation: () => mutation?.release(),
          recordProof: proof => this.#recordFile(proof),
        }),
      })
    } catch (error) {
      automaticCheckpointAdmission?.retire('unused')
      mutation?.release()
      throw error
    }
  }

  directTreeEvidence(): PersistentDirectTreeMaterializationEvidence {
    if (this.#directTreeCoordinates === undefined) {
      throw new TypeError('Workspace output cannot expose DirectTree ledger evidence')
    }
    return Object.freeze({
      kind: 'direct-tree-ledger' as const,
      materializationBindingDigest: this.#checkpointNamespace.materializationBindingDigest,
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

  closeCheckpointAuthorities(): void {
    this.#automaticCheckpointAdmission?.close('terminal-drain')
    this.#preservingWriterCapacity?.close('terminal-drain')
  }

  #recordDirectory(entry: Extract<MaterializedManifestEntry, { kind: 'directory' }>): void {
    this.#recordEntry(Object.freeze({ ...entry, artifactPath: Object.freeze([...entry.artifactPath]) }))
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

function requireMatchingOpenedRevision(
  request: OutputFileRequest,
  revision: OpenedOutputRevision,
): void {
  if (revision.shareInstance !== request.source.shareInstance ||
      revision.fileId !== request.source.fileId || revision.exactSize !== request.expectedSize) {
    throw new TypeError('persistent revision does not match the requested catalog file')
  }
}

function requireMatchingPersistentRevision(
  revision: OpenedOutputRevision,
  transaction: PersistentFileTransactionPort,
): void {
  if (transaction.revision.fileId !== revision.fileId ||
      transaction.revision.fileRevision !== revision.fileRevision ||
      transaction.revision.exactSize !== revision.exactSize) {
    throw new TypeError('persistent transaction returned another authenticated revision')
  }
}

function checkpointNamespace(
  intent: DirectTreeIntent | WorkspaceOriginalIntent | WorkspaceZipIntent,
): PersistentCheckpointNamespaceEvidence {
  return Object.freeze({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.kind === 'direct-tree'
      ? intent.plan.reservation.digest
      : intent.plan.workspace.digest,
  })
}
