import { sourceMetadataFields } from '../../content/revision-comparison'
import { observePerformance, type PerformanceSummaryObservations } from '../../diagnostics/performance/performance-summary'
import {
  createMaterializationDirectoryAdmittedEntry,
  createMaterializationDirectoryFinalizedEntry,
  materializationFinalProofId,
  validateFinalOutputAgainstCheckpoint,
} from '../materialization-ledger/journal'
import type {
  MaterializationDirectoryAdmittedEntryV1,
  MaterializationDirectoryFinalization,
  MaterializationDirectoryFinalizedEntryV1,
  MaterializationLedgerBindingV1,
} from '../materialization-ledger/model'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  FILE_CHECKPOINT_PHASE_PAUSED,
  deriveCheckpointLineageID,
  fileCheckpointIsComplete,
  newFileCheckpointV2,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import type { CheckpointLineageDecision, FileCheckpointJournal } from '../persistence/journal'
import { canShortCircuitCompletedFile } from '../persistence/source-admission'
import type { PerformanceFilePipelineObservation } from '../../diagnostics/performance/performance-runtime-observations'
import { snapshotMaterializationRootRelativePath, type MaterializationRootRelativePath } from '../../transfer/job/coordinate/direct-tree'
import type {
  OpenedFileRevision,
  PersistentDirectoryLedgerMaterialization,
  PersistentDirectoryLedgerRequest,
  PersistentOutputTree,
  PersistentTreeFile,
  SemanticPersistentOutputJournal,
} from './contracts'
import { TargetOwnershipUnknownError } from './errors'
import { runPersistentOutputStage, type PersistentOutputStageScope } from './stage-diagnostics'

export class PersistentDirectoryLedgerCoordinator {
  readonly #tree: PersistentOutputTree
  readonly #journal: SemanticPersistentOutputJournal
  readonly #binding: Promise<MaterializationLedgerBindingV1>
  readonly #performance: PerformanceSummaryObservations | undefined

  constructor(
    tree: PersistentOutputTree,
    journal: SemanticPersistentOutputJournal,
    binding: Promise<MaterializationLedgerBindingV1>,
    performance?: PerformanceSummaryObservations,
  ) {
    this.#tree = tree
    this.#journal = journal
    this.#binding = binding
    this.#performance = performance
  }

