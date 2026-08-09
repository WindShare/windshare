import {
  fileCheckpointDigest,
  fileCheckpointIsComplete,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import type { FinalFileCheckpointProof } from '../persistence/journal'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalI64,
  canonicalIdentity,
  canonicalPath,
  canonicalRecord,
  canonicalText,
  canonicalU32,
  canonicalU64,
  canonicalU8,
  concatCanonicalBytes,
  type CanonicalBytes,
} from '../workspace/canonical'
import type { MaterializedManifestEntry } from '../workspace/manifest'
import {
  RECEIVE_RECORD_CLEANUP,
  RECEIVE_RECORD_RECEIPT,
  createPersistedReceiveRecord,
  type PersistedReceiveRecord,
} from '../workspace/records'
import {
  DirectorySettlementKind,
  snapshotDirectoryAdmission,
  snapshotMaterializationPath,
  type DirectoryAdmission,
  type DirectoryAdmissionScope,
  type DirectorySettlement,
} from '../../transfer/directory-admission'
import {
  validateReceiveIntent,
  type DirectoryTreeArtifact,
  type DirectTreePlan,
  type ReceiveIntent,
} from '../../transfer/intent'
import type {
  MaterializationSummary,
  PlanPauseRequest,
  PlanSettlementRequest,
} from '../../transfer/output-session'
import type {
  CompletedTransferWorkerSettlement,
  TransferFailure,
} from '../../transfer/outcome'
import type { PersistentDirectorySettlementEvidence } from '../../transfer/settlement/persistent-execution'

const FSA_SETTLEMENT_RECEIPT = 12
const FSA_UNOPENED_CLEANUP_RECEIPT = 13

export type DirectTreeIntent = ReceiveIntent & Readonly<{
  plan: DirectTreePlan
  artifact: DirectoryTreeArtifact
}>

export interface ObservedSettlementEvidence {
  readonly entries: readonly MaterializedManifestEntry[]
  readonly directorySettlements: readonly PersistentDirectorySettlementEvidence[]
  readonly checkpoints: readonly FileCheckpointV2[]
  readonly checkpointSetDigest: string
  readonly completedFileCount: bigint
  readonly completedBytes: bigint
}

export async function requireDirectTreeIntent(input: ReceiveIntent): Promise<DirectTreeIntent> {
  const intent = await validateReceiveIntent(input)
  if (intent.plan.kind !== 'direct-tree' || intent.artifact.kind !== 'directory-tree' ||
      intent.plan.reservation.authorityKind !== 'fsa-container' ||
      intent.plan.reservation.guarantees.profile !== 'fsa-tree' ||
      intent.plan.reservation.guarantees.delivery !== 'managed-target' ||
      intent.plan.reservation.guarantees.replacement !== 'coordinated-no-replace' ||
      intent.plan.reservation.guarantees.visibility !== 'prefix-visible') {
    throw new TypeError('FSA settlement requires ManagedTarget CoordinatedNoReplace PrefixVisible')
  }
  return intent as DirectTreeIntent
}

export function snapshotEntries(
  entries: readonly MaterializedManifestEntry[],
): readonly MaterializedManifestEntry[] {
  const snapshots = entries.map(entry => {
    canonicalSettlementEntry(entry)
    return Object.freeze({
      ...entry,
      artifactPath: snapshotMaterializationPath(entry.artifactPath),
      ...(entry.kind === 'file'
        ? { checkpoint: Object.freeze({ ...entry.checkpoint }) }
        : {}),
    }) as MaterializedManifestEntry
  }).sort(compareEntries)
  const keys = snapshots.map(entry => JSON.stringify(entry.artifactPath))
  if (new Set(keys).size !== keys.length) {
    throw new TypeError('FSA settlement repeats an artifact path')
  }
  return Object.freeze(snapshots)
}

