import { BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS } from '../../transfer/intent'
import type { ReceiveIntent } from '../../transfer/intent'
import { encodeBase64Url } from '../../crypto/bytes'
import {
  canonicalBoolean,
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalRecord,
  canonicalText,
  canonicalU8,
  canonicalU64,
  canonicalUnixMilliseconds,
  concatCanonicalBytes,
  equalCanonicalBytes,
  snapshotCanonicalBytes,
  snapshotIdentity,
  type CanonicalBytes,
} from './canonical'
import {
  decodeWorkspaceBudgetV1,
  type WorkspaceBudgetV1,
} from './budget'
import {
  createPersistedReceiveRecord,
  RECEIVE_RECORD_CLEANUP,
  RECEIVE_RECORD_RECEIPT,
  type PersistedReceiveRecord,
} from './records'
import type { ReceiveLifecycleState } from './state'
import { CanonicalRecordReader } from './canonical-reader'

const RECEIPT_SCHEMA_VERSION = 1 as const
const RECEIPT_PREPARATION_ADMISSION = 5 as const
const RECEIPT_WORKSPACE_SEAL = 6 as const
const RECEIPT_PACKAGE = 7 as const
const RECEIPT_MANAGED_PUBLICATION = 8 as const
const RECEIPT_HANDOFF = 9 as const
const RECEIPT_EXPIRY = 10 as const
const RECEIPT_CLEANUP = 11 as const
const RECEIPT_PACKAGE_TEMPORARY_CLEANUP = 12 as const
const U64_MAXIMUM = 0xffff_ffff_ffff_ffffn
const RECEIVE_RECEIPT_PREFIX = canonicalRecord('windshare/receive-receipt/v1', 1, [])
const RAW_WORKSPACE_RECEIPT_DOMAIN = 'windshare/raw-workspace-receipt/v1'

interface ReceiveReceiptBase {
  readonly schemaVersion: typeof RECEIPT_SCHEMA_VERSION
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface PreparationAdmissionReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'preparation-admission'
  readonly preparationManifestDigest?: string
  readonly sealedZipLayoutDigest?: string
  readonly workspaceBudgetDigest: string
  readonly contentRequestCountAtAdmission: 0n
  readonly jobLimitBytes: bigint
  readonly processLimitBytes: bigint
  readonly estimatedQuotaBytes: bigint
  readonly currentUsageBytes: bigint
  readonly minimumReserveBytes: bigint
  readonly incrementalPhysicalPeakBytes: bigint
}

export interface OwnedWorkspaceObjectReceipt {
  readonly ownedObjectId: string
  readonly exactBytes: bigint
}

export interface RawWorkspaceReceiptV1 {
  readonly schemaVersion: typeof RECEIPT_SCHEMA_VERSION
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly workspaceBindingDigest: string
  readonly materializedManifestDigest: string
  readonly ownedObjects: readonly OwnedWorkspaceObjectReceipt[]
  readonly uniqueRawBytes: bigint
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface WorkspaceSealReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'workspace-seal'
  readonly workspaceBindingDigest: string
  readonly sealedMaterializationDigest: string
  readonly rawWorkspaceReceipt: RawWorkspaceReceiptV1
}

interface ArtifactVerificationReceiptBase {
  readonly schemaVersion: typeof RECEIPT_SCHEMA_VERSION
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly sealedMaterializationDigest: string
  readonly layoutDigest: string
  readonly packageOwnedObjectId: string
  readonly exactBytes: bigint
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface ZipArtifactVerificationReceiptV1 extends ArtifactVerificationReceiptBase {
  readonly kind: 'zip-writer'
  readonly writerCloseVerified: true
}

export interface OriginalFileArtifactVerificationReceiptV1 extends ArtifactVerificationReceiptBase {
  readonly kind: 'original-file-promotion'
  readonly finalCheckpointDigest: string
  readonly finalCheckpointGeneration: bigint
  readonly promotionVerified: true
}

export type ArtifactVerificationReceiptV1 =
  | ZipArtifactVerificationReceiptV1
  | OriginalFileArtifactVerificationReceiptV1

export interface PackageReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'package'
  readonly packagedArtifactDigest: string
  readonly artifactVerification: ArtifactVerificationReceiptV1
}

export interface ManagedPublicationReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'managed-publication'
  readonly publicationAttemptId: string
  readonly packagedArtifactDigest: string
  readonly reservationDigest: string
  readonly targetAuthorityRef: string
  readonly exactBytes: bigint
  readonly commitConfirmed: true
}

export interface HandoffReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'handoff'
  readonly publicationAttemptId: string
  readonly packagedArtifactDigest: string
  readonly suggestedName: string
  readonly urlLeaseEndsAt: number
  readonly handoffStarted: true
}

export interface ExpiryReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'expiry'
  readonly priorStableState:
    | 'resumable-receive'
    | 'resumable-package'
    | 'waiting-to-save'
    | 'download-started'
  readonly expiresAt: number
  readonly retainedSuccessCount: bigint
  readonly cleanupState: 'clean' | 'cleanup-pending'
}

export interface CleanupReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'cleanup'
  readonly removedObjectIds: readonly string[]
  readonly removedRecordDigests: readonly string[]
  readonly cleanupGeneration: bigint
}

export interface PackageTemporaryCleanupReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'package-temporary-cleanup'
  readonly sealedMaterializationDigest: string
  readonly packageOwnedObjectId: string
  readonly packageHandleId: string
  readonly cleanupResult: 'removed' | 'already-absent'
  readonly cleanupProofDigest: string
}

export type WorkspaceReceiveReceiptV1 =
  | PreparationAdmissionReceiptV1
  | WorkspaceSealReceiptV1
  | PackageReceiptV1
  | ManagedPublicationReceiptV1
  | HandoffReceiptV1
  | ExpiryReceiptV1
  | CleanupReceiptV1
  | PackageTemporaryCleanupReceiptV1

