import type {
  AuthenticatedGenerationReference,
  MaterializedManifestEntry,
} from '../../output/workspace/manifest'
import type { ReceiveLifecycleState } from '../../output/workspace/state'
import {
  DirectorySettlementKind,
  type DirectorySettlement,
} from '../directory-admission'
import type {
  OriginalFileArtifact,
  ReceiveIntent,
  WorkspaceThenPublishPlan,
  ZipArchiveArtifact,
} from '../intent'
import type {
  ExactPreparationEvidence,
  ExactSingleFileEvidence,
  PlanPauseRequest,
  PlanStopRequest,
  PlanSettlementRequest,
} from '../output-session'
import type {
  CompletedTransferWorkerSettlement,
  SuccessfulTransferWorkerSettlement,
} from '../outcome'

type WorkspaceOriginalIntent = ReceiveIntent & Readonly<{
  plan: WorkspaceThenPublishPlan
  artifact: OriginalFileArtifact
}>
type WorkspaceZipIntent = ReceiveIntent & Readonly<{
  plan: WorkspaceThenPublishPlan
  artifact: ZipArchiveArtifact
}>

export interface PersistentMaterializationEvidence {
  readonly entries: readonly MaterializedManifestEntry[]
  readonly directorySettlements: readonly PersistentDirectorySettlementEvidence[]
}

export interface PersistentDirectorySettlementEvidence {
  readonly artifactPath: readonly string[]
  readonly settlement: DirectorySettlement
}

export interface WorkspaceMaterializationEvidence extends PersistentMaterializationEvidence {
  readonly generations: readonly AuthenticatedGenerationReference[]
}

export interface PersistentMaterializationSettlementCut<
  Evidence extends PersistentMaterializationEvidence,
> {
  readonly evidence: Evidence
  /** Captures the immutable in-memory manifest only after the backend has quiesced admissions. */
  snapshotQuiescentEvidence(): Evidence
  /** Qualifies the captured snapshot as the settlement evidence after final materialization work. */
  sealEvidence(): Evidence
  /** The lifecycle owner chooses the final ownership-check/close ordering and must await this cut. */
  closeMaterialization(): Promise<void>
}

