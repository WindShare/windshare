import {
  DirectorySettlementKind,
  type DirectoryAdmissionScope,
} from '../../transfer/directory-admission'
import type { MaterializationSummary } from '../../transfer/output-session'
import type {
  PersistentDirectorySettlementEvidence,
  PersistentMaterializationEvidence,
} from '../../transfer/settlement/persistent-execution'
import { fileCheckpointIsComplete, type FileCheckpointV2 } from '../persistence/checkpoint'
import type { FinalFileCheckpointProof } from '../persistence/journal'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type { MaterializedManifestEntry } from '../workspace/manifest'
import type { FSAFinalSettlementObservation } from './session'
import {
  fsaCheckpointSetDigest,
  fsaCheckpointReferenceSetDigest,
  materializationSummary,
  sameFileEvidence,
  sameFinalProof,
  sameSummary,
  snapshotDirectorySettlements,
  snapshotEntries,
  type DirectTreeIntent,
  type ObservedSettlementEvidence,
  type SettlementReceiptEvidence,
} from './settlement-proof'
import { validateFSASettlementRootEvidence } from './settlement-root-evidence'

type MaterializedFileEntry = Extract<MaterializedManifestEntry, { kind: 'file' }>
type MaterializedDirectoryEntry = Extract<MaterializedManifestEntry, { kind: 'directory' }>

export interface ObserveFSASettlementEvidenceOptions {
  readonly intent: DirectTreeIntent
  readonly directoryScope: DirectoryAdmissionScope
  readonly observation: FSAFinalSettlementObservation
  readonly evidence: PersistentMaterializationEvidence
  readonly summary: MaterializationSummary
  readonly requireComplete: boolean
}

/**
 * Builds the deterministic pending-outcome receipt input from an already-quiescent
 * in-memory manifest. Native ownership reads remain behind the terminal footer.
 */
export async function snapshotQuiescentFSASettlementEvidence(
  options: Omit<ObserveFSASettlementEvidenceOptions, 'observation'>,
): Promise<SettlementReceiptEvidence> {
  validateFSASettlementRootEvidence({
    directoryScope: options.directoryScope,
    directories: directoryEntriesOf(options.evidence.entries),
    directorySettlements: options.evidence.directorySettlements,
    requireComplete: options.requireComplete,
  })
  const entries = snapshotEntries(options.evidence.entries)
  const fileEntries = entries.filter(
    (entry): entry is MaterializedFileEntry => entry.kind === 'file',
  )
  const directoryEntries = entries.filter(
    (entry): entry is MaterializedDirectoryEntry => entry.kind === 'directory',
  )
  const directorySettlements = snapshotDirectorySettlements(
    options.evidence.directorySettlements,
    directoryEntries,
    options.directoryScope,
  )
  if (options.requireComplete && (directorySettlements.length !== directoryEntries.length ||
      directorySettlements.some(value =>
        value.settlement.kind !== DirectorySettlementKind.Finalized))) {
    throw new TypeError('published FSA settlement lacks finalized directory evidence')
  }
  const measured = materializationSummary(entries)
  if (!sameSummary(measured, options.summary)) {
    throw new TypeError('FSA settlement summary differs from owned evidence')
  }
  const checkpointReferences = fileEntries
    .map(entry => entry.checkpoint)
    .sort(compareCheckpointReferences)
  return Object.freeze({
    entries,
    directorySettlements,
    checkpointSetDigest: await fsaCheckpointReferenceSetDigest(options.intent, checkpointReferences),
    completedFileCount: BigInt(fileEntries.length),
    completedBytes: fileEntries.reduce((total, entry) => total + entry.exactSize, 0n),
  })
}

function compareCheckpointReferences(
  left: Readonly<{ recordId: string }>,
  right: Readonly<{ recordId: string }>,
): number {
  if (left.recordId < right.recordId) return -1
  if (left.recordId > right.recordId) return 1
  return 0
}

/**
 * Verifies a read-only namespace cut without receiving any lifecycle mutation port.
 * Keeping proof observation separate prevents verified evidence from becoming an
 * accidental second settlement authority.
 */