export async function createPreparationAdmissionReceipt(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly preparationManifestDigest?: string
  readonly sealedZipLayoutDigest?: string
  readonly workspaceBudget: WorkspaceBudgetV1
  readonly contentRequestCountAtAdmission: bigint
  readonly jobLimitBytes: bigint
  readonly processLimitBytes: bigint
  readonly estimatedQuotaBytes: bigint
  readonly currentUsageBytes: bigint
  readonly minimumReserveBytes: bigint
  readonly incrementalPhysicalPeakBytes: bigint
}): Promise<PreparationAdmissionReceiptV1> {
  const identity = receiptIdentity(input)
  if (input.contentRequestCountAtAdmission !== 0n) {
    throw new TypeError('workspace admission occurred after a content request')
  }
  if (input.workspaceBudget.operationId !== identity.operationId ||
      input.workspaceBudget.receiveIntentDigest !== identity.receiveIntentDigest) {
    throw new TypeError('workspace budget escaped its admission receipt')
  }
  const preparationManifestDigest = optionalDigest(
    input.preparationManifestDigest,
    'preparation manifest digest',
  )
  const sealedZipLayoutDigest = optionalDigest(input.sealedZipLayoutDigest, 'ZIP layout digest')
  if ((preparationManifestDigest === undefined) !== (sealedZipLayoutDigest === undefined)) {
    throw new TypeError('workspace ZIP admission evidence is incomplete')
  }
  if (input.workspaceBudget.evidence.kind === 'prepared-zip') {
    if (preparationManifestDigest !== input.workspaceBudget.evidence.preparationManifestDigest ||
        sealedZipLayoutDigest !== input.workspaceBudget.evidence.sealedZipLayoutDigest) {
      throw new TypeError('workspace ZIP admission evidence changed')
    }
  } else if (preparationManifestDigest !== undefined) {
    throw new TypeError('single-file workspace budget cannot bind ZIP preparation')
  }
  const limits = snapshotAdmissionLimits(input)
  const variantFields = [
    canonicalFrame(canonicalOptionalDigest(preparationManifestDigest)),
    canonicalFrame(canonicalOptionalDigest(sealedZipLayoutDigest)),
    canonicalFrame(input.workspaceBudget.canonicalBytes),
    canonicalFrame(canonicalIdentity(input.workspaceBudget.digest, 32, 'workspace budget digest')),
    canonicalFrame(canonicalU64(0n)),
    canonicalFrame(canonicalU64(limits.jobLimitBytes)),
    canonicalFrame(canonicalU64(limits.processLimitBytes)),
    canonicalFrame(canonicalU64(limits.estimatedQuotaBytes)),
    canonicalFrame(canonicalU64(limits.currentUsageBytes)),
    canonicalFrame(canonicalU64(limits.minimumReserveBytes)),
    canonicalFrame(canonicalU64(limits.incrementalPhysicalPeakBytes)),
  ]
  const completed = await completeReceipt(identity, RECEIPT_PREPARATION_ADMISSION, variantFields)
  return Object.freeze({
    ...completed,
    kind: 'preparation-admission',
    ...(preparationManifestDigest === undefined ? {} : { preparationManifestDigest }),
    ...(sealedZipLayoutDigest === undefined ? {} : { sealedZipLayoutDigest }),
    workspaceBudgetDigest: input.workspaceBudget.digest,
    contentRequestCountAtAdmission: 0n,
    ...limits,
  })
}

/** Decodes only the durable admission receipt needed to reissue a post-crash content gate. */
export async function decodePreparationAdmissionReceipt(
  record: PersistedReceiveRecord,
  workspaceBudget: WorkspaceBudgetV1,
): Promise<PreparationAdmissionReceiptV1 | undefined> {
  if (record.kind !== RECEIVE_RECORD_RECEIPT) return undefined
  const reader = new ReceiptReader(record.canonicalBytes)
  reader.prefix(RECEIVE_RECEIPT_PREFIX)
  const discriminant = reader.byte()
  if (discriminant !== RECEIPT_PREPARATION_ADMISSION) return undefined
  const operationId = reader.identity(16, 'operation ID')
  const receiveIntentDigest = reader.identity(32, 'receive intent digest')
  const preparationManifestDigest = reader.optionalDigest('preparation manifest digest')
  const sealedZipLayoutDigest = reader.optionalDigest('sealed ZIP layout digest')
  const budgetBytes = reader.frame()
  const workspaceBudgetDigest = reader.identity(32, 'workspace budget digest')
  const contentRequestCountAtAdmission = reader.u64('content request count')
  const jobLimitBytes = reader.u64('job workspace limit')
  const processLimitBytes = reader.u64('process workspace limit')
  const estimatedQuotaBytes = reader.u64('estimated quota')
  const currentUsageBytes = reader.u64('current quota usage')
  const minimumReserveBytes = reader.u64('quota reserve')
  const incrementalPhysicalPeakBytes = reader.u64('incremental physical peak')
  reader.end()

  const digest = await canonicalDigest(record.canonicalBytes)
  if (record.operationId !== operationId || record.digest !== digest ||
      workspaceBudget.operationId !== operationId ||
      workspaceBudget.receiveIntentDigest !== receiveIntentDigest ||
      workspaceBudget.digest !== workspaceBudgetDigest ||
      !equalCanonicalBytes(workspaceBudget.canonicalBytes, budgetBytes) ||
      contentRequestCountAtAdmission !== 0n) {
    throw new TypeError('preparation admission receipt authority changed')
  }
  if (workspaceBudget.evidence.kind === 'single-file') {
    if (preparationManifestDigest !== undefined || sealedZipLayoutDigest !== undefined) {
      throw new TypeError('single-file admission unexpectedly binds ZIP preparation')
    }
  } else if (preparationManifestDigest !== workspaceBudget.evidence.preparationManifestDigest ||
      sealedZipLayoutDigest !== workspaceBudget.evidence.sealedZipLayoutDigest) {
    throw new TypeError('ZIP admission receipt changed its preparation evidence')
  }
  return Object.freeze({
    schemaVersion: RECEIPT_SCHEMA_VERSION,
    operationId,
    receiveIntentDigest,
    kind: 'preparation-admission',
    ...(preparationManifestDigest === undefined ? {} : { preparationManifestDigest }),
    ...(sealedZipLayoutDigest === undefined ? {} : { sealedZipLayoutDigest }),
    workspaceBudgetDigest,
    contentRequestCountAtAdmission: 0n,
    jobLimitBytes,
    processLimitBytes,
    estimatedQuotaBytes,
    currentUsageBytes,
    minimumReserveBytes,
    incrementalPhysicalPeakBytes,
    canonicalBytes: snapshotCanonicalBytes(record.canonicalBytes),
    digest,
  })
}