  async materialize(
    request: PersistentDirectoryLedgerRequest,
  ): Promise<PersistentDirectoryLedgerMaterialization> {
    const relativePath = snapshotMaterializationRootRelativePath(request.relativePath)
    const materialized = await this.#tree.ensureDirectory(relativePath)
    const binding = await this.#binding
    const ledgerAdmission = await createMaterializationDirectoryAdmittedEntry(binding, {
      relativePath,
      directoryId: request.directoryId,
      generation: request.generation,
      ownedObjectId: materialized.ownedObjectId,
      ...(request.parent === undefined ? {} : { parent: request.parent }),
      ...(request.modifiedTime === undefined ? {} : { modifiedTime: request.modifiedTime }),
    })
    const classification = await this.#journal.appendDirectoryAdmission(binding, ledgerAdmission)
    if (classification === 'insert') {
      observePerformance(this.#performance, summary => summary.observeLedger({ transition: 'entry' }))
    }
    return Object.freeze({ ...materialized, ledgerAdmission })
  }

  async finalize(
    admission: MaterializationDirectoryAdmittedEntryV1,
    outcome: MaterializationDirectoryFinalization,
  ): Promise<MaterializationDirectoryFinalizedEntryV1> {
    const binding = await this.#binding
    const finalized = await createMaterializationDirectoryFinalizedEntry(binding, admission, outcome)
    const classification = await this.#journal.appendDirectoryFinalization(binding, finalized)
    if (classification === 'insert') {
      observePerformance(this.#performance, summary => summary.observeLedger({ transition: 'entry' }))
    }
    return finalized
  }
}

export async function openPersistentSelectedFile(input: Readonly<{
  tree: PersistentOutputTree
  semantic?: SemanticPersistentOutputJournal
  revision: OpenedFileRevision
  path: MaterializationRootRelativePath
  checkpoint: FileCheckpointV2
  performancePipeline?: PerformanceFilePipelineObservation
  stageScope?: PersistentOutputStageScope
  promote(checkpoint: FileCheckpointV2, stageScope?: PersistentOutputStageScope): Promise<FileCheckpointV2>
}>): Promise<Readonly<{ handle: PersistentTreeFile; checkpoint: FileCheckpointV2 }>> {
  let handle = await input.tree.openFile(
    input.path,
    input.checkpoint.ownedObjectId,
    input.stageScope,
  )
  let committed = input.checkpoint
  let createdWithAtomicClaim = false
  if (handle === undefined) {
    if (!isPristinePreObjectCandidate(input.checkpoint)) {
      throw new TargetOwnershipUnknownError('checkpoint', input.checkpoint.operationId)
    }
    const atomicCommitted = committedInitialCheckpoint(input.checkpoint)
    input.performancePipeline?.transition('namespace_creation')
    try {
      handle = await input.tree.createFileAfterRevisionOpen(
        input.path,
        input.revision,
        input.checkpoint.ownedObjectId,
        input.stageScope,
        input.semantic === undefined
          ? undefined
          : persistedHandle => runPersistentOutputStage(
              input.stageScope,
              'indexeddb.checkpoint.created-file-commit',
              () => input.semantic!.commitCreatedFile({
                candidate: input.checkpoint,
                committed: atomicCommitted,
                handle: persistedHandle,
              }),
            ),
      )
    } finally {
      input.performancePipeline?.transition('initial_lineage')
    }
    if (input.semantic !== undefined) {
      committed = atomicCommitted
      createdWithAtomicClaim = true
    }
  }
  try {
    if (!createdWithAtomicClaim) await verifySelectedFile(handle, input.checkpoint, input.revision)
    if (committed.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE) {
      committed = await input.promote(committed, input.stageScope)
    }
    return Object.freeze({ handle, checkpoint: committed })
  } catch (error) {
    await handle.close()
    throw error
  }
}

export function isPristinePreObjectCandidate(checkpoint: FileCheckpointV2): boolean {
  return checkpoint.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE &&
    checkpoint.phase === FILE_CHECKPOINT_PHASE_ACTIVE &&
    checkpoint.stateGeneration === 1n &&
    checkpoint.checkpointGeneration === 0n &&
    checkpoint.verifiedRanges.length === 0
}

function committedInitialCheckpoint(candidate: FileCheckpointV2): FileCheckpointV2 {
  return newFileCheckpointV2({ ...candidate, commitState: FILE_CHECKPOINT_COMMIT_VERIFIED })
}

async function verifySelectedFile(
  handle: PersistentTreeFile,
  checkpoint: FileCheckpointV2,
  revision: OpenedFileRevision,
): Promise<void> {
  if (handle.ownedObjectId !== checkpoint.ownedObjectId) {
    throw new TargetOwnershipUnknownError('checkpoint', checkpoint.operationId)
  }
  const actualSize = await handle.size()
  const durableEnd = checkpoint.verifiedRanges.at(-1)?.end ?? 0n
  if (actualSize < durableEnd || actualSize > revision.exactSize) {
    throw new TargetOwnershipUnknownError('checkpoint', checkpoint.operationId)
  }
  await handle.verify('checkpoint')
}

/** Short-circuits completed files so resume preserves immutable local delivery history without reopening source handles. */
export async function resolveCompletedPersistentRevision(input: Readonly<{
  fileId: string
  expectedSize: bigint | undefined
  path: MaterializationRootRelativePath
  checkpoints: Pick<FileCheckpointJournal, 'binding' | 'readCommitted' | 'lookupLineage'>
  semantic?: SemanticPersistentOutputJournal
  ledgerBinding?: Promise<MaterializationLedgerBindingV1>
}>): Promise<Readonly<{ revision: OpenedFileRevision; decision: CheckpointLineageDecision }> | undefined> {
  const { fileId, expectedSize, path, checkpoints, semantic, ledgerBinding } = input
  const lineageId = deriveCheckpointLineageID({
    ...checkpoints.binding,
    fileId,
    canonicalPath: path,
  })
  const snapshot = await checkpoints.readCommitted(lineageId)
  if (snapshot === undefined || !completedCheckpointMatchesRequest(snapshot, fileId, expectedSize, path)) return undefined
  // Local recovery can only select an existing lineage. A concurrent readmission
  // may have advanced its revision since the point read.
  const selection = await checkpoints.lookupLineage({
    lineageId, fileId, canonicalPath: path, fileRevision: snapshot.fileRevision,
    exactSize: snapshot.exactSize, ...sourceMetadataFields(snapshot),
  })
  if (selection.kind !== 'exact' && selection.kind !== 'metadata-match') return undefined
  const committed = selection.record
  if (committed.ownedObjectId !== snapshot.ownedObjectId ||
      !completedCheckpointMatchesRequest(committed, fileId, expectedSize, path)) return undefined
  if (semantic !== undefined && ledgerBinding !== undefined) {
    const delivered = await hasMatchingFinalProof(semantic, await ledgerBinding, committed)
    if (!delivered && committed.exactSize === 0n) return undefined
  }
  const revision = Object.freeze({ fileId, fileRevision: committed.fileRevision,
    ...sourceMetadataFields(committed), exactSize: committed.exactSize })
  return { revision, decision: { kind: 'exact', lineageId, record: committed } }
}

export async function hasMatchingFinalProof(
  journal: SemanticPersistentOutputJournal, binding: MaterializationLedgerBindingV1, checkpoint: FileCheckpointV2,
): Promise<boolean> {
  const proof = await journal.readMaterializationFinalProof(binding, materializationFinalProofId(binding, checkpoint.recordId))
  if (proof === undefined) return false
  if (proof.checkpoint.recordId !== checkpoint.recordId || proof.checkpoint.recordDigest !== checkpoint.checksum ||
      proof.checkpoint.checkpointGeneration !== checkpoint.checkpointGeneration) {
    throw new TargetOwnershipUnknownError('commit', checkpoint.operationId)
  }
  validateFinalOutputAgainstCheckpoint(proof.finalOutput, checkpoint)
  return true
}

function completedCheckpointMatchesRequest(
  committed: FileCheckpointV2,
  fileId: string,
  expectedSize: bigint | undefined,
  path: MaterializationRootRelativePath,
): boolean {
  return canShortCircuitCompletedFile({
    requestFileId: fileId,
    requestExpectedSize: expectedSize,
    retainedFileId: committed.fileId,
    retainedExactSize: committed.exactSize,
    locallyComplete:
      committed.commitState === FILE_CHECKPOINT_COMMIT_VERIFIED &&
      committed.checkpointGeneration > 0n &&
      (committed.phase === FILE_CHECKPOINT_PHASE_ACTIVE || committed.phase === FILE_CHECKPOINT_PHASE_PAUSED) &&
      fileCheckpointIsComplete(committed),
    coordinateMatches:
      committed.canonicalPath.length === path.length &&
      committed.canonicalPath.every((segment, index) => segment === path[index]),
  })
}
