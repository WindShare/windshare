import type { FinalFileCheckpointProof } from '../../output/persistence/journal'
import type {
  PersistentFileTransactionPort,
  PersistentMaterializationPort,
} from '../../output/persistent-tree/contracts'
import type { MaterializedManifestEntry } from '../../output/workspace/manifest'
import type { PreparationManifestEntry } from '../../output/workspace/preparation'
import type { ReceiveLifecycleState } from '../../output/workspace/state'
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
  VerifiedDurableRanges,
  outputCapabilities,
  outputSessionIdentity,
  snapshotOpenedOutputRevision,
  snapshotOutputFileRequest,
  type BeginOutputFileResult,
  type DirectTreeExecution,
  type ExactPreparationEvidence,
  type ExactSingleFileEvidence,
  type FileRetirementDisposition,
  type IncrementalDirectoryOutput,
  type OpenedOutputRevision,
  type OutputCapabilities,
  type OutputFileOwnership,
  type OutputFileRequest,
  type OutputFileTransaction,
  type OutputSession,
  type OutputSessionIdentity,
  type WorkspaceExecution,
} from '../output-session'
import {
  snapshotExactPreparationEvidence,
  snapshotExactSingleFileEvidence,
} from './v2-plan-authority'
import {
  compareDirectorySettlementEvidence,
  compareMaterializedEntries,
  requireCompleteDirectorySettlement,
  requireCompleteWorkspaceMaterialization,
  requireMatchingMaterializationSummary,
  sameMaterializedEntry,
  samePath,
  type PersistentDirectTreeSettlementAuthority,
  type PersistentDirectorySettlementEvidence,
  type PersistentMaterializationEvidence,
  type PersistentMaterializationSettlementCut,
  type PersistentWorkspaceSettlementAuthority,
  type WorkspaceMaterializationEvidence,
} from './persistent-evidence'

export type {
  PersistentDirectTreeSettlementAuthority,
  PersistentDirectorySettlementEvidence,
  PersistentMaterializationEvidence,
  PersistentMaterializationSettlementCut,
  PersistentWorkspaceSettlementAuthority,
  WorkspaceMaterializationEvidence,
} from './persistent-evidence'

type DirectTreeIntent = ReceiveIntent & Readonly<{ plan: DirectTreePlan }>
type WorkspaceOriginalIntent = ReceiveIntent & Readonly<{
  plan: WorkspaceThenPublishPlan
  artifact: OriginalFileArtifact
}>
type WorkspaceZipIntent = ReceiveIntent & Readonly<{
  plan: WorkspaceThenPublishPlan
  artifact: ZipArchiveArtifact
}>

interface PersistentCheckpointNamespaceEvidence {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly materializationBindingDigest: string
}