export async function decodePreparationAdmissionAuthority(
  record: PersistedReceiveRecord,
  receiveIntent: ReceiveIntent,
): Promise<Readonly<{
  budget: WorkspaceBudgetV1
  receipt: PreparationAdmissionReceiptV1
}> | undefined> {
  const budgetBytes = preparationAdmissionBudgetBytes(record)
  if (budgetBytes === undefined) return undefined
  const budget = await decodeWorkspaceBudgetV1(budgetBytes, receiveIntent)
  const receipt = await decodePreparationAdmissionReceipt(record, budget)
  if (receipt === undefined) throw new TypeError('admission budget lacks its receipt authority')
  return Object.freeze({ budget, receipt })
}

function preparationAdmissionBudgetBytes(
  record: PersistedReceiveRecord,
): CanonicalBytes | undefined {
  if (record.kind !== RECEIVE_RECORD_RECEIPT) return undefined
  const reader = new ReceiptReader(record.canonicalBytes)
  reader.prefix(RECEIVE_RECEIPT_PREFIX)
  if (reader.byte() !== RECEIPT_PREPARATION_ADMISSION) return undefined
  reader.identity(16, 'operation ID')
  reader.identity(32, 'receive intent digest')
  reader.optionalDigest('preparation manifest digest')
  reader.optionalDigest('sealed ZIP layout digest')
  return reader.frame()
}

