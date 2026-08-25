import {
  fileCheckpointDigest,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalPath,
  canonicalRecord,
  canonicalText,
  canonicalU64,
  canonicalU8,
  concatCanonicalBytes,
  type CanonicalBytes,
} from '../workspace/canonical'
import {
  RECEIVE_RECORD_CLEANUP,
  RECEIVE_RECORD_RECEIPT,
  createPersistedReceiveRecord,
  type PersistedReceiveRecord,
} from '../workspace/records'
import type { DirectoryAdmissionScope } from '../../transfer/directory-admission'
import type { DirectTreeRootExpectation } from '../../transfer/job/coordinate/direct-tree'
import {
  validateReceiveIntent,
  type DirectoryTreeArtifact,
  type DirectTreePlan,
  type ReceiveIntent,
} from '../../transfer/intent'
import type {
  PlanPauseRequest,
  PlanSettlementRequest,
  PlanStopRequest,
} from '../../transfer/output-session'
import type {
  CompletedTransferWorkerSettlement,
  TransferFailure,
} from '../../transfer/outcome'

const RECEIVE_RECEIPT_DOMAIN = 'windshare/receive-receipt/v1'
const RECEIVE_RECEIPT_SCHEMA_VERSION = 1
const FSA_SEALED_LEDGER_SETTLEMENT_RECEIPT_V1 = 16
const FSA_UNOPENED_CLEANUP_RECEIPT_V2 = 15

export type DirectTreeIntent = ReceiveIntent & Readonly<{
  plan: DirectTreePlan
  artifact: DirectoryTreeArtifact
}>

export interface SettlementReceiptEvidence {
  readonly sealId: string
  readonly sealDigest: string
  readonly aggregateRoot: string
  readonly pageCount: bigint
  readonly entryEventCount: bigint
  readonly fileCount: bigint
  readonly visibleDirectoryCount: bigint
  readonly materializedDirectoryCount: bigint
  readonly finalizedDirectoryCount: bigint
  readonly isolatedDirectoryCount: bigint
  readonly completedBytes: bigint
  readonly checkpointCount: bigint
  readonly checkpointSetDigest?: string
}

export async function requireDirectTreeIntent(input: ReceiveIntent): Promise<DirectTreeIntent> {
  const intent = await validateReceiveIntent(input)
  if (intent.plan.kind !== 'direct-tree' || intent.artifact.kind !== 'directory-tree' ||
      intent.plan.reservation.authorityKind !== 'fsa-container' ||
      intent.plan.reservation.guarantees.profile !== 'fsa-tree' ||
      intent.plan.reservation.guarantees.delivery !== 'managed-target' ||
      intent.plan.reservation.guarantees.replacement !== 'coordinated-no-replace' ||
      intent.plan.reservation.guarantees.targetVisibility !== 'committed-objects-visible') {
    throw new TypeError(
      'FSA settlement requires managed, coordinated, committed-object-visible output',
    )
  }
  return intent as DirectTreeIntent
}

export async function fsaCheckpointSetDigest(
  intent: DirectTreeIntent,
  checkpoints: readonly FileCheckpointV2[],
): Promise<string> {
  return fsaCheckpointReferenceSetDigest(intent, checkpoints.map(checkpoint => Object.freeze({
    recordId: checkpoint.recordId,
    recordDigest: fileCheckpointDigest(checkpoint),
    checkpointGeneration: checkpoint.checkpointGeneration,
  })))
}

export function fsaCheckpointReferenceSetDigest(
  intent: DirectTreeIntent,
  checkpoints: readonly Readonly<{
    recordId: string
    recordDigest: string
    checkpointGeneration: bigint
  }>[],
): Promise<string> {
  return canonicalDigest(canonicalRecord('windshare/fsa-checkpoint-set/v1', 1, [
    identityFrame(intent.operationId, 16, 'operation ID'),
    identityFrame(intent.digest, 32, 'receive intent digest'),
    identityFrame(intent.plan.reservation.digest, 32, 'reservation digest'),
    canonicalFrame(canonicalU64(BigInt(checkpoints.length))),
    ...checkpoints.map(checkpoint => canonicalFrame(concatCanonicalBytes([
      identityFrame(checkpoint.recordId, 32, 'checkpoint record ID'),
      identityFrame(checkpoint.recordDigest, 32, 'checkpoint digest'),
      canonicalFrame(canonicalU64(checkpoint.checkpointGeneration)),
    ]))),
  ]))
}