export interface PersistentDirectTreeSettlementAuthority {
  pause(
    request: PlanPauseRequest,
    cut: PersistentMaterializationSettlementCut<PersistentMaterializationEvidence>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
  settle(
    request: PlanSettlementRequest<CompletedTransferWorkerSettlement>,
    cut: PersistentMaterializationSettlementCut<PersistentMaterializationEvidence>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
  stop?(
    request: PlanStopRequest,
    cut: PersistentMaterializationSettlementCut<PersistentMaterializationEvidence>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
}

export interface PersistentWorkspaceSettlementAuthority {
  pause(
    request: PlanPauseRequest,
    cut: PersistentMaterializationSettlementCut<WorkspaceMaterializationEvidence>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
  settle(
    request: PlanSettlementRequest<SuccessfulTransferWorkerSettlement>,
    cut: PersistentMaterializationSettlementCut<WorkspaceMaterializationEvidence>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
}

export function requireCompleteWorkspaceMaterialization(
  intent: WorkspaceOriginalIntent | WorkspaceZipIntent,
  admission:
    | Readonly<{ kind: 'single-file'; evidence: ExactSingleFileEvidence }>
    | Readonly<{ kind: 'prepared'; evidence: ExactPreparationEvidence }>,
  evidence: WorkspaceMaterializationEvidence,
): void {
  if (admission.kind === 'single-file') {
    const entry = evidence.entries[0]
    if (evidence.entries.length !== 1 || entry?.kind !== 'file' ||
        entry.fileId !== admission.evidence.fileId ||
        entry.exactSize !== admission.evidence.catalogSize ||
        !samePath(entry.artifactPath, [intent.artifact.suggestedName])) {
      throw new TypeError('Workspace OriginalFile lacks its exact admitted checkpoint proof')
    }
    return
  }
  if (evidence.entries.length !== admission.evidence.entries.length) {
    throw new TypeError('prepared Workspace materialization is incomplete')
  }
  const byPath = new Map(evidence.entries.map(entry => [JSON.stringify(entry.artifactPath), entry]))
  for (const expected of admission.evidence.entries) {
    const materialized = byPath.get(JSON.stringify(expected.artifactPath))
    if (expected.kind === 'directory') {
      if (materialized?.kind !== 'directory' ||
          materialized.directoryId !== expected.directoryId ||
          materialized.generation !== expected.generation) {
        throw new TypeError('prepared Workspace directory lacks materialized ownership proof')
      }
      continue
    }
    if (materialized?.kind !== 'file' || materialized.fileId !== expected.fileId ||
        materialized.exactSize !== expected.exactSize) {
      throw new TypeError('prepared Workspace file lacks final checkpoint proof')
    }
  }
}

export function requireMatchingMaterializationSummary(
  request: PlanSettlementRequest<CompletedTransferWorkerSettlement> | PlanStopRequest,
  evidence: PersistentMaterializationEvidence,
): void {
  const materializedEntries = evidence.entries.filter(entry =>
    entry.kind === 'file' || entry.artifactPath.length > 0)
  const fileCount = BigInt(materializedEntries.filter(entry => entry.kind === 'file').length)
  const directoryCount = BigInt(materializedEntries.length) - fileCount
  const rawBytes = evidence.entries.reduce(
    (total, entry) => total + (entry.kind === 'file' ? entry.exactSize : 0n),
    0n,
  )
  if (request.materialization.entryCount !== BigInt(materializedEntries.length) ||
      request.materialization.fileCount !== fileCount ||
      request.materialization.directoryCount !== directoryCount ||
      request.materialization.rawBytes !== rawBytes) {
    throw new TypeError('worker summary cannot substitute for materialized checkpoint evidence')
  }
}

export function requireCompleteDirectorySettlement(
  evidence: PersistentMaterializationEvidence,
): void {
  const materializedDirectories = evidence.entries.filter(entry => entry.kind === 'directory')
  if (evidence.directorySettlements.length !== materializedDirectories.length ||
      evidence.directorySettlements.some(({ settlement }) =>
        settlement.kind !== DirectorySettlementKind.Finalized)) {
    throw new TypeError('successful DirectTree settlement requires every directory proof')
  }
}

export function compareMaterializedEntries(
  left: MaterializedManifestEntry,
  right: MaterializedManifestEntry,
): number {
  const leftPath = left.artifactPath.join('/')
  const rightPath = right.artifactPath.join('/')
  if (leftPath < rightPath) return -1
  if (leftPath > rightPath) return 1
  if (left.kind === right.kind) return 0
  return left.kind === 'directory' ? -1 : 1
}

export function compareDirectorySettlementEvidence(
  left: PersistentDirectorySettlementEvidence,
  right: PersistentDirectorySettlementEvidence,
): number {
  const leftPath = left.artifactPath.join('/')
  const rightPath = right.artifactPath.join('/')
  if (leftPath < rightPath) return -1
  if (leftPath > rightPath) return 1
  return left.settlement.admission.token.localeCompare(right.settlement.admission.token)
}

export function sameMaterializedEntry(
  left: MaterializedManifestEntry,
  right: MaterializedManifestEntry,
): boolean {
  if (left.kind !== right.kind || !samePath(left.artifactPath, right.artifactPath) ||
      left.ownedObjectId !== right.ownedObjectId) return false
  if (left.kind === 'directory' && right.kind === 'directory') {
    return left.directoryId === right.directoryId && left.generation === right.generation
  }
  if (left.kind === 'file' && right.kind === 'file') {
    return left.fileId === right.fileId && left.fileRevision === right.fileRevision &&
      left.exactSize === right.exactSize && left.checkpoint.recordId === right.checkpoint.recordId &&
      left.checkpoint.recordDigest === right.checkpoint.recordDigest &&
      left.checkpoint.checkpointGeneration === right.checkpoint.checkpointGeneration
  }
  return false
}

export function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((segment, index) => segment === right[index])
}