export async function createRawWorkspaceReceipt(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly workspaceBindingDigest: string
  readonly materializedManifestDigest: string
  readonly ownedObjects: readonly OwnedWorkspaceObjectReceipt[]
}): Promise<RawWorkspaceReceiptV1> {
  const identity = receiptIdentity(input)
  const workspaceBindingDigest = digest(input.workspaceBindingDigest, 'workspace binding digest')
  const materializedManifestDigest = digest(
    input.materializedManifestDigest,
    'materialized manifest digest',
  )
  const ownedObjects = snapshotOwnedObjects(input.ownedObjects)
  const uniqueRawBytes = ownedObjects.reduce(
    (total, object) => checkedAdd(total, object.exactBytes),
    0n,
  )
  const canonicalBytes = canonicalRecord('windshare/raw-workspace-receipt/v1', 1, [
    canonicalFrame(canonicalIdentity(identity.operationId, 16, 'operation ID')),
    canonicalFrame(canonicalIdentity(identity.receiveIntentDigest, 32, 'receive intent digest')),
    canonicalFrame(canonicalIdentity(workspaceBindingDigest, 32, 'workspace binding digest')),
    canonicalFrame(canonicalIdentity(materializedManifestDigest, 32, 'manifest digest')),
    canonicalU64(BigInt(ownedObjects.length)),
    ...ownedObjects.map((object) => canonicalFrame(concatCanonicalBytes([
      canonicalFrame(canonicalIdentity(object.ownedObjectId, 32, 'owned object ID')),
      canonicalFrame(canonicalU64(object.exactBytes)),
    ]))),
    canonicalFrame(canonicalU64(uniqueRawBytes)),
  ])
  return Object.freeze({
    schemaVersion: RECEIPT_SCHEMA_VERSION,
    ...identity,
    workspaceBindingDigest,
    materializedManifestDigest,
    ownedObjects,
    uniqueRawBytes,
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

export async function createWorkspaceSealReceipt(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly workspaceBindingDigest: string
  readonly sealedMaterializationDigest: string
  readonly rawWorkspaceReceipt: RawWorkspaceReceiptV1
}): Promise<WorkspaceSealReceiptV1> {
  const identity = receiptIdentity(input)
  const workspaceBindingDigest = digest(input.workspaceBindingDigest, 'workspace binding digest')
  const sealedMaterializationDigest = digest(input.sealedMaterializationDigest, 'seal digest')
  const rawWorkspaceReceipt = input.rawWorkspaceReceipt
  if (rawWorkspaceReceipt.operationId !== identity.operationId ||
      rawWorkspaceReceipt.receiveIntentDigest !== identity.receiveIntentDigest ||
      rawWorkspaceReceipt.workspaceBindingDigest !== workspaceBindingDigest ||
      await canonicalDigest(rawWorkspaceReceipt.canonicalBytes) !== rawWorkspaceReceipt.digest) {
    throw new TypeError('raw workspace receipt escaped its sealed materialization')
  }
  const completed = await completeReceipt(identity, RECEIPT_WORKSPACE_SEAL, [
    canonicalFrame(canonicalIdentity(workspaceBindingDigest, 32, 'workspace binding digest')),
    canonicalFrame(canonicalIdentity(sealedMaterializationDigest, 32, 'seal digest')),
    canonicalFrame(rawWorkspaceReceipt.canonicalBytes),
    canonicalFrame(canonicalIdentity(rawWorkspaceReceipt.digest, 32, 'raw receipt digest')),
  ])
  return Object.freeze({
    ...completed,
    kind: 'workspace-seal',
    workspaceBindingDigest,
    sealedMaterializationDigest,
    rawWorkspaceReceipt,
  })
}

export async function decodeWorkspaceSealReceipt(
  record: PersistedReceiveRecord,
): Promise<WorkspaceSealReceiptV1 | undefined> {
  if (record.kind !== RECEIVE_RECORD_RECEIPT) return undefined
  const reader = new ReceiptReader(record.canonicalBytes)
  reader.prefix(RECEIVE_RECEIPT_PREFIX)
  if (reader.byte() !== RECEIPT_WORKSPACE_SEAL) return undefined
  const operationId = reader.identity(16, 'operation ID')
  const receiveIntentDigest = reader.identity(32, 'receive intent digest')
  const workspaceBindingDigest = reader.identity(32, 'workspace binding digest')
  const sealedMaterializationDigest = reader.identity(32, 'seal digest')
  const rawReceiptBytes = reader.frame()
  const rawReceiptDigest = reader.identity(32, 'raw receipt digest')
  reader.end()
  const rawWorkspaceReceipt = await decodeRawWorkspaceReceipt(rawReceiptBytes)
  const rebuilt = await createWorkspaceSealReceipt({
    operationId,
    receiveIntentDigest,
    workspaceBindingDigest,
    sealedMaterializationDigest,
    rawWorkspaceReceipt,
  })
  if (rawWorkspaceReceipt.digest !== rawReceiptDigest || record.operationId !== operationId ||
      record.digest !== rebuilt.digest || !equalCanonicalBytes(record.canonicalBytes, rebuilt.canonicalBytes)) {
    throw new TypeError('workspace seal receipt changed its canonical authority')
  }
  return rebuilt
}

export async function createZipArtifactVerificationReceipt(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly sealedMaterializationDigest: string
  readonly layoutDigest: string
  readonly packageOwnedObjectId: string
  readonly exactBytes: bigint
  readonly writerCloseVerified: boolean
}): Promise<ZipArtifactVerificationReceiptV1> {
  const identity = receiptIdentity(input)
  if (input.writerCloseVerified !== true) throw new TypeError('package writer close is not verified')
  const sealedMaterializationDigest = digest(input.sealedMaterializationDigest, 'seal digest')
  const layoutDigest = digest(input.layoutDigest, 'layout digest')
  const packageOwnedObjectId = digest(input.packageOwnedObjectId, 'package owned object ID')
  const exactBytes = checkedU64(input.exactBytes, 'package bytes')
  const canonicalBytes = canonicalRecord('windshare/artifact-verification-receipt/v1', 1, [
    canonicalU8(1),
    canonicalFrame(canonicalIdentity(identity.operationId, 16, 'operation ID')),
    canonicalFrame(canonicalIdentity(identity.receiveIntentDigest, 32, 'receive intent digest')),
    canonicalFrame(canonicalIdentity(sealedMaterializationDigest, 32, 'seal digest')),
    canonicalFrame(canonicalIdentity(layoutDigest, 32, 'layout digest')),
    canonicalFrame(canonicalIdentity(packageOwnedObjectId, 32, 'package owned object ID')),
    canonicalFrame(canonicalU64(exactBytes)),
    canonicalFrame(canonicalBoolean(true)),
  ])
  return Object.freeze({
    schemaVersion: RECEIPT_SCHEMA_VERSION,
    kind: 'zip-writer',
    ...identity,
    sealedMaterializationDigest,
    layoutDigest,
    packageOwnedObjectId,
    exactBytes,
    writerCloseVerified: true,
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

export async function createOriginalFileArtifactVerificationReceipt(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly sealedMaterializationDigest: string
  readonly artifactSpecDigest: string
  readonly packageOwnedObjectId: string
  readonly exactBytes: bigint
  readonly finalCheckpointDigest: string
  readonly finalCheckpointGeneration: bigint
  readonly promotionVerified: boolean
}): Promise<OriginalFileArtifactVerificationReceiptV1> {
  const identity = receiptIdentity(input)
  if (input.promotionVerified !== true) throw new TypeError('original file promotion is not verified')
  const sealedMaterializationDigest = digest(input.sealedMaterializationDigest, 'seal digest')
  const layoutDigest = digest(input.artifactSpecDigest, 'artifact digest')
  const packageOwnedObjectId = digest(input.packageOwnedObjectId, 'package owned object ID')
  const exactBytes = checkedU64(input.exactBytes, 'package bytes')
  const finalCheckpointDigest = digest(input.finalCheckpointDigest, 'checkpoint digest')
  const finalCheckpointGeneration = checkedU64(
    input.finalCheckpointGeneration,
    'checkpoint generation',
  )
  if (finalCheckpointGeneration === 0n) throw new TypeError('checkpoint generation must be non-zero')
  const canonicalBytes = canonicalRecord('windshare/artifact-verification-receipt/v1', 1, [
    canonicalU8(2),
    canonicalFrame(canonicalIdentity(identity.operationId, 16, 'operation ID')),
    canonicalFrame(canonicalIdentity(identity.receiveIntentDigest, 32, 'receive intent digest')),
    canonicalFrame(canonicalIdentity(sealedMaterializationDigest, 32, 'seal digest')),
    canonicalFrame(canonicalIdentity(layoutDigest, 32, 'artifact digest')),
    canonicalFrame(canonicalIdentity(packageOwnedObjectId, 32, 'package owned object ID')),
    canonicalFrame(canonicalU64(exactBytes)),
    canonicalFrame(canonicalIdentity(finalCheckpointDigest, 32, 'checkpoint digest')),
    canonicalFrame(canonicalU64(finalCheckpointGeneration)),
    canonicalFrame(canonicalBoolean(true)),
  ])
  return Object.freeze({
    schemaVersion: RECEIPT_SCHEMA_VERSION,
    kind: 'original-file-promotion',
    ...identity,
    sealedMaterializationDigest,
    layoutDigest,
    packageOwnedObjectId,
    exactBytes,
    finalCheckpointDigest,
    finalCheckpointGeneration,
    promotionVerified: true,
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

export async function validateArtifactVerificationReceipt(
  candidate: ArtifactVerificationReceiptV1,
): Promise<ArtifactVerificationReceiptV1> {
  if (candidate.kind !== 'zip-writer' && candidate.kind !== 'original-file-promotion') {
    throw new TypeError('artifact verification receipt kind is invalid')
  }
  const rebuilt = candidate.kind === 'zip-writer'
    ? await createZipArtifactVerificationReceipt(candidate)
    : await createOriginalFileArtifactVerificationReceipt({
        ...candidate,
        artifactSpecDigest: candidate.layoutDigest,
      })
  if (candidate.digest !== rebuilt.digest ||
      !equalCanonicalBytes(candidate.canonicalBytes, rebuilt.canonicalBytes)) {
    throw new TypeError('artifact verification receipt is not canonical')
  }
  return rebuilt
}

export async function createPackageReceipt(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly packagedArtifactDigest: string
  readonly artifactVerification: ArtifactVerificationReceiptV1
}): Promise<PackageReceiptV1> {
  const identity = receiptIdentity(input)
  const packagedArtifactDigest = digest(input.packagedArtifactDigest, 'package digest')
  const artifactVerification = await validateArtifactVerificationReceipt(input.artifactVerification)
  if (artifactVerification.operationId !== identity.operationId ||
      artifactVerification.receiveIntentDigest !== identity.receiveIntentDigest ||
      await canonicalDigest(artifactVerification.canonicalBytes) !== artifactVerification.digest) {
    throw new TypeError('artifact verification escaped its package receipt')
  }
  const completed = await completeReceipt(identity, RECEIPT_PACKAGE, [
    canonicalFrame(canonicalIdentity(packagedArtifactDigest, 32, 'package digest')),
    canonicalFrame(artifactVerification.canonicalBytes),
    canonicalFrame(canonicalIdentity(artifactVerification.digest, 32, 'artifact receipt digest')),
  ])
  return Object.freeze({
    ...completed,
    kind: 'package',
    packagedArtifactDigest,
    artifactVerification,
  })
}

export async function createManagedPublicationReceipt(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly publicationAttemptId: string
  readonly packagedArtifactDigest: string
  readonly reservationDigest: string
  readonly targetAuthorityRef: string
  readonly exactBytes: bigint
  readonly commitConfirmed: boolean
}): Promise<ManagedPublicationReceiptV1> {
  const identity = receiptIdentity(input)
  if (input.commitConfirmed !== true) throw new TypeError('managed publication commit is not confirmed')
  const publicationAttemptId = snapshotIdentity(
    input.publicationAttemptId,
    16,
    'publication attempt ID',
  )
  const packagedArtifactDigest = digest(input.packagedArtifactDigest, 'package digest')
  const reservationDigest = digest(input.reservationDigest, 'reservation digest')
  const targetAuthorityRef = digest(input.targetAuthorityRef, 'target authority reference')
  const exactBytes = checkedU64(input.exactBytes, 'published bytes')
  const completed = await completeReceipt(identity, RECEIPT_MANAGED_PUBLICATION, [
    canonicalFrame(canonicalIdentity(publicationAttemptId, 16, 'publication attempt ID')),
    canonicalFrame(canonicalIdentity(packagedArtifactDigest, 32, 'package digest')),
    canonicalFrame(canonicalIdentity(reservationDigest, 32, 'reservation digest')),
    canonicalFrame(canonicalIdentity(targetAuthorityRef, 32, 'target authority reference')),
    canonicalFrame(canonicalU64(exactBytes)),
    canonicalFrame(canonicalBoolean(true)),
  ])
  return Object.freeze({
    ...completed,
    kind: 'managed-publication',
    publicationAttemptId,
    packagedArtifactDigest,
    reservationDigest,
    targetAuthorityRef,
    exactBytes,
    commitConfirmed: true,
  })
}

export async function createHandoffReceipt(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly publicationAttemptId: string
  readonly packagedArtifactDigest: string
  readonly suggestedName: string
  readonly urlLeaseEndsAt: number
  readonly handoffStarted: boolean
}): Promise<HandoffReceiptV1> {
  const identity = receiptIdentity(input)
  if (input.handoffStarted !== true) throw new TypeError('browser handoff was not started')
  const publicationAttemptId = snapshotIdentity(
    input.publicationAttemptId,
    16,
    'publication attempt ID',
  )
  const packagedArtifactDigest = digest(input.packagedArtifactDigest, 'package digest')
  const suggestedName = new TextDecoder(undefined, { fatal: true }).decode(
    canonicalText(input.suggestedName),
  )
  const urlLeaseEndsAt = unixMilliseconds(input.urlLeaseEndsAt, 'URL lease end')
  const completed = await completeReceipt(identity, RECEIPT_HANDOFF, [
    canonicalFrame(canonicalIdentity(publicationAttemptId, 16, 'publication attempt ID')),
    canonicalFrame(canonicalIdentity(packagedArtifactDigest, 32, 'package digest')),
    canonicalFrame(canonicalText(suggestedName)),
    canonicalFrame(canonicalU64(BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS)),
    canonicalFrame(canonicalUnixMilliseconds(urlLeaseEndsAt)),
    canonicalFrame(canonicalBoolean(true)),
  ])
  return Object.freeze({
    ...completed,
    kind: 'handoff',
    publicationAttemptId,
    packagedArtifactDigest,
    suggestedName,
    urlLeaseEndsAt,
    handoffStarted: true,
  })
}

export async function createExpiryReceipt(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly priorStableState: ExpiryReceiptV1['priorStableState']
  readonly expiresAt: number
  readonly retainedSuccessCount?: bigint
  readonly cleanupState: 'clean' | 'cleanup-pending'
}): Promise<ExpiryReceiptV1> {
  const identity = receiptIdentity(input)
  const expiresAt = unixMilliseconds(input.expiresAt, 'expiry deadline')
  const retainedSuccessCount = checkedU64(
    input.retainedSuccessCount ?? 0n,
    'retained success count',
  )
  const cleanupState = input.cleanupState
  if (cleanupState !== 'clean' && cleanupState !== 'cleanup-pending') {
    throw new TypeError('expiry cleanup state is invalid')
  }
  const completed = await completeReceipt(identity, RECEIPT_EXPIRY, [
    canonicalFrame(canonicalU8(stableStateByte(input.priorStableState))),
    canonicalFrame(canonicalUnixMilliseconds(expiresAt)),
    canonicalFrame(canonicalU64(retainedSuccessCount)),
    canonicalFrame(canonicalU8(cleanupState === 'clean' ? 1 : 2)),
  ])
  return Object.freeze({
    ...completed,
    kind: 'expiry',
    priorStableState: input.priorStableState,
    expiresAt,
    retainedSuccessCount,
    cleanupState,
  })
}

export async function createCleanupReceipt(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly removedObjectIds: readonly string[]
  readonly removedRecordDigests: readonly string[]
  readonly cleanupGeneration: bigint
}): Promise<CleanupReceiptV1> {
  const identity = receiptIdentity(input)
  const removedObjectIds = snapshotSortedDigests(input.removedObjectIds, 'removed object ID')
  const removedRecordDigests = snapshotSortedDigests(
    input.removedRecordDigests,
    'removed record digest',
  )
  const cleanupGeneration = checkedU64(input.cleanupGeneration, 'cleanup generation')
  if (cleanupGeneration === 0n) throw new TypeError('cleanup generation must be non-zero')
  const completed = await completeReceipt(identity, RECEIPT_CLEANUP, [
    canonicalU64(BigInt(removedObjectIds.length)),
    ...removedObjectIds.map((value) => canonicalFrame(canonicalIdentity(
      value,
      32,
      'removed object ID',
    ))),
    canonicalU64(BigInt(removedRecordDigests.length)),
    ...removedRecordDigests.map((value) => canonicalFrame(canonicalIdentity(
      value,
      32,
      'removed record digest',
    ))),
    canonicalFrame(canonicalU64(cleanupGeneration)),
  ])
  return Object.freeze({
    ...completed,
    kind: 'cleanup',
    removedObjectIds,
    removedRecordDigests,
    cleanupGeneration,
  })
}

export async function createPackageTemporaryCleanupReceipt(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly sealedMaterializationDigest: string
  readonly packageOwnedObjectId: string
  readonly packageHandleId: string
  readonly cleanupResult: 'removed' | 'already-absent'
  readonly cleanupProofDigest: string
}): Promise<PackageTemporaryCleanupReceiptV1> {
  const identity = receiptIdentity(input)
  const sealedMaterializationDigest = digest(input.sealedMaterializationDigest, 'seal digest')
  const packageOwnedObjectId = digest(input.packageOwnedObjectId, 'package owned object ID')
  const packageHandleId = canonicalTextValue(input.packageHandleId, 'package handle ID')
  const cleanupResult = input.cleanupResult
  if (cleanupResult !== 'removed' && cleanupResult !== 'already-absent') {
    throw new TypeError('temporary package cleanup result is invalid')
  }
  const cleanupProofDigest = digest(input.cleanupProofDigest, 'temporary cleanup proof digest')
  const completed = await completeReceipt(identity, RECEIPT_PACKAGE_TEMPORARY_CLEANUP, [
    canonicalFrame(canonicalIdentity(sealedMaterializationDigest, 32, 'seal digest')),
    canonicalFrame(canonicalIdentity(packageOwnedObjectId, 32, 'package owned object ID')),
    canonicalFrame(canonicalText(packageHandleId)),
    canonicalFrame(canonicalU8(cleanupResult === 'removed' ? 1 : 2)),
    canonicalFrame(canonicalIdentity(cleanupProofDigest, 32, 'temporary cleanup proof digest')),
  ])
  return Object.freeze({
    ...completed,
    kind: 'package-temporary-cleanup',
    sealedMaterializationDigest,
    packageOwnedObjectId,
    packageHandleId,
    cleanupResult,
    cleanupProofDigest,
  })
}

