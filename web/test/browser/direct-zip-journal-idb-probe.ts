import { decodeBase64Url, encodeBase64Url } from '../../src/crypto/bytes'
import { chainDirectZipEpochDigestV1 } from '../../src/output/direct-zip/format'
import {
  CHECKPOINT_DATABASE_VERSION,
  INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
  INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
  INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
  INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE,
  INDEXEDDB_DIRECT_ZIP_EPOCH_PAGE_STORE,
  INDEXEDDB_DIRECT_ZIP_LAYOUT_PAGE_STORE,
  INDEXEDDB_DIRECT_ZIP_STATE_STORE,
  INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
  INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
  INDEXEDDB_RECEIVE_HANDLE_STORE,
  INDEXEDDB_RECEIVE_LEASE_STORE,
  INDEXEDDB_RECEIVE_RECORD_STORE,
  installIndexedDbV8Schema,
  openIndexedDbCheckpointDatabase,
  transactionCompletion,
} from '../../src/output/browser/indexeddb-database'
import { IndexedDbReceiveResumeSource } from '../../src/output/browser/indexeddb-resume-state'
import { directZipJournalBudgetDigestV1 } from '../../src/output/direct-zip/journal/budget'
import { IndexedDbDirectZipJournalRepository } from '../../src/output/direct-zip/journal/indexeddb'
import { createDirectZipRecoveryGateV1 } from '../../src/output/direct-zip/journal/recovery-gate'
import { createDirectZipBootstrapCandidateV1 } from '../../src/output/direct-zip/journal/records'
import {
  createDirectZipCheckpointV1,
  createDirectZipCheckpointProposalV1,
  createDirectZipCommitCandidateV1,
  createDirectZipImmutablePageV1,
  createDirectZipTargetObservationV1,
} from '../../src/output/direct-zip/journal/records'
import {
  createReceiveOperationV2,
  storedReceiveOperationRecord,
  receiveOperationHandleRecord,
  receiveOperationLeaseRecord,
} from '../../src/output/workspace/records'
import {
  initialReceiveLifecycleState,
  nextReceiveLifecycleState,
} from '../../src/output/workspace/state'
import { storedReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import {
  createDirectorySelectionResultRoot,
  createDirectResumableZipPlan,
  createFSAOwnedFileBinding,
  createReceiveIntent,
  createSelectionSpec,
  createZipArchiveArtifact,
  deriveArtifactChoiceIdentity,
} from '../../src/transfer/intent'

const LEGACY_RECEIVE_STORES = [
  'receive-operation-v1-records',
  'receive-operation-v1-manifest-pages',
  'receive-operation-v1-handles',
  'receive-operation-v1-leases',
] as const

export async function probeDirectZipBootstrapAtomicity(databaseName: string) {
  await deleteDatabase(databaseName)
  const repository = await IndexedDbDirectZipJournalRepository.open({ databaseName })
  const raw = await openRawDatabase(databaseName, CHECKPOINT_DATABASE_VERSION)
  try {
    const operationId = identity(16, 1)
    const leaseId = identity(16, 2)
    const parentHandle = receiveOperationHandleRecord({
      id: 'probe-parent-handle',
      operationId,
      kind: 1,
      authorityRef: identity(32, 3),
      handle: { probe: 'preexisting' },
    })
    const lease = receiveOperationLeaseRecord({
      operationId,
      leaseId,
      acquiredAt: 1,
    })
    const choiceId = identity(32, 4) as import('../../src/transfer/intent').ArtifactChoiceID
    const candidate = await createDirectZipBootstrapCandidateV1({
      operationId,
      candidateId: identity(16, 5),
      leaseId,
      leaseGeneration: 1n,
      selectionCanonicalBytes: Uint8Array.of(1),
      artifactCanonicalBytes: Uint8Array.of(2),
      choiceIdentityCanonicalBytes: Uint8Array.of(3),
      choiceId,
      preClickRanking: [choiceId],
      stablePhysicalName: 'probe.zip',
      ownershipNonce: identity(32, 6),
      targetBindingDigest: identity(32, 7),
      policies: {
        encodingPolicyDigest: identity(32, 8),
        layoutPolicyDigest: identity(32, 9),
        checkpointPolicyDigest: identity(32, 10),
        journalBudgetDigest: await directZipJournalBudgetDigestV1(),
        epochPolicyDigest: identity(32, 11),
    },
      parentHandleId: parentHandle.id,
    })

    await put(raw, INDEXEDDB_RECEIVE_HANDLE_STORE, parentHandle)
    let failureName = 'none'
    try {
      await repository.createBootstrapCandidate({
        candidate,
        provisionalParentHandle: parentHandle,
        lease,
      })
    } catch (error) {
      failureName = error instanceof DOMException ? error.name : 'Error'
    }
    const rolledBackCandidateCount = await count(raw, INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE)
    const rolledBackLeaseCount = await count(raw, INDEXEDDB_RECEIVE_LEASE_STORE)
    await remove(raw, INDEXEDDB_RECEIVE_HANDLE_STORE, parentHandle.id)

    await repository.createBootstrapCandidate({
      candidate,
      provisionalParentHandle: parentHandle,
      lease,
    })
    const replacementLease = receiveOperationLeaseRecord({
      operationId,
      leaseId: identity(16, 12),
      acquiredAt: 2,
    })
    const recoveredCandidate = await createDirectZipBootstrapCandidateV1({
      ...candidate,
      leaseId: replacementLease.leaseId,
      leaseGeneration: 2n,
    })
    await repository.replaceBootstrapLease({
      expectedCandidate: candidate,
      candidate: recoveredCandidate,
      lease: replacementLease,
    })
    let staleLeaseFailure = 'none'
    try {
      await repository.replaceBootstrapLease({
        expectedCandidate: candidate,
        candidate: recoveredCandidate,
        lease: replacementLease,
      })
    } catch (error) {
      staleLeaseFailure = error instanceof DOMException ? error.name : 'Error'
    }
    const restoredCandidate = await repository.readOperationCandidate(operationId)
    const source = await IndexedDbReceiveResumeSource.open(databaseName)
    const startup = await source.listDirectZipBootstrapCandidates()
    source.close()
    return {
      failureName,
      rolledBackCandidateCount,
      rolledBackLeaseCount,
      candidateDigest: restoredCandidate?.digest,
      leaseGeneration: restoredCandidate?.kind === 'bootstrap'
        ? restoredCandidate.leaseGeneration.toString(10)
        : undefined,
      staleLeaseFailure,
      startupCandidateDigests: startup.map(value => value.candidateDigest),
    }
  } finally {
    raw.close()
    repository.close()
    await deleteDatabase(databaseName)
  }
}

export async function probeIndexedDbV9Migration(databaseName: string) {
  await deleteDatabase(databaseName)
  const legacy = await openRawDatabase(databaseName, 8, (database, transaction) => {
    installIndexedDbV8Schema(database, transaction, 0)
  })
  for (const name of LEGACY_RECEIVE_STORES) {
    await put(legacy, name, { id: `${name}/sentinel`, operationId: 'legacy' })
  }
  await put(legacy, INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE, { id: 'file-candidate' })
  await put(legacy, INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, { id: 'file-committed' })
  await put(legacy, INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE, { operationId: 'compatible' })
  await put(legacy, INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE, { id: 'mapping' })
  legacy.close()

  const migrated = await openIndexedDbCheckpointDatabase(databaseName)
  try {
    return {
      version: migrated.version,
      legacyStoresPresent: LEGACY_RECEIVE_STORES.map(name => migrated.objectStoreNames.contains(name)),
      currentReceiveCounts: await Promise.all([
        INDEXEDDB_RECEIVE_RECORD_STORE,
        INDEXEDDB_RECEIVE_HANDLE_STORE,
        INDEXEDDB_RECEIVE_LEASE_STORE,
      ].map(name => count(migrated, name))),
      invalidatedAuthorityCounts: await Promise.all([
        INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
        INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
        INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
        INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
      ].map(name => count(migrated, name))),
      directZipStoresPresent: [
        INDEXEDDB_DIRECT_ZIP_STATE_STORE,
        INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
        INDEXEDDB_DIRECT_ZIP_LAYOUT_PAGE_STORE,
        INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE,
        INDEXEDDB_DIRECT_ZIP_EPOCH_PAGE_STORE,
      ].map(name => migrated.objectStoreNames.contains(name)),
    }
  } finally {
    migrated.close()
    await deleteDatabase(databaseName)
  }
}

export async function probeDirectZipJournalPromotion(databaseName: string) {
  await deleteDatabase(databaseName)
  const repository = await IndexedDbDirectZipJournalRepository.open({ databaseName })
  try {
    const operationId = identity(16, 21)
    let leaseId = identity(16, 22)
    const journalBudget = await directZipJournalBudgetDigestV1()
    const selection = await createSelectionSpec({
      shareInstance: identity(16, 23),
      syntheticRoot: identity(16, 24),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    })
    const artifact = await createZipArchiveArtifact(
      createDirectorySelectionResultRoot(identity(16, 25), 'probe'),
    )
    const stableName = `probe.windshare-${identity(16, 26)}.zip`
    const binding = await createFSAOwnedFileBinding({
      operationId,
      artifact,
      stableName,
      targetRef: identity(32, 27),
      policies: {
        zipEncoding: identity(32, 28),
        layout: identity(32, 29),
        checkpoint: identity(32, 30),
        journalBudget,
        epoch: identity(32, 31),
      },
    })
    const plan = await createDirectResumableZipPlan(artifact, binding)
    const intent = await createReceiveIntent({ selection, artifact, plan })
    const choice = await deriveArtifactChoiceIdentity(artifact, plan)
    const operation = await createReceiveOperationV2({
      receiveIntent: intent,
      preClickRanking: [choice.id],
    })
    const policies = {
      encodingPolicyDigest: binding.policies.zipEncoding,
      layoutPolicyDigest: binding.policies.layout,
      checkpointPolicyDigest: binding.policies.checkpoint,
      journalBudgetDigest: binding.policies.journalBudget,
      epochPolicyDigest: binding.policies.epoch,
    }
    const parentBindingDigest = identity(32, 32)
    const fileBindingDigest = identity(32, 33)
    const zeroDigest = identity(32, 0)
    const observation = await createDirectZipTargetObservationV1({
      operationId,
      parentBindingDigest,
      fileBindingDigest,
      ownershipMarkerDigest: identity(32, 34),
      exactLength: 0n,
      lastModifiedMilliseconds: 1,
      epochRootDigest: zeroDigest,
    })
    const emptyChain = (fill: number) => ({
      chainId: identity(16, fill),
      rootDigest: zeroDigest,
      pageCount: 0n,
      recordCount: 0n,
      canonicalMetadataBytes: 0n,
    })
    const checkpoint = await createDirectZipCheckpointV1({
      operationId,
      receiveIntentDigest: intent.digest,
      targetBindingDigest: binding.digest,
      policies,
      generation: 1n,
      phase: 'between-members',
      entryOrdinal: 0n,
      discovery: {
        cursorCanonicalBytes: Uint8Array.of(1),
        directoryAdmissionDigest: identity(32, 35),
        discoveryRootDigest: identity(32, 36),
      },
      archiveOffset: 0n,
      committedArchiveLength: 0n,
      committedSelectedPayloadBytes: 0n,
      parentBindingDigest,
      fileBindingDigest,
      targetObservation: observation,
      epochRootDigest: zeroDigest,
      layoutPages: emptyChain(37),
      centralPages: emptyChain(38),
      epochPages: emptyChain(39),
      journalUsage: { memberCount: 0n, canonicalMetadataBytes: 0n },
    })
    const parentHandle = receiveOperationHandleRecord({
      id: 'promotion-parent-handle',
      operationId,
      kind: 1,
      authorityRef: parentBindingDigest,
      handle: { probe: 'parent' },
    })
    const fileHandle = receiveOperationHandleRecord({
      id: 'promotion-file-handle',
      operationId,
      kind: 2,
      authorityRef: fileBindingDigest,
      handle: { probe: 'file' },
    })
    const lease = receiveOperationLeaseRecord({ operationId, leaseId, acquiredAt: 1 })
    const bootstrap = await createDirectZipBootstrapCandidateV1({
      operationId,
      candidateId: identity(16, 40),
      leaseId,
      leaseGeneration: 1n,
      selectionCanonicalBytes: selection.canonicalBytes,
      artifactCanonicalBytes: artifact.canonicalBytes,
      choiceIdentityCanonicalBytes: choice.canonicalBytes,
      choiceId: choice.id,
      preClickRanking: [choice.id],
      stablePhysicalName: stableName,
      ownershipNonce: identity(32, 41),
      targetBindingDigest: binding.digest,
      policies,
      parentHandleId: parentHandle.id,
    })
    const receiving = nextReceiveLifecycleState(initialReceiveLifecycleState({
      operationId,
      receiveIntentDigest: intent.digest,
    }), { kind: 'receiving', activeLeaseId: leaseId })
    await repository.createBootstrapCandidate({
      candidate: bootstrap,
      provisionalParentHandle: parentHandle,
      lease,
    })
    await repository.commitBootstrap({
      candidate: bootstrap,
      operation,
      operationRecord: storedReceiveOperationRecord(operation),
      lifecycle: receiving,
      lifecycleRecord: await storedReceiveLifecycleState(receiving),
      handles: [parentHandle, fileHandle],
      lease,
      checkpoint,
    })
    leaseId = await replaceDirectZipLease(repository, operation, receiving.generation, leaseId)
    const { centralPage, fence, page } = await stageFirstMemberPages(
      repository,
      checkpoint,
      operationId,
      leaseId,
    )
    const {
      candidate,
      candidateBindScopes,
      promoted,
      promotedObservation,
      promotedState,
      resumable,
    } = await promoteFirstEpoch({
      repository,
      fence,
      checkpoint,
      page,
      centralPage,
      observation,
      operationId,
      leaseId,
      parentBindingDigest,
      fileBindingDigest,
      receiving,
    })
    const recoveryExpectedRangeDigest = identity(32, 45)
    const proposedAfterGate = await createDirectZipCheckpointProposalV1({
      ...promoted,
      generation: 3n,
      predecessorCheckpointDigest: promoted.digest,
      archiveOffset: 2n,
      committedArchiveLength: 2n,
      epochRootDigest: epochRoot(promoted.epochRootDigest, 1n, 2n, recoveryExpectedRangeDigest),
    })
    const recoveryCandidate = await createDirectZipCommitCandidateV1({
      kind: 'epoch',
      operationId,
      candidateId: identity(16, 44),
      leaseId,
      predecessorCheckpointGeneration: 2n,
      predecessorCheckpointDigest: promoted.digest,
      expectedRangeDigest: recoveryExpectedRangeDigest,
      predecessorTargetObservation: promotedObservation,
      proposedCheckpoint: proposedAfterGate,
    })
    const recoveryFence = { operationId, leaseId, checkpointGeneration: 2n }
    await repository.bindCandidate(recoveryFence, recoveryCandidate)
    const gate = await createDirectZipRecoveryGateV1({
      operationId,
      receiveIntentDigest: intent.digest,
      kind: 'authorization-required',
      checkpointDigest: promoted.digest,
      candidateDigest: recoveryCandidate.digest,
    })
    const authorizationRequired = nextReceiveLifecycleState(resumable, {
      kind: 'authorization-required',
      recoveryGateDigest: gate.digest,
      expiresAt: 86_400_002,
    })
    await repository.commitRecoveryLifecycle({
      fence: recoveryFence,
      candidate: recoveryCandidate,
      lifecycle: authorizationRequired,
      lifecycleRecord: await storedReceiveLifecycleState(authorizationRequired),
      recoveryGate: gate,
    })
    const gatedState = await repository.readState(operationId)
    const recoveredObservation = await freshObservation(proposedAfterGate, observation, 3)
    const recoveredCheckpoint = await createDirectZipCheckpointV1({
      ...proposedAfterGate,
      candidateLineageDigest: recoveryCandidate.digest,
      targetObservation: recoveredObservation,
    })
    const resolved = nextReceiveLifecycleState(authorizationRequired, {
      kind: 'resumable-receive',
      payloadKind: 'direct-zip',
      directZipCheckpointDigest: recoveredCheckpoint.digest,
      safeSelectedPayloadBytes: recoveredCheckpoint.committedSelectedPayloadBytes,
      committedArchiveLength: recoveredCheckpoint.committedArchiveLength,
      checkpointPhase: recoveredCheckpoint.phase,
      expiresAt: 86_400_003,
    })
    await repository.promoteCandidate({
      fence: recoveryFence,
      candidate: recoveryCandidate,
      checkpoint: recoveredCheckpoint,
      lifecycle: resolved,
      lifecycleRecord: await storedReceiveLifecycleState(resolved),
    })
    const truncateExpectedRangeDigest = identity(32, 47)
    const candidateCheckpoint = await createDirectZipCheckpointProposalV1({
      ...recoveredCheckpoint,
      generation: 4n,
      predecessorCheckpointDigest: recoveredCheckpoint.digest,
      archiveOffset: 3n,
      committedArchiveLength: 3n,
      epochRootDigest: epochRoot(
        recoveredCheckpoint.epochRootDigest,
        2n,
        3n,
        truncateExpectedRangeDigest,
      ),
    })
    const truncateCandidate = await createDirectZipCommitCandidateV1({
      kind: 'epoch',
      operationId,
      candidateId: identity(16, 46),
      leaseId,
      predecessorCheckpointGeneration: 3n,
      predecessorCheckpointDigest: recoveredCheckpoint.digest,
      expectedRangeDigest: truncateExpectedRangeDigest,
      predecessorTargetObservation: recoveredObservation,
      proposedCheckpoint: candidateCheckpoint,
    })
    const truncateFence = { operationId, leaseId, checkpointGeneration: 3n }
    await repository.bindCandidate(truncateFence, truncateCandidate)
    const truncatedObservation = await createDirectZipTargetObservationV1({
      operationId,
      parentBindingDigest,
      fileBindingDigest,
      ownershipMarkerDigest: observation.ownershipMarkerDigest,
      exactLength: proposedAfterGate.committedArchiveLength,
      lastModifiedMilliseconds: 2,
      epochRootDigest: proposedAfterGate.epochRootDigest,
    })
    const truncatedCheckpoint = await createDirectZipCheckpointV1({
      ...recoveredCheckpoint,
      generation: 4n,
      predecessorCheckpointDigest: recoveredCheckpoint.digest,
      candidateLineageDigest: truncateCandidate.digest,
      targetObservation: truncatedObservation,
    })
    const replaying = nextReceiveLifecycleState(resolved, {
      kind: 'resumable-receive',
      payloadKind: 'direct-zip',
      directZipCheckpointDigest: truncatedCheckpoint.digest,
      safeSelectedPayloadBytes: truncatedCheckpoint.committedSelectedPayloadBytes,
      committedArchiveLength: truncatedCheckpoint.committedArchiveLength,
      checkpointPhase: truncatedCheckpoint.phase,
      expiresAt: 86_400_004,
    })
    await repository.retireCandidate({
      fence: truncateFence,
      candidate: truncateCandidate,
      disposition: 'truncate-and-replay',
      checkpoint: truncatedCheckpoint,
      lifecycle: replaying,
      lifecycleRecord: await storedReceiveLifecycleState(replaying),
    })

    let staleFenceFailure = 'none'
    try {
      await repository.stagePage(fence, page)
    } catch (error) {
      staleFenceFailure = error instanceof DOMException ? error.name : 'Error'
    }
    const finalFence = { operationId, leaseId, checkpointGeneration: 4n }
    const orphan = await createDirectZipImmutablePageV1({
      operationId,
      pageKind: 'layout',
      chainId: page.chainId,
      pageOrdinal: 1,
      predecessorRootDigest: page.chainRootDigest,
      canonicalEntries: [Uint8Array.of(8)],
      accountingPredecessor: {
        kind: 'page',
        pageKind: 'layout',
        pageId: page.id,
        pageDigest: page.digest,
      },
      previousBudgetUsage: page.budgetUsage,
      previousChainRecordCount: page.chainRecordCount,
      previousChainCanonicalMetadataBytes: page.chainCanonicalMetadataBytes,
    })
    await repository.stagePage(finalFence, orphan)
    const orphanCollection = await repository.collectOrphanPages(finalFence)
    const state = await repository.readState(operationId)
    const pages = await repository.readPageBatch({
      operationId,
      pageKind: 'layout',
      chainId: page.chainId,
    })
    return {
      checkpointGeneration: state?.checkpointGeneration,
      atomicCandidatePageFence: candidateBindScopes.some(scope => [
        INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
        INDEXEDDB_DIRECT_ZIP_LAYOUT_PAGE_STORE,
        INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE,
        INDEXEDDB_DIRECT_ZIP_EPOCH_PAGE_STORE,
      ].every(store => scope.includes(store))),
      freshObservationBound: promotedState?.checkpoint.targetObservation.digest ===
        promotedObservation.digest && promotedState.checkpoint.candidateLineageDigest === candidate.digest,
      gatedRecoveryDigest: gatedState?.recoveryGate?.digest,
      gateCleared: state?.recoveryGate === undefined,
      candidatePresent: (await repository.readOperationCandidate(operationId)) !== undefined,
      pageCount: pages.pages.length,
      orphanPagesDeleted: orphanCollection.deletedPageCount.toString(10),
      staleFenceFailure,
      leaseReplaced: state?.leaseId === leaseId,
    }
  } finally {
    repository.close()
    await deleteDatabase(databaseName)
  }
}

async function replaceDirectZipLease(
  repository: IndexedDbDirectZipJournalRepository,
  operation: import('../../src/output/workspace/records').ReceiveOperationV2,
  lifecycleGeneration: bigint,
  expectedLeaseId: string,
): Promise<string> {
  const leaseId = identity(16, 48)
  await repository.commitLeaseAcquisition({
    operationId: operation.operationId,
    expectedLifecycleGeneration: lifecycleGeneration,
    expectedLeaseId,
    records: [storedReceiveOperationRecord(operation)],
    lease: {
      kind: 'put',
      record: receiveOperationLeaseRecord({ operationId: operation.operationId, leaseId, acquiredAt: 2 }),
    },
  })
  return leaseId
}

async function promoteFirstEpoch(input: Readonly<{
  repository: IndexedDbDirectZipJournalRepository
  fence: Parameters<IndexedDbDirectZipJournalRepository['bindCandidate']>[0]
  checkpoint: Awaited<ReturnType<typeof createDirectZipCheckpointV1>>
  page: Awaited<ReturnType<typeof createDirectZipImmutablePageV1>>
  centralPage: Awaited<ReturnType<typeof createDirectZipImmutablePageV1>>
  observation: Awaited<ReturnType<typeof createDirectZipTargetObservationV1>>
  operationId: string
  leaseId: string
  parentBindingDigest: string
  fileBindingDigest: string
  receiving: ReturnType<typeof nextReceiveLifecycleState>
}>) {
  const expectedRangeDigest = identity(32, 43)
  const proposed = await createDirectZipCheckpointProposalV1({
    ...input.checkpoint,
    generation: 2n,
    predecessorCheckpointDigest: input.checkpoint.digest,
    entryOrdinal: 1n,
    archiveOffset: 1n,
    committedArchiveLength: 1n,
    epochRootDigest: epochRoot(
      input.checkpoint.epochRootDigest,
      input.checkpoint.committedArchiveLength,
      1n,
      expectedRangeDigest,
    ),
    layoutPages: {
      chainId: input.page.chainId,
      rootDigest: input.page.chainRootDigest,
      pageCount: 1n,
      recordCount: input.page.chainRecordCount,
      canonicalMetadataBytes: input.page.chainCanonicalMetadataBytes,
    },
    centralPages: {
      chainId: input.centralPage.chainId,
      rootDigest: input.centralPage.chainRootDigest,
      pageCount: 1n,
      recordCount: input.centralPage.chainRecordCount,
      canonicalMetadataBytes: input.centralPage.chainCanonicalMetadataBytes,
    },
    journalUsage: input.centralPage.budgetUsage,
    accountingTailPageId: input.centralPage.id,
  })
  const candidate = await createDirectZipCommitCandidateV1({
    kind: 'epoch',
    operationId: input.operationId,
    candidateId: identity(16, 42),
    leaseId: input.leaseId,
    predecessorCheckpointGeneration: 1n,
    predecessorCheckpointDigest: input.checkpoint.digest,
    expectedRangeDigest,
    predecessorTargetObservation: input.observation,
    proposedCheckpoint: proposed,
  })
  const candidateBindScopes = await bindCandidateAndObserveTransactionScope(
    input.repository,
    input.fence,
    candidate,
  )
  const promotedObservation = await createDirectZipTargetObservationV1({
    operationId: input.operationId,
    parentBindingDigest: input.parentBindingDigest,
    fileBindingDigest: input.fileBindingDigest,
    ownershipMarkerDigest: input.observation.ownershipMarkerDigest,
    exactLength: proposed.committedArchiveLength,
    lastModifiedMilliseconds: 2,
    epochRootDigest: proposed.epochRootDigest,
  })
  const promoted = await createDirectZipCheckpointV1({
    ...proposed,
    candidateLineageDigest: candidate.digest,
    targetObservation: promotedObservation,
  })
  const resumable = nextReceiveLifecycleState(input.receiving, {
    kind: 'resumable-receive',
    payloadKind: 'direct-zip',
    directZipCheckpointDigest: promoted.digest,
    safeSelectedPayloadBytes: promoted.committedSelectedPayloadBytes,
    committedArchiveLength: promoted.committedArchiveLength,
    checkpointPhase: promoted.phase,
    expiresAt: 86_400_001,
  })
  await input.repository.promoteCandidate({
    fence: input.fence,
    candidate,
    checkpoint: promoted,
    lifecycle: resumable,
    lifecycleRecord: await storedReceiveLifecycleState(resumable),
  })
  return Object.freeze({
    candidate,
    candidateBindScopes,
    promoted,
    promotedObservation,
    promotedState: await input.repository.readState(input.operationId),
    resumable,
  })
}

function freshObservation(
  proposal: Awaited<ReturnType<typeof createDirectZipCheckpointProposalV1>>,
  predecessor: Awaited<ReturnType<typeof createDirectZipTargetObservationV1>>,
  lastModifiedMilliseconds: number,
) {
  return createDirectZipTargetObservationV1({
    operationId: proposal.operationId,
    parentBindingDigest: proposal.parentBindingDigest,
    fileBindingDigest: proposal.fileBindingDigest,
    ownershipMarkerDigest: predecessor.ownershipMarkerDigest,
    exactLength: proposal.committedArchiveLength,
    lastModifiedMilliseconds,
    epochRootDigest: proposal.epochRootDigest,
  })
}

async function stageFirstMemberPages(
  repository: IndexedDbDirectZipJournalRepository,
  checkpoint: Awaited<ReturnType<typeof createDirectZipCheckpointV1>>,
  operationId: string,
  leaseId: string,
) {
  const page = await createDirectZipImmutablePageV1({
    operationId,
    pageKind: 'layout',
    chainId: checkpoint.layoutPages.chainId,
    pageOrdinal: 0,
    predecessorRootDigest: identity(32, 0),
    canonicalEntries: [Uint8Array.of(7)],
    accountingPredecessor: {
      kind: 'checkpoint',
      checkpointGeneration: checkpoint.generation,
      checkpointDigest: checkpoint.digest,
    },
    previousBudgetUsage: checkpoint.journalUsage,
    previousChainRecordCount: 0n,
    previousChainCanonicalMetadataBytes: 0n,
  })
  const fence = { operationId, leaseId, checkpointGeneration: 1n }
  await repository.stagePage(fence, page)
  const centralPage = await createDirectZipImmutablePageV1({
    operationId,
    pageKind: 'central',
    chainId: checkpoint.centralPages.chainId,
    pageOrdinal: 0,
    predecessorRootDigest: identity(32, 0),
    canonicalEntries: [Uint8Array.of(8)],
    accountingPredecessor: {
      kind: 'page',
      pageKind: 'layout',
      pageId: page.id,
      pageDigest: page.digest,
    },
    previousBudgetUsage: page.budgetUsage,
    previousChainRecordCount: 0n,
    previousChainCanonicalMetadataBytes: 0n,
  })
  await repository.stagePage(fence, centralPage)
  return Object.freeze({ centralPage, fence, page })
}

async function bindCandidateAndObserveTransactionScope(
  repository: IndexedDbDirectZipJournalRepository,
  fence: Parameters<IndexedDbDirectZipJournalRepository['bindCandidate']>[0],
  candidate: Parameters<IndexedDbDirectZipJournalRepository['bindCandidate']>[1],
): Promise<readonly string[][]> {
  const scopes: string[][] = []
  const databasePrototype = IDBDatabase.prototype as unknown as {
    transaction: (
      this: IDBDatabase,
      storeNames: string | readonly string[],
      mode?: IDBTransactionMode,
      options?: IDBTransactionOptions,
    ) => IDBTransaction
  }
  const originalTransaction = databasePrototype.transaction
  databasePrototype.transaction = function (storeNames, mode, options) {
    scopes.push(typeof storeNames === 'string' ? [storeNames] : [...storeNames])
    return originalTransaction.call(this, storeNames, mode, options)
  }
  try {
    await repository.bindCandidate(fence, candidate)
  } finally {
    databasePrototype.transaction = originalTransaction
  }
  return Object.freeze(scopes)
}

function openRawDatabase(
  name: string,
  version: number,
  upgrade?: (database: IDBDatabase, transaction: IDBTransaction) => void,
): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(name, version)
    request.addEventListener('upgradeneeded', () => {
      const transaction = request.transaction
      if (transaction === null) throw new Error('upgrade transaction is unavailable')
      upgrade?.(request.result, transaction)
    })
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => resolve(request.result), { once: true })
  })
}

async function put(database: IDBDatabase, store: string, value: unknown): Promise<void> {
  const transaction = database.transaction(store, 'readwrite')
  transaction.objectStore(store).put(value)
  await transactionCompletion(transaction)
}

async function remove(database: IDBDatabase, store: string, key: IDBValidKey): Promise<void> {
  const transaction = database.transaction(store, 'readwrite')
  transaction.objectStore(store).delete(key)
  await transactionCompletion(transaction)
}

async function count(database: IDBDatabase, store: string): Promise<number> {
  const transaction = database.transaction(store, 'readonly')
  const request = transaction.objectStore(store).count()
  const result = await new Promise<number>((resolve, reject) => {
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
  await transactionCompletion(transaction)
  return result
}

function deleteDatabase(name: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.addEventListener('success', () => resolve(), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('blocked', () => reject(new Error('database deletion was blocked')), {
      once: true,
    })
  })
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}

function epochRoot(predecessor: string, start: bigint, end: bigint, content: string): string {
  return encodeBase64Url(chainDirectZipEpochDigestV1({
    predecessorRoot: decodeBase64Url(predecessor)!,
    start,
    end,
    contentDigest: decodeBase64Url(content)!,
  }))
}