export function snapshotDirectorySettlements(
  values: readonly PersistentDirectorySettlementEvidence[],
  directories: readonly Extract<MaterializedManifestEntry, { kind: 'directory' }>[],
  scope: DirectoryAdmissionScope,
): readonly PersistentDirectorySettlementEvidence[] {
  const byPath = new Map(directories.map(entry => [JSON.stringify(entry.artifactPath), entry]))
  const snapshots = values.map(value => {
    const artifactPath = snapshotMaterializationPath(value.artifactPath)
    const settlement = snapshotSettlement(value.settlement)
    const entry = byPath.get(JSON.stringify(artifactPath))
    if (entry === undefined || settlement.admission.receiveIntentDigest !== scope.receiveIntentDigest ||
        settlement.admission.layoutVersion !== scope.layoutVersion ||
        settlement.admission.layout !== scope.layout ||
        settlement.admission.directoryId !== entry.directoryId ||
        settlement.admission.generation !== entry.generation) {
      throw new TypeError('FSA directory settlement escaped its owned directory evidence')
    }
    return Object.freeze({ artifactPath, settlement })
  }).sort((left, right) => comparePath(left.artifactPath, right.artifactPath))
  const paths = snapshots.map(value => JSON.stringify(value.artifactPath))
  if (new Set(paths).size !== paths.length) {
    throw new TypeError('FSA settlement repeats a directory receipt')
  }
  return Object.freeze(snapshots)
}

function snapshotSettlement(input: DirectorySettlement): DirectorySettlement {
  const admission = snapshotDirectoryAdmission(input.admission)
  switch (input.kind) {
    case DirectorySettlementKind.Finalized:
      return Object.freeze({ kind: input.kind, admission })
    case DirectorySettlementKind.IsolatedFailure:
      return Object.freeze({ kind: input.kind, admission, fault: input.fault })
  }
}

export function sameFileEvidence(
  entry: Extract<MaterializedManifestEntry, { kind: 'file' }>,
  checkpoint: FileCheckpointV2,
): boolean {
  return fileCheckpointIsComplete(checkpoint) &&
    checkpoint.operationId.length !== 0 &&
    checkpoint.recordId === entry.checkpoint.recordId &&
    fileCheckpointDigest(checkpoint) === entry.checkpoint.recordDigest &&
    checkpoint.checkpointGeneration === entry.checkpoint.checkpointGeneration &&
    checkpoint.fileId === entry.fileId && checkpoint.fileRevision === entry.fileRevision &&
    checkpoint.exactSize === entry.exactSize && checkpoint.ownedObjectId === entry.ownedObjectId &&
    samePath(checkpoint.canonicalPath, entry.artifactPath)
}

export function sameFinalProof(
  proof: FinalFileCheckpointProof,
  entry: Extract<MaterializedManifestEntry, { kind: 'file' }>,
  intent: DirectTreeIntent,
): boolean {
  return proof.operationId === intent.operationId && proof.receiveIntentDigest === intent.digest &&
    proof.materializationBindingDigest === intent.plan.reservation.digest && proof.complete === true &&
    proof.recordId === entry.checkpoint.recordId &&
    proof.recordDigest === entry.checkpoint.recordDigest &&
    proof.checkpointGeneration === entry.checkpoint.checkpointGeneration &&
    proof.fileId === entry.fileId && proof.fileRevision === entry.fileRevision &&
    proof.exactSize === entry.exactSize && proof.ownedObjectId === entry.ownedObjectId &&
    samePath(proof.canonicalPath, entry.artifactPath)
}

export function materializationSummary(
  entries: readonly MaterializedManifestEntry[],
): MaterializationSummary {
  const visible = entries.filter(entry => entry.kind === 'file' || entry.artifactPath.length !== 0)
  const files = visible.filter(entry => entry.kind === 'file')
  return Object.freeze({
    entryCount: BigInt(visible.length),
    fileCount: BigInt(files.length),
    directoryCount: BigInt(visible.length - files.length),
    rawBytes: files.reduce((total, entry) => total + entry.exactSize, 0n),
  })
}

export function sameSummary(
  left: MaterializationSummary,
  right: MaterializationSummary,
): boolean {
  return left.entryCount === right.entryCount && left.fileCount === right.fileCount &&
    left.directoryCount === right.directoryCount && left.rawBytes === right.rawBytes
}

export async function fsaCheckpointSetDigest(
  intent: DirectTreeIntent,
  checkpoints: readonly FileCheckpointV2[],
): Promise<string> {
  return canonicalDigest(canonicalRecord('windshare/fsa-checkpoint-set/v1', 1, [
    identityFrame(intent.operationId, 16, 'operation ID'),
    identityFrame(intent.digest, 32, 'receive intent digest'),
    identityFrame(intent.plan.reservation.digest, 32, 'reservation digest'),
    canonicalFrame(canonicalU64(BigInt(checkpoints.length))),
    ...checkpoints.map(checkpoint => canonicalFrame(concatCanonicalBytes([
      identityFrame(checkpoint.recordId, 32, 'checkpoint record ID'),
      identityFrame(fileCheckpointDigest(checkpoint), 32, 'checkpoint digest'),
      canonicalFrame(canonicalU64(checkpoint.checkpointGeneration)),
    ]))),
  ]))
}