export function createFSASettlementReceipt(input: Readonly<{
  intent: DirectTreeIntent
  transferJobId: string
  outcome: 'published' | 'partial-directory' | 'resumable-receive'
  request: PlanPauseRequest | PlanSettlementRequest<CompletedTransferWorkerSettlement> | PlanStopRequest
  evidence: SettlementReceiptEvidence
  directoryScope: DirectoryAdmissionScope
}>): Promise<PersistedReceiveRecord> {
  const bytes = canonicalRecord(RECEIVE_RECEIPT_DOMAIN, RECEIVE_RECEIPT_SCHEMA_VERSION, [
    canonicalU8(FSA_SEALED_LEDGER_SETTLEMENT_RECEIPT_V1),
    identityFrame(input.intent.operationId, 16, 'operation ID'),
    identityFrame(input.intent.digest, 32, 'receive intent digest'),
    identityFrame(input.intent.plan.reservation.digest, 32, 'reservation digest'),
    ...canonicalFSASettlementBinding(input.intent, input.directoryScope),
    identityFrame(input.transferJobId, 16, 'transfer job ID'),
    canonicalFrame(canonicalU8(settlementOutcomeByte(input.outcome))),
    identityFrame(input.evidence.sealId, 32, 'materialization ledger seal ID'),
    identityFrame(input.evidence.sealDigest, 32, 'materialization ledger seal digest'),
    identityFrame(input.evidence.aggregateRoot, 32, 'materialization ledger aggregate root'),
    canonicalFrame(canonicalU64(input.evidence.pageCount)),
    canonicalFrame(canonicalU64(input.evidence.entryEventCount)),
    canonicalFrame(canonicalU64(input.evidence.fileCount)),
    canonicalFrame(canonicalU64(input.evidence.visibleDirectoryCount)),
    canonicalFrame(canonicalU64(input.evidence.materializedDirectoryCount)),
    canonicalFrame(canonicalU64(input.evidence.finalizedDirectoryCount)),
    canonicalFrame(canonicalU64(input.evidence.isolatedDirectoryCount)),
    canonicalFrame(canonicalU64(input.evidence.completedBytes)),
    canonicalFrame(canonicalU64(input.evidence.checkpointCount)),
    canonicalOptionalIdentity(input.evidence.checkpointSetDigest, 32, 'checkpoint set digest'),
    canonicalFrame(canonicalU64(input.request.materialization.entryCount)),
    canonicalFrame(canonicalU64(input.request.materialization.fileCount)),
    canonicalFrame(canonicalU64(input.request.materialization.directoryCount)),
    canonicalFrame(canonicalU64(input.request.materialization.rawBytes)),
    canonicalFrame(canonicalU8(workerStatusByte(input.request.worker.status))),
    canonicalFrame(canonicalU64(BigInt(input.request.worker.failureCount))),
    canonicalFrame(canonicalU64(BigInt(input.request.worker.omittedFailureCount))),
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
  directoryScope: DirectoryAdmissionScope
}>): Promise<PersistedReceiveRecord> {
  return createPersistedReceiveRecord({
    operationId: input.intent.operationId,
    kind: RECEIVE_RECORD_CLEANUP,
    canonicalBytes: canonicalRecord(RECEIVE_RECEIPT_DOMAIN, RECEIVE_RECEIPT_SCHEMA_VERSION, [
      canonicalU8(FSA_UNOPENED_CLEANUP_RECEIPT_V2),
      identityFrame(input.intent.operationId, 16, 'operation ID'),
      identityFrame(input.intent.digest, 32, 'receive intent digest'),
      identityFrame(input.intent.plan.reservation.digest, 32, 'reservation digest'),
      ...canonicalFSASettlementBinding(input.intent, input.directoryScope),
      identityFrame(input.transferJobId, 16, 'transfer job ID'),
      canonicalFrame(canonicalU64(0n)),
    ]),
  })
}

function canonicalFSASettlementBinding(
  intent: DirectTreeIntent,
  scope: DirectoryAdmissionScope,
): readonly CanonicalBytes[] {
  const reservation = intent.plan.reservation
  if (reservation.kind !== 'named-container-entry' ||
      reservation.authorityKind !== 'fsa-container') {
    throw new TypeError('FSA receipt requires a reserved-root-relative layout binding')
  }
  if (scope.receiveIntentDigest !== intent.digest) {
    throw new TypeError('FSA receipt root expectation belongs to another receive intent')
  }
  return Object.freeze([
    canonicalFrame(canonicalU8(reservation.fsaLayoutVersion)),
    canonicalFrame(canonicalU8(scope.layoutVersion)),
    canonicalFrame(canonicalText(scope.layout)),
    canonicalFrame(canonicalRootExpectation(scope.rootExpectation)),
  ])
}

function canonicalRootExpectation(expectation: DirectTreeRootExpectation): CanonicalBytes {
  if (expectation.kind === 'none') {
    return concatCanonicalBytes([
      canonicalU8(0),
      canonicalFrame(canonicalU8(rootAnchorKindByte(expectation.anchorKind))),
    ])
  }
  return concatCanonicalBytes([
    canonicalU8(1),
    canonicalFrame(canonicalU8(rootAnchorKindByte(expectation.anchorKind))),
    identityFrame(expectation.directoryId, 16, 'expected root directory ID'),
    canonicalFrame(expectation.relativePath.length === 0
      ? canonicalU64(0n)
      : canonicalPath(expectation.relativePath)),
  ])
}

function rootAnchorKindByte(anchorKind: DirectTreeRootExpectation['anchorKind']): number {
  switch (anchorKind) {
    case 'single-file': return 1
    case 'directory': return 2
    case 'synthetic-root': return 3
    case 'catalog-root': return 4
  }
}

function canonicalFailure(failure: TransferFailure): CanonicalBytes {
  return concatCanonicalBytes([
    canonicalU8(failure.kind === 'directory' ? 1 : 2),
    canonicalFrame(canonicalIdentity(
      failure.kind === 'directory' ? failure.directoryId : failure.fileId,
      16,
      `${failure.kind} failure identity`,
    )),
  ])
}

function canonicalOptionalIdentity(
  value: string | undefined,
  width: number,
  label: string,
): CanonicalBytes {
  return canonicalFrame(value === undefined
    ? canonicalU8(0)
    : concatCanonicalBytes([canonicalU8(1), identityFrame(value, width, label)]))
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