export async function decodePackageTemporaryCleanupReceipt(
  record: PersistedReceiveRecord,
): Promise<PackageTemporaryCleanupReceiptV1 | undefined> {
  if (record.kind !== RECEIVE_RECORD_RECEIPT) return undefined
  const reader = new ReceiptReader(record.canonicalBytes)
  reader.prefix(RECEIVE_RECEIPT_PREFIX)
  if (reader.byte() !== RECEIPT_PACKAGE_TEMPORARY_CLEANUP) return undefined
  const operationId = reader.identity(16, 'operation ID')
  const receiveIntentDigest = reader.identity(32, 'receive intent digest')
  const sealedMaterializationDigest = reader.identity(32, 'seal digest')
  const packageOwnedObjectId = reader.identity(32, 'package owned object ID')
  const packageHandleId = reader.text('package handle ID')
  const resultByte = reader.u8('temporary package cleanup result')
  const cleanupProofDigest = reader.identity(32, 'temporary cleanup proof digest')
  reader.end()
  let cleanupResult: PackageTemporaryCleanupReceiptV1['cleanupResult'] | undefined
  if (resultByte === 1) cleanupResult = 'removed'
  if (resultByte === 2) cleanupResult = 'already-absent'
  if (cleanupResult === undefined) throw new TypeError('temporary package cleanup result is invalid')
  const rebuilt = await createPackageTemporaryCleanupReceipt({
    operationId,
    receiveIntentDigest,
    sealedMaterializationDigest,
    packageOwnedObjectId,
    packageHandleId,
    cleanupResult,
    cleanupProofDigest,
  })
  if (record.operationId !== operationId || record.digest !== rebuilt.digest ||
      !equalCanonicalBytes(record.canonicalBytes, rebuilt.canonicalBytes)) {
    throw new TypeError('temporary package cleanup receipt changed its canonical authority')
  }
  return rebuilt
}