export function createFSASettlementReceipt(input: Readonly<{
  intent: DirectTreeIntent
  transferJobId: string
  outcome: 'published' | 'partial-directory' | 'resumable-receive'
  request: PlanPauseRequest | PlanSettlementRequest<CompletedTransferWorkerSettlement>
  evidence: ObservedSettlementEvidence
}>): Promise<PersistedReceiveRecord> {
  const bytes = canonicalRecord('windshare/receive-receipt/v1', 1, [
    canonicalU8(FSA_SETTLEMENT_RECEIPT),
    identityFrame(input.intent.operationId, 16, 'operation ID'),
    identityFrame(input.intent.digest, 32, 'receive intent digest'),
    identityFrame(input.intent.plan.reservation.digest, 32, 'reservation digest'),
    identityFrame(input.transferJobId, 16, 'transfer job ID'),
    canonicalFrame(canonicalU8(settlementOutcomeByte(input.outcome))),
    identityFrame(input.evidence.checkpointSetDigest, 32, 'checkpoint set digest'),
    canonicalFrame(canonicalU64(input.request.materialization.entryCount)),
    canonicalFrame(canonicalU64(input.request.materialization.fileCount)),
    canonicalFrame(canonicalU64(input.request.materialization.directoryCount)),
    canonicalFrame(canonicalU64(input.request.materialization.rawBytes)),
    canonicalFrame(canonicalU8(workerStatusByte(input.request.worker.status))),
    canonicalFrame(canonicalU64(BigInt(input.request.worker.failureCount))),
    canonicalFrame(canonicalU64(BigInt(input.request.worker.omittedFailureCount))),
    canonicalFrame(canonicalU64(BigInt(input.evidence.entries.length))),
    ...input.evidence.entries.flatMap(entry => [
      canonicalFrame(canonicalSettlementEntry(entry)),
      ...(entry.kind === 'file'
        ? [identityFrame(entry.checkpoint.recordId, 32, 'checkpoint record ID')]
        : []),
    ]),
    canonicalFrame(canonicalU64(BigInt(input.evidence.directorySettlements.length))),
    ...input.evidence.directorySettlements.map(value => canonicalFrame(
      canonicalDirectorySettlement(value),
    )),
    canonicalFrame(canonicalU64(BigInt(input.request.worker.failures.length))),
    ...input.request.worker.failures.map(failure => canonicalFrame(canonicalFailure(failure))),
  ])
  return createPersistedReceiveRecord({
    operationId: input.intent.operationId,
    kind: RECEIVE_RECORD_RECEIPT,
    canonicalBytes: bytes,
  })
}

export function createFSAUnopenedCleanupReceipt(input: Readonly<{
  intent: DirectTreeIntent
  transferJobId: string
}>): Promise<PersistedReceiveRecord> {
  return createPersistedReceiveRecord({
    operationId: input.intent.operationId,
    kind: RECEIVE_RECORD_CLEANUP,
    canonicalBytes: canonicalRecord('windshare/receive-receipt/v1', 1, [
      canonicalU8(FSA_UNOPENED_CLEANUP_RECEIPT),
      identityFrame(input.intent.operationId, 16, 'operation ID'),
      identityFrame(input.intent.digest, 32, 'receive intent digest'),
      identityFrame(input.intent.plan.reservation.digest, 32, 'reservation digest'),
      identityFrame(input.transferJobId, 16, 'transfer job ID'),
      canonicalFrame(canonicalU64(0n)),
    ]),
  })
}