export async function createPersistentDirectTreeExecution(input: {
  readonly intent: DirectTreeIntent
  readonly materialization: PersistentMaterializationPort
  readonly outputIdentity: OutputSessionIdentity
  readonly settlement: PersistentDirectTreeSettlementAuthority
  readonly capabilities?: Partial<OutputCapabilities>
}): Promise<DirectTreeExecution> {
  const scope = await createDirectoryAdmissionScope(input.intent)
  const adapter = new PersistentMaterializationOutput({
    materialization: input.materialization,
    checkpointNamespace: checkpointNamespace(input.intent),
    outputIdentity: input.outputIdentity,
    capabilities: persistentCapabilities({
      fileFailureIsolation: true,
      ...input.capabilities,
    }),
    directoryLedger: new DirectoryAdmissionLedger(scope),
  })
  const execution: DirectTreeExecution = {
    planKind: 'direct-tree',
    output: adapter,
    directories: adapter.directories(),
    pause: async (request, signal) => {
      const cut = new PersistentSettlementCut(adapter.evidence(), () => adapter.close())
      const state = await input.settlement.pause(request, cut, signal)
      await cut.validateReturnedState(state)
      return state
    },
    settle: async (request, signal) => {
      const evidence = adapter.evidence()
      requireMatchingMaterializationSummary(request, evidence)
      if (request.worker.status === 'Succeeded') requireCompleteDirectorySettlement(evidence)
      const cut = new PersistentSettlementCut(evidence, () => adapter.close())
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
    const materialized = adapter.evidence()
    return Object.freeze({
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
  readonly #materialization: PersistentMaterializationPort
  readonly #checkpointNamespace: PersistentCheckpointNamespaceEvidence
  readonly #directoryLedger: DirectoryAdmissionLedger | undefined
  readonly #entries = new Map<string, MaterializedManifestEntry>()
  readonly #directoryPathByAdmission = new Map<string, readonly string[]>()
  readonly #directorySettlements = new Map<string, PersistentDirectorySettlementEvidence>()
  #closePromise: Promise<void> | undefined

  constructor(input: {
    readonly materialization: PersistentMaterializationPort
    readonly checkpointNamespace: PersistentCheckpointNamespaceEvidence
    readonly outputIdentity: OutputSessionIdentity
    readonly capabilities: OutputCapabilities
    readonly directoryLedger?: DirectoryAdmissionLedger
  }) {
    this.#materialization = input.materialization
    this.#checkpointNamespace = Object.freeze({ ...input.checkpointNamespace })
    this.identity = outputSessionIdentity(input.outputIdentity)
    this.capabilities = outputCapabilities(input.capabilities)
    this.#directoryLedger = input.directoryLedger
  }

  directories(): IncrementalDirectoryOutput {
    const ledger = this.#directoryLedger
    if (ledger === undefined) throw new TypeError('persistent output has no incremental directory authority')
    const directories: IncrementalDirectoryOutput = {
      admitDirectory: async (request, signal) => {
        const admission = await ledger.admitDirectory(
          request.source,
          signal,
          async () => {
            const materialized = await this.#materialization.ensureDirectory(request.artifactPath)
            this.#recordDirectory({
              kind: 'directory',
              artifactPath: request.artifactPath,
              directoryId: request.source.directoryId,
              generation: request.source.generation,
              ownedObjectId: materialized.ownedObjectId,
            })
          },
        )
        const artifactPath = Object.freeze([...request.artifactPath])
        const existingPath = this.#directoryPathByAdmission.get(admission.token)
        if (existingPath !== undefined && !samePath(existingPath, artifactPath)) {
          throw new TypeError('directory admission changed its materialized artifact path')
        }
        this.#directoryPathByAdmission.set(admission.token, artifactPath)
        return admission
      },
      finalizeDirectory: async (admission, signal) => {
        const settlement = validateDirectorySettlement(
          admission,
          await ledger.finalizeDirectory(admission, signal),
        )
        const artifactPath = this.#directoryPathByAdmission.get(admission.token)
        if (artifactPath === undefined) {
          throw new TypeError('directory settlement has no materialized artifact binding')
        }
        const materialized = this.#entries.get(JSON.stringify(artifactPath))
        if (materialized?.kind !== 'directory' ||
            materialized.directoryId !== admission.directoryId ||
            materialized.generation !== admission.generation) {
          throw new TypeError('directory settlement escaped its materialized ownership proof')
        }
        this.#directorySettlements.set(admission.token, Object.freeze({
          artifactPath,
          settlement,
        }))
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
      const materialized = await this.#materialization.ensureDirectory(entry.artifactPath)
      this.#recordDirectory({
        kind: 'directory',
        artifactPath: entry.artifactPath,
        directoryId: entry.directoryId,
        generation: entry.generation,
        ownedObjectId: materialized.ownedObjectId,
      })
    }
  }

  async beginFile(input: OutputFileRequest, signal: AbortSignal): Promise<BeginOutputFileResult> {
    signal.throwIfAborted()
    const request = snapshotOutputFileRequest(input)
    const mutation = request.parentAdmission === undefined || this.#directoryLedger === undefined
      ? undefined
      : this.#directoryLedger.acquireFileMutation({
          path: request.sourcePath,
          parentAdmission: request.parentAdmission,
        })
    let callbackInvoked = false
    let opened: OpenedOutputRevision | undefined
    try {
      const transaction = await this.#materialization.beginFile({
        artifactPath: request.artifactPath,
        openRevision: async () => {
          if (callbackInvoked) {
            throw new TypeError('persistent materializer invoked revision authority more than once')
          }
          callbackInvoked = true
          opened = snapshotOpenedOutputRevision(await request.openRevision(signal))
          requireMatchingOpenedRevision(request, opened)
          return Object.freeze({
            fileId: opened.fileId,
            fileRevision: opened.fileRevision,
            exactSize: opened.exactSize,
          })
        },
      })
      signal.throwIfAborted()
      if (!callbackInvoked || opened === undefined) {
        throw new TypeError('persistent materializer bypassed authenticated revision authority')
      }
      requireMatchingPersistentRevision(opened, transaction)
      const ownership: OutputFileOwnership = Object.freeze({
        ...this.identity,
        canonicalPath: request.artifactPath,
        ownedFileIdentity: transaction.ownedObjectId,
      })
      const durableRanges = verifiedRanges(ownership, opened, transaction.verifiedRanges)
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
      mutation?.release()
      throw error
    }
  }

  evidence(): PersistentMaterializationEvidence {
    const entries = [...this.#entries.values()].sort(compareMaterializedEntries)
    const directorySettlements = [...this.#directorySettlements.values()]
      .sort(compareDirectorySettlementEvidence)
    return Object.freeze({
      entries: Object.freeze(entries),
      directorySettlements: Object.freeze(directorySettlements),
    })
  }

  close(): Promise<void> {
    this.#closePromise ??= this.#materialization.close()
    return this.#closePromise
  }

  #recordDirectory(entry: Extract<MaterializedManifestEntry, { kind: 'directory' }>): void {
    this.#recordEntry(Object.freeze({ ...entry, artifactPath: Object.freeze([...entry.artifactPath]) }))
  }

  #recordFile(proof: FinalFileCheckpointProof): void {
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

class PersistentSettlementCut<Evidence extends PersistentMaterializationEvidence>
implements PersistentMaterializationSettlementCut<Evidence> {
  readonly evidence: Evidence
  readonly #close: () => Promise<void>
  #closePromise: Promise<void> | undefined

  constructor(evidence: Evidence, close: () => Promise<void>) {
    this.evidence = evidence
    this.#close = close
  }

  closeMaterialization(): Promise<void> {
    this.#closePromise ??= this.#close()
    return this.#closePromise
  }

  async validateReturnedState(state: ReceiveLifecycleState): Promise<void> {
    if (this.#closePromise === undefined) {
      throw new TypeError('persistent lifecycle settlement returned before closing materialization')
    }
    try {
      await this.#closePromise
    } catch (cause) {
      if (state.kind !== 'needs-attention') {
        throw new TypeError(
          'persistent lifecycle settlement hid materialization close uncertainty',
          { cause },
        )
      }
    }
  }
}

class PersistentOutputTransaction implements OutputFileTransaction {
  readonly #transaction: PersistentFileTransactionPort
  readonly #revision: OpenedOutputRevision
  readonly #ownership: OutputFileOwnership
  readonly #checkpointNamespace: PersistentCheckpointNamespaceEvidence
  readonly #isolated: boolean
  readonly #releaseMutation: () => void
  readonly #recordProof: (proof: FinalFileCheckpointProof) => void
  #settled = false

  constructor(input: {
    readonly transaction: PersistentFileTransactionPort
    readonly revision: OpenedOutputRevision
    readonly ownership: OutputFileOwnership
    readonly checkpointNamespace: PersistentCheckpointNamespaceEvidence
    readonly isolated: boolean
    readonly releaseMutation: () => void
    readonly recordProof: (proof: FinalFileCheckpointProof) => void
  }) {
    this.#transaction = input.transaction
    this.#revision = input.revision
    this.#ownership = input.ownership
    this.#checkpointNamespace = input.checkpointNamespace
    this.#isolated = input.isolated
    this.#releaseMutation = input.releaseMutation
    this.#recordProof = input.recordProof
  }

  writeRange(offset: bigint, data: Uint8Array, signal: AbortSignal): Promise<void> {
    return this.#transaction.writeRange(offset, data, signal)
  }

  async checkpoint(signal: AbortSignal): Promise<VerifiedDurableRanges> {
    return verifiedRanges(
      this.#ownership,
      this.#revision,
      await this.#transaction.checkpoint(signal),
    )
  }

  async commit(signal: AbortSignal): Promise<void> {
    if (this.#settled) return
    const proof = await this.#transaction.commit(signal)
    requireMatchingFinalProof(
      this.#revision,
      this.#ownership,
      this.#checkpointNamespace,
      proof,
    )
    await this.#transaction.close()
    this.#recordProof(proof)
    this.#settle()
  }

  async retire(): Promise<FileRetirementDisposition> {
    if (!this.#settled) {
      await this.#transaction.close()
      this.#settle()
    }
    return this.#isolated ? 'FileIsolated' : 'JobOutputCompromised'
  }

  async pause(): Promise<void> {
    if (this.#settled) return
    await this.#transaction.checkpoint()
    await this.#transaction.close()
    this.#settle()
  }

  #settle(): void {
    if (this.#settled) return
    this.#settled = true
    this.#releaseMutation()
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

function verifiedRanges(
  ownership: OutputFileOwnership,
  revision: OpenedOutputRevision,
  ranges: readonly Readonly<{ start: bigint; end: bigint }>[],
): VerifiedDurableRanges {
  return new VerifiedDurableRanges(ownership, revision, revision.exactSize, ranges)
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

function requireMatchingFinalProof(
  revision: OpenedOutputRevision,
  ownership: OutputFileOwnership,
  namespace: PersistentCheckpointNamespaceEvidence,
  proof: FinalFileCheckpointProof,
): void {
  if (proof.operationId !== namespace.operationId ||
      proof.receiveIntentDigest !== namespace.receiveIntentDigest ||
      proof.materializationBindingDigest !== namespace.materializationBindingDigest ||
      proof.fileId !== revision.fileId || proof.fileRevision !== revision.fileRevision ||
      proof.exactSize !== revision.exactSize || proof.ownedObjectId !== ownership.ownedFileIdentity ||
      !samePath(proof.canonicalPath, ownership.canonicalPath) || proof.complete !== true) {
    throw new TypeError('final checkpoint proof escaped its output transaction')
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