export async function observeFSASettlementEvidence(
  options: ObserveFSASettlementEvidenceOptions,
): Promise<ObservedSettlementEvidence> {
  await options.observation.verifyOperationBinding()
  const candidates = await options.observation.candidateCheckpoints()
  if (candidates.length !== 0) {
    throw new TargetOwnershipUnknownError('settlement', options.intent.operationId)
  }
  const checkpoints = await options.observation.committedCheckpoints()
  validateFSASettlementRootEvidence({
    directoryScope: options.directoryScope,
    directories: directoryEntriesOf(options.evidence.entries),
    directorySettlements: options.evidence.directorySettlements,
    requireComplete: options.requireComplete,
  })
  const entries = snapshotEntries(options.evidence.entries)
  const fileEntries = entries.filter(
    (entry): entry is MaterializedFileEntry => entry.kind === 'file',
  )
  const directoryEntries = entries.filter(
    (entry): entry is MaterializedDirectoryEntry => entry.kind === 'directory',
  )
  await verifyCheckpointEvidence(
    options.intent,
    options.observation,
    checkpoints,
    fileEntries,
    options.requireComplete,
  )
  const directorySettlements = await verifyDirectoryEvidence(
    options.observation,
    options.evidence.directorySettlements,
    directoryEntries,
    options.directoryScope,
    options.requireComplete,
  )
  const measured = materializationSummary(entries)
  if (!sameSummary(measured, options.summary)) {
    throw new TypeError('FSA settlement summary differs from owned evidence')
  }
  const checkpointSetDigest = await fsaCheckpointSetDigest(options.intent, checkpoints)
  return Object.freeze({
    entries,
    directorySettlements,
    checkpoints,
    checkpointSetDigest,
    completedFileCount: BigInt(fileEntries.length),
    completedBytes: fileEntries.reduce((total, entry) => total + entry.exactSize, 0n),
  })
}

async function verifyCheckpointEvidence(
  intent: DirectTreeIntent,
  observation: FSAFinalSettlementObservation,
  checkpoints: readonly FileCheckpointV2[],
  fileEntries: readonly MaterializedFileEntry[],
  requireComplete: boolean,
): Promise<void> {
  const checkpointById = new Map(checkpoints.map(record => [record.recordId, record]))
  const fileByCheckpoint = new Map(fileEntries.map(entry => [entry.checkpoint.recordId, entry]))
  if (fileByCheckpoint.size !== fileEntries.length) {
    throw new TypeError('FSA settlement repeats a final checkpoint')
  }

  for (const checkpoint of checkpoints) {
    await observation.verifyCheckpointFile(checkpoint)
    if (fileCheckpointIsComplete(checkpoint) && !fileByCheckpoint.has(checkpoint.recordId)) {
      throw new TargetOwnershipUnknownError('settlement', intent.operationId)
    }
    if (requireComplete && !fileCheckpointIsComplete(checkpoint)) {
      throw new TypeError('published FSA settlement contains an incomplete checkpoint')
    }
  }
  for (const entry of fileEntries) {
    const checkpoint = checkpointById.get(entry.checkpoint.recordId)
    if (checkpoint === undefined || !sameFileEvidence(entry, checkpoint)) {
      throw new TargetOwnershipUnknownError('settlement', intent.operationId)
    }
    let proof: FinalFileCheckpointProof
    try {
      proof = await observation.finalCheckpointProof(
        entry.checkpoint.recordId,
        entry.checkpoint.checkpointGeneration,
      )
    } catch (cause) {
      throw new TargetOwnershipUnknownError('settlement', intent.operationId, { cause })
    }
    if (!sameFinalProof(proof, entry, intent)) {
      throw new TargetOwnershipUnknownError('settlement', intent.operationId)
    }
  }
}

async function verifyDirectoryEvidence(
  observation: FSAFinalSettlementObservation,
  supplied: readonly PersistentDirectorySettlementEvidence[],
  directoryEntries: readonly MaterializedDirectoryEntry[],
  directoryScope: DirectoryAdmissionScope,
  requireComplete: boolean,
): Promise<readonly PersistentDirectorySettlementEvidence[]> {
  for (const entry of directoryEntries) {
    await observation.verifyDirectory(entry.artifactPath, entry.ownedObjectId)
  }
  const directorySettlements = snapshotDirectorySettlements(
    supplied,
    directoryEntries,
    directoryScope,
  )
  if (requireComplete && (directorySettlements.length !== directoryEntries.length ||
      directorySettlements.some(value =>
        value.settlement.kind !== DirectorySettlementKind.Finalized))) {
    throw new TypeError('published FSA settlement lacks finalized directory evidence')
  }
  return directorySettlements
}

function directoryEntriesOf(
  entries: readonly MaterializedManifestEntry[],
): readonly MaterializedDirectoryEntry[] {
  return entries.filter(
    (entry): entry is MaterializedDirectoryEntry => entry.kind === 'directory',
  )
}