function canonicalDirectorySettlement(
  value: PersistentDirectorySettlementEvidence,
): CanonicalBytes {
  const admission = value.settlement.admission
  return concatCanonicalBytes([
    canonicalFrame(canonicalSettlementPath(value.artifactPath)),
    canonicalFrame(canonicalU8(
      value.settlement.kind === DirectorySettlementKind.Finalized ? 1 : 2,
    )),
    canonicalFrame(canonicalText(admission.layout)),
    identityFrame(admission.receiveIntentDigest, 32, 'directory receive intent digest'),
    canonicalFrame(canonicalU8(admission.layoutVersion)),
    identityFrame(admission.token, 32, 'directory admission token'),
    identityFrame(admission.directoryId, 16, 'directory ID'),
    identityFrame(admission.generation, 16, 'directory generation'),
    canonicalFrame(canonicalSettlementPath(admission.path)),
    canonicalOptionalIdentity(admission.parentToken, 32, 'parent admission token'),
    canonicalModifiedTime(admission),
  ])
}

function canonicalFailure(failure: TransferFailure): CanonicalBytes {
  return concatCanonicalBytes([
    canonicalU8(failure.kind === 'directory' ? 1 : 2),
    canonicalFrame(canonicalIdentity(
      failure.kind === 'directory' ? failure.directoryId : failure.fileId,
      16,
      failure.kind + ' failure identity',
    )),
  ])
}

function canonicalSettlementEntry(entry: MaterializedManifestEntry): CanonicalBytes {
  if (entry.kind === 'directory') {
    return concatCanonicalBytes([
      canonicalU8(1),
      canonicalFrame(canonicalSettlementPath(entry.artifactPath)),
      identityFrame(entry.directoryId, 16, 'directory ID'),
      identityFrame(entry.generation, 16, 'directory generation'),
      identityFrame(entry.ownedObjectId, 32, 'owned object ID'),
    ])
  }
  return concatCanonicalBytes([
    canonicalU8(2),
    canonicalFrame(canonicalSettlementPath(entry.artifactPath)),
    identityFrame(entry.fileId, 16, 'file ID'),
    identityFrame(entry.fileRevision, 16, 'file revision'),
    canonicalFrame(canonicalU64(entry.exactSize)),
    identityFrame(entry.ownedObjectId, 32, 'owned object ID'),
    identityFrame(entry.checkpoint.recordDigest, 32, 'checkpoint digest'),
    canonicalFrame(canonicalU64(entry.checkpoint.checkpointGeneration)),
  ])
}

function canonicalSettlementPath(path: readonly string[]): CanonicalBytes {
  return path.length === 0 ? canonicalU64(0n) : canonicalPath(path)
}

function canonicalModifiedTime(admission: DirectoryAdmission): CanonicalBytes {
  if (admission.modifiedTime === undefined) return canonicalFrame(canonicalU8(0))
  return canonicalFrame(concatCanonicalBytes([
    canonicalU8(1),
    canonicalFrame(canonicalI64(admission.modifiedTime.seconds)),
    canonicalFrame(canonicalU32(admission.modifiedTime.nanoseconds)),
    canonicalFrame(canonicalU8(admission.modifiedTime.precision)),
  ]))
}

function canonicalOptionalIdentity(
  value: string | undefined,
  width: number,
  label: string,
): CanonicalBytes {
  return canonicalFrame(value === undefined
    ? canonicalU8(0)
    : concatCanonicalBytes([canonicalU8(1), canonicalFrame(canonicalIdentity(value, width, label))]))
}

function identityFrame(value: string, width: number, label: string): CanonicalBytes {
  return canonicalFrame(canonicalIdentity(value, width, label))
}

function settlementOutcomeByte(
  outcome: 'published' | 'partial-directory' | 'resumable-receive',
): number {
  switch (outcome) {
    case 'published': return 1
    case 'partial-directory': return 2
    case 'resumable-receive': return 3
  }
}

function workerStatusByte(status: string): number {
  switch (status) {
    case 'Succeeded': return 1
    case 'CompletedWithErrors': return 2
    case 'Paused': return 3
    default: throw new TypeError('FSA worker settlement status is invalid')
  }
}

function compareEntries(left: MaterializedManifestEntry, right: MaterializedManifestEntry): number {
  const path = comparePath(left.artifactPath, right.artifactPath)
  if (path !== 0) return path
  if (left.kind === right.kind) return 0
  return left.kind === 'directory' ? -1 : 1
}

function comparePath(left: readonly string[], right: readonly string[]): number {
  const limit = Math.min(left.length, right.length)
  for (let index = 0; index < limit; index += 1) {
    if (left[index] === right[index]) continue
    return left[index]! < right[index]! ? -1 : 1
  }
  return left.length - right.length
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
}
