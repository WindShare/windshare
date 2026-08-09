import { BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS } from '../../transfer/intent'
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
  snapshotIdentity,
} from './canonical'
import {
  createPersistedReceiveRecord,
  RECEIVE_RECORD_CLEANUP,
  RECEIVE_RECORD_RECEIPT,
  type PersistedReceiveRecord,
} from './records'
import { CanonicalRecordReader } from './canonical-reader'
import {
  RECEIVE_RECEIPT_PREFIX,
  ReceiptReader,
  canonicalTextValue,
  checkedAdd,
  checkedU64,
  completeReceipt,
  digest,
  receiptIdentity,
  snapshotOwnedObjects,
  snapshotSortedDigests,
  stableStateByte,
  unixMilliseconds,
} from './receipts/codec'
import {
  RAW_WORKSPACE_RECEIPT_DOMAIN,
  RECEIPT_CLEANUP,
  RECEIPT_EXPIRY,
  RECEIPT_HANDOFF,
  RECEIPT_MANAGED_PUBLICATION,
  RECEIPT_PACKAGE,
  RECEIPT_PACKAGE_TEMPORARY_CLEANUP,
  RECEIPT_SCHEMA_VERSION,
  RECEIPT_WORKSPACE_SEAL,
  type ArtifactVerificationReceiptV1,
  type CleanupReceiptV1,
  type ExpiryReceiptV1,
  type HandoffReceiptV1,
  type ManagedPublicationReceiptV1,
  type OriginalFileArtifactVerificationReceiptV1,
  type OwnedWorkspaceObjectReceipt,
  type PackageReceiptV1,
  type PackageTemporaryCleanupReceiptV1,
  type RawWorkspaceReceiptV1,
  type WorkspaceReceiveReceiptV1,
  type WorkspaceSealReceiptV1,
  type ZipArtifactVerificationReceiptV1,
} from './receipts/model'

export {
  createPreparationAdmissionReceipt,
  decodePreparationAdmissionAuthority,
  decodePreparationAdmissionReceipt,
} from './receipts/admission'
export type {
  ArtifactVerificationReceiptV1,
  CleanupReceiptV1,
  ExpiryReceiptV1,
  HandoffReceiptV1,
  ManagedPublicationReceiptV1,
  OriginalFileArtifactVerificationReceiptV1,
  OwnedWorkspaceObjectReceipt,
  PackageReceiptV1,
  PackageTemporaryCleanupReceiptV1,
  PreparationAdmissionReceiptV1,
  RawWorkspaceReceiptV1,
  WorkspaceReceiveReceiptV1,
  WorkspaceSealReceiptV1,
  WorkspaceStableState,
  ZipArtifactVerificationReceiptV1,
} from './receipts/model'

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
