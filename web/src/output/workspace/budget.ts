import {
  validateReceiveIntent,
  type ReceiveIntent,
} from '../../transfer/intent'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalRecord,
  canonicalU8,
  canonicalU64,
  concatCanonicalBytes,
  equalCanonicalBytes,
  snapshotIdentity,
  type CanonicalBytes,
} from './canonical'
import { CanonicalRecordReader } from './canonical-reader'
import {
  validateWorkspaceZipPreparation,
  type SealedWorkspaceZipPreparationV1,
} from './preparation'
import type { PreparationAdmissionReason } from './state'

export const DEFAULT_OPFS_JOB_WORKSPACE_LIMIT = 8_589_934_592n
export const DEFAULT_OPFS_PROCESS_WORKSPACE_LIMIT = 17_179_869_184n
export const MINIMUM_OPFS_QUOTA_RESERVE = 536_870_912n

const WORKSPACE_BUDGET_SCHEMA_VERSION = 1 as const
const U64_MAXIMUM = 0xffff_ffff_ffff_ffffn

export type WorkspaceBudgetEvidence =
  | Readonly<{
      kind: 'single-file'
      fileId: string
      containingDirectoryId: string
      generation: string
      catalogSize: bigint
    }>
  | Readonly<{
      kind: 'prepared-zip'
      preparationManifestDigest: string
      sealedZipLayoutDigest: string
    }>