export function persistedReceiptRecord(
  receipt: WorkspaceReceiveReceiptV1,
): Promise<PersistedReceiveRecord> {
  return createPersistedReceiveRecord({
    operationId: receipt.operationId,
    kind: receipt.kind === 'cleanup' ? RECEIVE_RECORD_CLEANUP : RECEIVE_RECORD_RECEIPT,
    canonicalBytes: receipt.canonicalBytes,
  })
}

function receiptIdentity(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
}): { readonly operationId: string; readonly receiveIntentDigest: string } {
  return Object.freeze({
    operationId: snapshotIdentity(input.operationId, 16, 'operation ID'),
    receiveIntentDigest: digest(input.receiveIntentDigest, 'receive intent digest'),
  })
}

async function completeReceipt(
  identity: { readonly operationId: string; readonly receiveIntentDigest: string },
  discriminant: number,
  variantFields: readonly CanonicalBytes[],
): Promise<ReceiveReceiptBase> {
  const canonicalBytes = canonicalRecord('windshare/receive-receipt/v1', 1, [
    canonicalU8(discriminant),
    canonicalFrame(canonicalIdentity(identity.operationId, 16, 'operation ID')),
    canonicalFrame(canonicalIdentity(identity.receiveIntentDigest, 32, 'receive intent digest')),
    ...variantFields,
  ])
  return Object.freeze({
    schemaVersion: RECEIPT_SCHEMA_VERSION,
    ...identity,
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

class ReceiptReader {
  readonly #bytes: Uint8Array
  #offset = 0

  constructor(bytes: Uint8Array) {
    this.#bytes = snapshotCanonicalBytes(bytes)
  }

  prefix(expected: Uint8Array): void {
    const actual = this.#take(expected.byteLength)
    if (!equalCanonicalBytes(actual, expected)) {
      throw new TypeError('receive receipt domain or version is invalid')
    }
  }

  byte(): number {
    return this.#take(1)[0]!
  }

  frame(): CanonicalBytes {
    const size = this.#u64Raw('canonical frame length')
    if (size > BigInt(Number.MAX_SAFE_INTEGER)) {
      throw new TypeError('canonical receipt frame exceeds the runtime bound')
    }
    return snapshotCanonicalBytes(this.#take(Number(size)))
  }

  identity(width: number, label: string): string {
    const bytes = this.frame()
    if (bytes.byteLength !== width) throw new TypeError(`${label} width is invalid`)
    return snapshotIdentity(encodeBase64Url(bytes), width, label)
  }

  optionalDigest(label: string): string | undefined {
    const optional = new ReceiptReader(this.frame())
    const discriminant = optional.byte()
    if (discriminant === 2) {
      optional.end()
      return undefined
    }
    if (discriminant !== 1) throw new TypeError(`${label} optional discriminant is invalid`)
    const value = optional.identity(32, label)
    optional.end()
    return value
  }

  u64(label: string): bigint {
    const bytes = this.frame()
    if (bytes.byteLength !== 8) throw new TypeError(`${label} width is invalid`)
    return new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getBigUint64(0, false)
  }

  u8(label: string): number {
    const bytes = this.frame()
    if (bytes.byteLength !== 1) throw new TypeError(`${label} width is invalid`)
    return bytes[0]!
  }

  text(label: string): string {
    const canonical = this.frame()
    const value = new TextDecoder(undefined, { fatal: true }).decode(canonical)
    if (!equalCanonicalBytes(canonicalText(value), canonical)) {
      throw new TypeError(`${label} is not canonical text`)
    }
    return value
  }

  end(): void {
    if (this.#offset !== this.#bytes.byteLength) {
      throw new TypeError('receive receipt has trailing canonical bytes')
    }
  }

  #u64Raw(label: string): bigint {
    const bytes = this.#take(8)
    if (bytes.byteLength !== 8) throw new TypeError(`${label} is truncated`)
    return new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getBigUint64(0, false)
  }

  #take(length: number): Uint8Array {
    if (!Number.isSafeInteger(length) || length < 0 ||
        this.#offset > this.#bytes.byteLength - length) {
      throw new TypeError('receive receipt canonical bytes are truncated')
    }
    const result = this.#bytes.subarray(this.#offset, this.#offset + length)
    this.#offset += length
    return result
  }
}

async function decodeRawWorkspaceReceipt(
  canonicalBytes: Uint8Array,
): Promise<RawWorkspaceReceiptV1> {
  const reader = CanonicalRecordReader.open(canonicalBytes, RAW_WORKSPACE_RECEIPT_DOMAIN, 1)
  const operationId = reader.framedIdentity(16, 'operation ID')
  const receiveIntentDigest = reader.framedIdentity(32, 'receive intent digest')
  const workspaceBindingDigest = reader.framedIdentity(32, 'workspace binding digest')
  const materializedManifestDigest = reader.framedIdentity(32, 'manifest digest')
  const objectCount = reader.u64('owned object count')
  if (objectCount === 0n || objectCount > 1_000_000n) {
    throw new TypeError('raw workspace receipt object count is invalid')
  }
  const ownedObjects: OwnedWorkspaceObjectReceipt[] = []
  for (let index = 0n; index < objectCount; index += 1n) {
    const object = CanonicalRecordReader.value(reader.frame('owned workspace object'))
    ownedObjects.push(Object.freeze({
      ownedObjectId: object.framedIdentity(32, 'owned object ID'),
      exactBytes: object.framedU64('owned object bytes'),
    }))
    object.finish('owned workspace object')
  }
  const uniqueRawBytes = reader.framedU64('unique raw bytes')
  reader.finish('raw workspace receipt')
  const rebuilt = await createRawWorkspaceReceipt({
    operationId,
    receiveIntentDigest,
    workspaceBindingDigest,
    materializedManifestDigest,
    ownedObjects,
  })
  if (rebuilt.uniqueRawBytes !== uniqueRawBytes ||
      !equalCanonicalBytes(rebuilt.canonicalBytes, canonicalBytes)) {
    throw new TypeError('raw workspace receipt changed its canonical authority')
  }
  return rebuilt
}

function snapshotOwnedObjects(
  input: readonly OwnedWorkspaceObjectReceipt[],
): readonly OwnedWorkspaceObjectReceipt[] {
  const values = input.map((object) => Object.freeze({
    ownedObjectId: digest(object.ownedObjectId, 'owned object ID'),
    exactBytes: checkedU64(object.exactBytes, 'owned object bytes'),
  })).sort((left, right) => compareCanonicalIdentity(left.ownedObjectId, right.ownedObjectId))
  if (values.length === 0 || values.some((value, index) =>
    index > 0 && value.ownedObjectId === values[index - 1]?.ownedObjectId)) {
    throw new TypeError('workspace seal object inventory is empty or duplicated')
  }
  return Object.freeze(values)
}

function snapshotSortedDigests(input: readonly string[], label: string): readonly string[] {
  const values = input.map((value) => digest(value, label)).sort()
  if (values.some((value, index) => index > 0 && value === values[index - 1])) {
    throw new TypeError(`${label} inventory contains duplicates`)
  }
  return Object.freeze(values)
}

function snapshotAdmissionLimits(input: {
  readonly jobLimitBytes: bigint
  readonly processLimitBytes: bigint
  readonly estimatedQuotaBytes: bigint
  readonly currentUsageBytes: bigint
  readonly minimumReserveBytes: bigint
  readonly incrementalPhysicalPeakBytes: bigint
}): Pick<
  PreparationAdmissionReceiptV1,
  | 'jobLimitBytes'
  | 'processLimitBytes'
  | 'estimatedQuotaBytes'
  | 'currentUsageBytes'
  | 'minimumReserveBytes'
  | 'incrementalPhysicalPeakBytes'
> {
  return Object.freeze({
    jobLimitBytes: checkedU64(input.jobLimitBytes, 'job workspace limit'),
    processLimitBytes: checkedU64(input.processLimitBytes, 'process workspace limit'),
    estimatedQuotaBytes: checkedU64(input.estimatedQuotaBytes, 'estimated quota'),
    currentUsageBytes: checkedU64(input.currentUsageBytes, 'current quota usage'),
    minimumReserveBytes: checkedU64(input.minimumReserveBytes, 'quota reserve'),
    incrementalPhysicalPeakBytes: checkedU64(
      input.incrementalPhysicalPeakBytes,
      'incremental physical peak',
    ),
  })
}

function canonicalOptionalDigest(value: string | undefined): CanonicalBytes {
  return value === undefined
    ? canonicalU8(2)
    : concatCanonicalBytes([
        canonicalU8(1),
        canonicalFrame(canonicalIdentity(value, 32, 'optional digest')),
      ])
}

function optionalDigest(value: string | undefined, label: string): string | undefined {
  return value === undefined ? undefined : digest(value, label)
}

function canonicalTextValue(value: string, label: string): string {
  if (typeof value !== 'string' || value.length === 0) throw new TypeError(`${label} is empty`)
  const canonical = canonicalText(value)
  const decoded = new TextDecoder(undefined, { fatal: true }).decode(canonical)
  if (decoded !== value) throw new TypeError(`${label} is not canonical text`)
  return decoded
}

function digest(value: string, label: string): string {
  return snapshotIdentity(value, 32, label)
}

function unixMilliseconds(value: number, label: string): number {
  try {
    canonicalUnixMilliseconds(value)
  } catch (error) {
    throw new TypeError(`${label} is invalid`, { cause: error })
  }
  return value
}

function compareCanonicalIdentity(left: string, right: string): number {
  const leftBytes = canonicalIdentity(left, 32, 'sortable identity')
  const rightBytes = canonicalIdentity(right, 32, 'sortable identity')
  for (let index = 0; index < leftBytes.length; index += 1) {
    const difference = (leftBytes[index] ?? 0) - (rightBytes[index] ?? 0)
    if (difference !== 0) return difference
  }
  return 0
}

function checkedAdd(left: bigint, right: bigint): bigint {
  const value = left + right
  if (value > U64_MAXIMUM) throw new RangeError('receipt byte arithmetic overflow')
  return value
}

function checkedU64(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n || value > U64_MAXIMUM) {
    throw new TypeError(`${label} is not a u64`)
  }
  return value
}

function stableStateByte(state: ExpiryReceiptV1['priorStableState']): number {
  switch (state) {
    case 'resumable-receive': return 1
    case 'resumable-package': return 2
    case 'waiting-to-save': return 3
    case 'download-started': return 4
  }
}

export type WorkspaceStableState = Extract<
  ReceiveLifecycleState,
  { kind: 'resumable-receive' | 'resumable-package' | 'waiting-to-save' | 'download-started' }
>