export interface WorkspaceBudgetV1 {
  readonly schemaVersion: typeof WORKSPACE_BUDGET_SCHEMA_VERSION
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly workspaceBindingDigest: string
  readonly evidence: WorkspaceBudgetEvidence
  readonly uniqueRawBytes: bigint
  readonly packageBytes: bigint
  readonly peakTemporaryBytes: bigint
  readonly durableMetadataBytes: bigint
  readonly peakOwnedBytes: bigint
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface WorkspaceCapacitySnapshot {
  readonly jobLimitBytes: bigint
  readonly processLimitBytes: bigint
  readonly otherActiveJobPeakBytes: bigint
  readonly estimatedQuotaBytes: bigint
  readonly currentUsageBytes: bigint
  readonly minimumReserveBytes: bigint
  readonly verifiedAlreadyOwnedBytes: bigint
}

export type WorkspaceBudgetAdmission =
  | Readonly<{
      kind: 'accepted'
      budgetDigest: string
      incrementalPhysicalPeakBytes: bigint
      limitClass: 'none'
    }>
  | Readonly<{
      kind: 'rejected'
      budgetDigest: string
      reason: Extract<
        PreparationAdmissionReason,
        'job-workspace-limit' | 'process-workspace-limit' | 'quota-insufficient'
      >
      limitClass: 'workspace-job' | 'workspace-process' | 'workspace-quota'
      incrementalPhysicalPeakBytes: bigint
    }>

export async function createPreparedZipWorkspaceBudget(input: {
  readonly receiveIntent: ReceiveIntent
  readonly preparation: SealedWorkspaceZipPreparationV1
  readonly durableMetadataBytes: bigint
}): Promise<WorkspaceBudgetV1> {
  const intent = await validateReceiveIntent(input.receiveIntent)
  if (intent.plan.kind !== 'workspace-then-publish' ||
      intent.artifact.kind !== 'zip-archive' ||
      intent.plan.preparation !== 'exact-zip') {
    throw new TypeError('prepared ZIP workspace budget requires an exact workspace ZIP intent')
  }
  const preparation = await validateWorkspaceZipPreparation(input.preparation, intent)
  if (preparation.manifest.operationId !== intent.operationId ||
      preparation.manifest.receiveIntentDigest !== intent.digest ||
      preparation.manifest.artifactSpecDigest !== intent.artifact.digest) {
    throw new TypeError('workspace budget preparation escaped its receive intent')
  }
  return createWorkspaceBudget({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    workspaceBindingDigest: intent.plan.workspace.digest,
    evidence: Object.freeze({
      kind: 'prepared-zip',
      preparationManifestDigest: preparation.manifest.digest,
      sealedZipLayoutDigest: preparation.zipLayout.digest,
    }),
    uniqueRawBytes: preparation.manifest.selectedRawBytes,
    packageBytes: preparation.zipLayout.exactArchiveBytes,
    peakTemporaryBytes: preparation.zipLayout.maximumSpoolBytes,
    durableMetadataBytes: input.durableMetadataBytes,
  })
}

export async function createSingleFileWorkspaceBudget(input: {
  readonly receiveIntent: ReceiveIntent
  readonly fileId: string
  readonly containingDirectoryId: string
  readonly generation: string
  readonly catalogSize: bigint
  readonly durableMetadataBytes: bigint
}): Promise<WorkspaceBudgetV1> {
  const intent = await validateReceiveIntent(input.receiveIntent)
  if (intent.plan.kind !== 'workspace-then-publish' ||
      intent.artifact.kind !== 'original-file' ||
      intent.plan.preparation !== 'none' ||
      intent.artifact.fileId !== input.fileId) {
    throw new TypeError('single-file workspace budget does not match its receive intent')
  }
  const catalogSize = checkedU64(input.catalogSize, 'single-file catalog size')
  return createWorkspaceBudget({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    workspaceBindingDigest: intent.plan.workspace.digest,
    evidence: Object.freeze({
      kind: 'single-file',
      fileId: snapshotIdentity(input.fileId, 16, 'file ID'),
      containingDirectoryId: snapshotIdentity(
        input.containingDirectoryId,
        16,
        'containing directory ID',
      ),
      generation: snapshotIdentity(input.generation, 16, 'directory generation'),
      catalogSize,
    }),
    uniqueRawBytes: catalogSize,
    // Promoting the sole raw object changes its accounting class without allocating it twice.
    packageBytes: 0n,
    peakTemporaryBytes: 0n,
    durableMetadataBytes: input.durableMetadataBytes,
  })
}

/** Rebuilds the canonical budget so a recovered claim cannot trust mutable structured fields. */
export async function validateWorkspaceBudget(
  candidate: WorkspaceBudgetV1,
  receiveIntent: ReceiveIntent,
): Promise<WorkspaceBudgetV1> {
  const intent = await validateReceiveIntent(receiveIntent)
  if (intent.plan.kind !== 'workspace-then-publish' ||
      candidate.schemaVersion !== WORKSPACE_BUDGET_SCHEMA_VERSION ||
      candidate.operationId !== intent.operationId ||
      candidate.receiveIntentDigest !== intent.digest ||
      candidate.workspaceBindingDigest !== intent.plan.workspace.digest) {
    throw new TypeError('workspace budget escaped its receive intent')
  }
  if ((candidate.evidence.kind === 'single-file' &&
       (intent.artifact.kind !== 'original-file' ||
        intent.plan.preparation !== 'none' ||
        candidate.evidence.fileId !== intent.artifact.fileId ||
        candidate.uniqueRawBytes !== candidate.evidence.catalogSize ||
        candidate.packageBytes !== 0n ||
        candidate.peakTemporaryBytes !== 0n)) ||
      (candidate.evidence.kind === 'prepared-zip' &&
       (intent.artifact.kind !== 'zip-archive' || intent.plan.preparation !== 'exact-zip'))) {
    throw new TypeError('workspace budget evidence disagrees with the artifact')
  }
  const rebuilt = await createWorkspaceBudget({
    operationId: candidate.operationId,
    receiveIntentDigest: candidate.receiveIntentDigest,
    workspaceBindingDigest: candidate.workspaceBindingDigest,
    evidence: candidate.evidence,
    uniqueRawBytes: candidate.uniqueRawBytes,
    packageBytes: candidate.packageBytes,
    peakTemporaryBytes: candidate.peakTemporaryBytes,
    durableMetadataBytes: candidate.durableMetadataBytes,
  })
  if (candidate.peakOwnedBytes !== rebuilt.peakOwnedBytes ||
      candidate.digest !== rebuilt.digest ||
      !equalCanonicalBytes(candidate.canonicalBytes, rebuilt.canonicalBytes)) {
    throw new TypeError('workspace budget canonical authority changed')
  }
  return rebuilt
}

/** Decodes persisted budget bytes, then rebuilds them under the canonical ReceiveIntent. */
export async function decodeWorkspaceBudgetV1(
  canonicalBytes: Uint8Array,
  receiveIntent: ReceiveIntent,
): Promise<WorkspaceBudgetV1> {
  const reader = CanonicalRecordReader.open(
    canonicalBytes,
    'windshare/workspace-budget/v1',
    WORKSPACE_BUDGET_SCHEMA_VERSION,
  )
  const operationId = reader.framedIdentity(16, 'operation ID')
  const receiveIntentDigest = reader.framedIdentity(32, 'receive intent digest')
  const workspaceBindingDigest = reader.framedIdentity(32, 'workspace binding digest')
  const evidence = decodeBudgetEvidence(reader.frame('workspace budget evidence'))
  const uniqueRawBytes = reader.framedU64('unique raw bytes')
  const packageBytes = reader.framedU64('package bytes')
  const peakTemporaryBytes = reader.framedU64('peak temporary bytes')
  const durableMetadataBytes = reader.framedU64('durable metadata bytes')
  const peakOwnedBytes = reader.framedU64('peak owned bytes')
  reader.finish('workspace budget')
  const rebuilt = await createWorkspaceBudget({
    operationId,
    receiveIntentDigest,
    workspaceBindingDigest,
    evidence,
    uniqueRawBytes,
    packageBytes,
    peakTemporaryBytes,
    durableMetadataBytes,
  })
  if (rebuilt.peakOwnedBytes !== peakOwnedBytes ||
      !equalCanonicalBytes(rebuilt.canonicalBytes, canonicalBytes)) {
    throw new TypeError('decoded workspace budget changed its canonical authority')
  }
  return validateWorkspaceBudget(rebuilt, receiveIntent)
}

export function admitWorkspaceBudget(
  budget: WorkspaceBudgetV1,
  capacity: WorkspaceCapacitySnapshot,
): WorkspaceBudgetAdmission {
  const snapshot = snapshotCapacity(capacity)
  const incrementalPhysicalPeakBytes = budget.peakOwnedBytes > snapshot.verifiedAlreadyOwnedBytes
    ? budget.peakOwnedBytes - snapshot.verifiedAlreadyOwnedBytes
    : 0n
  if (budget.peakOwnedBytes > snapshot.jobLimitBytes) {
    return rejection(
      budget,
      incrementalPhysicalPeakBytes,
      'job-workspace-limit',
      'workspace-job',
    )
  }
  if (checkedAdd(snapshot.otherActiveJobPeakBytes, budget.peakOwnedBytes) >
      snapshot.processLimitBytes) {
    return rejection(
      budget,
      incrementalPhysicalPeakBytes,
      'process-workspace-limit',
      'workspace-process',
    )
  }
  const available = snapshot.estimatedQuotaBytes >= snapshot.currentUsageBytes
    ? snapshot.estimatedQuotaBytes - snapshot.currentUsageBytes
    : 0n
  const afterReserve = available >= snapshot.minimumReserveBytes
    ? available - snapshot.minimumReserveBytes
    : 0n
  if (afterReserve < incrementalPhysicalPeakBytes) {
    return rejection(
      budget,
      incrementalPhysicalPeakBytes,
      'quota-insufficient',
      'workspace-quota',
    )
  }
  return Object.freeze({
    kind: 'accepted',
    budgetDigest: budget.digest,
    incrementalPhysicalPeakBytes,
    limitClass: 'none',
  })
}

async function createWorkspaceBudget(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly workspaceBindingDigest: string
  readonly evidence: WorkspaceBudgetEvidence
  readonly uniqueRawBytes: bigint
  readonly packageBytes: bigint
  readonly peakTemporaryBytes: bigint
  readonly durableMetadataBytes: bigint
}): Promise<WorkspaceBudgetV1> {
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const receiveIntentDigest = snapshotIdentity(input.receiveIntentDigest, 32, 'receive intent digest')
  const workspaceBindingDigest = snapshotIdentity(
    input.workspaceBindingDigest,
    32,
    'workspace binding digest',
  )
  const evidence = snapshotEvidence(input.evidence)
  const uniqueRawBytes = checkedU64(input.uniqueRawBytes, 'unique raw bytes')
  const packageBytes = checkedU64(input.packageBytes, 'package bytes')
  const peakTemporaryBytes = checkedU64(input.peakTemporaryBytes, 'peak temporary bytes')
  const durableMetadataBytes = checkedU64(input.durableMetadataBytes, 'durable metadata bytes')
  const peakOwnedBytes = checkedAdd(
    uniqueRawBytes,
    packageBytes,
    peakTemporaryBytes,
    durableMetadataBytes,
  )
  const canonicalBytes = canonicalRecord('windshare/workspace-budget/v1', 1, [
    canonicalFrame(canonicalIdentity(operationId, 16, 'operation ID')),
    canonicalFrame(canonicalIdentity(receiveIntentDigest, 32, 'receive intent digest')),
    canonicalFrame(canonicalIdentity(workspaceBindingDigest, 32, 'workspace binding digest')),
    canonicalFrame(canonicalBudgetEvidence(evidence)),
    canonicalFrame(canonicalU64(uniqueRawBytes)),
    canonicalFrame(canonicalU64(packageBytes)),
    canonicalFrame(canonicalU64(peakTemporaryBytes)),
    canonicalFrame(canonicalU64(durableMetadataBytes)),
    canonicalFrame(canonicalU64(peakOwnedBytes)),
  ])
  return Object.freeze({
    schemaVersion: WORKSPACE_BUDGET_SCHEMA_VERSION,
    operationId,
    receiveIntentDigest,
    workspaceBindingDigest,
    evidence,
    uniqueRawBytes,
    packageBytes,
    peakTemporaryBytes,
    durableMetadataBytes,
    peakOwnedBytes,
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

function snapshotEvidence(evidence: WorkspaceBudgetEvidence): WorkspaceBudgetEvidence {
  if (evidence.kind === 'single-file') {
    return Object.freeze({
      kind: 'single-file',
      fileId: snapshotIdentity(evidence.fileId, 16, 'file ID'),
      containingDirectoryId: snapshotIdentity(
        evidence.containingDirectoryId,
        16,
        'containing directory ID',
      ),
      generation: snapshotIdentity(evidence.generation, 16, 'directory generation'),
      catalogSize: checkedU64(evidence.catalogSize, 'catalog size'),
    })
  }
  if (evidence.kind !== 'prepared-zip') throw new TypeError('workspace budget evidence is invalid')
  return Object.freeze({
    kind: 'prepared-zip',
    preparationManifestDigest: snapshotIdentity(
      evidence.preparationManifestDigest,
      32,
      'preparation manifest digest',
    ),
    sealedZipLayoutDigest: snapshotIdentity(
      evidence.sealedZipLayoutDigest,
      32,
      'sealed ZIP layout digest',
    ),
  })
}

function decodeBudgetEvidence(canonicalBytes: Uint8Array): WorkspaceBudgetEvidence {
  const reader = CanonicalRecordReader.value(canonicalBytes)
  const discriminant = reader.byte('workspace budget evidence discriminant')
  if (discriminant === 1) {
    const evidence = Object.freeze({
      kind: 'single-file' as const,
      fileId: reader.framedIdentity(16, 'file ID'),
      containingDirectoryId: reader.framedIdentity(16, 'containing directory ID'),
      generation: reader.framedIdentity(16, 'directory generation'),
      catalogSize: reader.framedU64('catalog size'),
    })
    reader.finish('single-file workspace budget evidence')
    return evidence
  }
  if (discriminant !== 2) throw new TypeError('workspace budget evidence discriminant is invalid')
  const evidence = Object.freeze({
    kind: 'prepared-zip' as const,
    preparationManifestDigest: reader.framedIdentity(32, 'preparation manifest digest'),
    sealedZipLayoutDigest: reader.framedIdentity(32, 'sealed ZIP layout digest'),
  })
  reader.finish('prepared ZIP workspace budget evidence')
  return evidence
}

function canonicalBudgetEvidence(evidence: WorkspaceBudgetEvidence): CanonicalBytes {
  if (evidence.kind === 'single-file') {
    return concatCanonicalBytes([
      canonicalU8(1),
      canonicalFrame(canonicalIdentity(evidence.fileId, 16, 'file ID')),
      canonicalFrame(canonicalIdentity(
        evidence.containingDirectoryId,
        16,
        'containing directory ID',
      )),
      canonicalFrame(canonicalIdentity(evidence.generation, 16, 'directory generation')),
      canonicalFrame(canonicalU64(evidence.catalogSize)),
    ])
  }
  return concatCanonicalBytes([
    canonicalU8(2),
    canonicalFrame(canonicalIdentity(
      evidence.preparationManifestDigest,
      32,
      'preparation manifest digest',
    )),
    canonicalFrame(canonicalIdentity(
      evidence.sealedZipLayoutDigest,
      32,
      'sealed ZIP layout digest',
    )),
  ])
}

function snapshotCapacity(input: WorkspaceCapacitySnapshot): WorkspaceCapacitySnapshot {
  const snapshot = Object.freeze({
    jobLimitBytes: checkedU64(input.jobLimitBytes, 'workspace job limit'),
    processLimitBytes: checkedU64(input.processLimitBytes, 'workspace process limit'),
    otherActiveJobPeakBytes: checkedU64(
      input.otherActiveJobPeakBytes,
      'other active workspace peaks',
    ),
    estimatedQuotaBytes: checkedU64(input.estimatedQuotaBytes, 'estimated quota'),
    currentUsageBytes: checkedU64(input.currentUsageBytes, 'current quota usage'),
    minimumReserveBytes: checkedU64(input.minimumReserveBytes, 'minimum quota reserve'),
    verifiedAlreadyOwnedBytes: checkedU64(
      input.verifiedAlreadyOwnedBytes,
      'verified already-owned bytes',
    ),
  })
  if (snapshot.jobLimitBytes === 0n || snapshot.processLimitBytes === 0n) {
    throw new TypeError('workspace capacity limits must be positive')
  }
  return snapshot
}

function rejection(
  budget: WorkspaceBudgetV1,
  incrementalPhysicalPeakBytes: bigint,
  reason: Extract<
    PreparationAdmissionReason,
    'job-workspace-limit' | 'process-workspace-limit' | 'quota-insufficient'
  >,
  limitClass: 'workspace-job' | 'workspace-process' | 'workspace-quota',
): WorkspaceBudgetAdmission {
  return Object.freeze({
    kind: 'rejected',
    budgetDigest: budget.digest,
    reason,
    limitClass,
    incrementalPhysicalPeakBytes,
  })
}

function checkedAdd(...values: readonly bigint[]): bigint {
  let total = 0n
  for (const value of values) {
    total += value
    if (total > U64_MAXIMUM) throw new RangeError('workspace budget arithmetic overflow')
  }
  return total
}

function checkedU64(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n || value > U64_MAXIMUM) {
    throw new TypeError(`${label} is not a u64`)
  }
  return value
}
