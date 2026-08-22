import { expect, test } from '@playwright/test'

import { requireOriginPrivateStorage } from './browser-storage-support'
import type {
  CompatibleNameRecoveryCut,
  CompatibleNameRecoveryProof,
  DurablePackageFixture,
  DurableReceiveFixture,
  FreshPageWorkspaceResumeCut,
  FreshPageWorkspaceResumeFixture,
  FreshPageWorkspaceResumeProof,
  PublicationRetryResult,
  ReceiveCrashCutResult,
  RecoveredPackageResult,
  WorkspaceActivationReloadCut,
  WorkspaceActivationReloadProof,
} from './durable-recovery-harness'
import type {
  FreshPreparedZipAdmissionProof,
  ProductPreparedZipAdmissionProof,
  TransferJobPreparedZipProof,
} from './durable-preparation-harness'
import type {
  IndexedDbCheckpointLineageProbe,
  IndexedDbV9Probe,
} from './durable-recovery-idb-probe'

const RECOVERY_HARNESS_PATH = '/test/browser/durable-recovery-harness.ts'
const PREPARATION_HARNESS_PATH = '/test/browser/durable-preparation-harness.ts'
const IDB_PROBE_PATH = '/test/browser/durable-recovery-idb-probe.ts'

test.beforeEach(async ({ browserName, page }) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
})

test('reopens compatible-name translation without changing logical checkpoint lineage', async ({
  page,
}) => {
  const key = `compatible-${crypto.randomUUID()}`
  const cut = await page.evaluate(async ({ path, fixtureKey }) => {
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.createCompatibleNameRecoveryCut(fixtureKey)
  }, { path: RECOVERY_HARNESS_PATH, fixtureKey: key }) as CompatibleNameRecoveryCut
  expect(cut).toMatchObject({
    logicalCheckpointPath: ['logical-checkpoint.bin'],
    physicalComponent: 'logical-checkpoint-bin.windshare-aaaaaa',
    rejectedEntriesBefore: [],
    logicalEntryAbsent: true,
    sidecarCommittedCountBeforeCommit: 0,
    durableActivationState: 'active',
    durableRepairSummaryCount: 0,
    checkpointRanges: ['0:2'],
    physicalPrefixBytes: [1, 2],
  })

  await page.reload()
  const proof = await page.evaluate(async ({ path, fixture }) => {
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.reopenCompatibleNameRecovery(fixture)
  }, { path: RECOVERY_HARNESS_PATH, fixture: cut.fixture }) as CompatibleNameRecoveryProof
  expect(proof).toEqual({
    headerPointRead: true,
    logicalCheckpointPath: ['logical-checkpoint.bin'],
    physicalComponent: 'logical-checkpoint-bin.windshare-aaaaaa',
    committedOrdinal: 1,
    resumedRanges: ['0:2'],
    physicalBytes: [1, 2, 3, 4],
    sidecarCommittedCount: 1,
    reopenedRepairSummaryCount: 0,
    incompleteTailTruncated: true,
    logicalEntryAbsent: true,
  })
})

test('v8 repositories migrate destructively to v9 and fail closed across IndexedDB boundaries', async ({
  page,
}) => {
  const result = await page.evaluate(async (path) => {
    const probe = await import(path) as typeof import('./durable-recovery-idb-probe')
    return probe.probeIndexedDbV9Replacement()
  }, IDB_PROBE_PATH) as IndexedDbV9Probe

  expect(result).toEqual({
    blockedUpgrade: 'InvalidStateError',
    blockedRequestClosedLate: true,
    versionChange: 'InvalidStateError',
    schemaVersion: 9,
    v9StoresPresent: true,
    legacyStoreRetainedForCleanup: true,
    legacyRowsVisibleToV9: false,
  })
})

test('promotes an exactly marked workspace activation candidate after reload', async ({ page }) => {
  const key = `activation-${crypto.randomUUID()}`
  const cut = await page.evaluate(async ({ path, key: fixtureKey }) => {
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.createWorkspaceActivationReloadCut(fixtureKey)
  }, { path: RECOVERY_HARNESS_PATH, key }) as WorkspaceActivationReloadCut
  expect(cut.candidateCount).toBe(1)

  await page.reload()
  const proof = await page.evaluate(async ({ path, cut: fixture }) => {
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.recoverWorkspaceActivationReloadCut(fixture)
  }, { path: RECOVERY_HARNESS_PATH, cut }) as WorkspaceActivationReloadProof
  expect(proof).toEqual({
    candidateCount: 0,
    promotedHandlePresent: true,
    lifecycle: 'needs-attention',
    retainedContinuation: 'needs-attention',
  })
})

test('checkpoint lineage claim reconciles physical crash evidence and serializes authorities', async ({
  page,
}) => {
  const result = await page.evaluate(async (path) => {
    const probe = await import(path) as typeof import('./durable-recovery-idb-probe')
    return probe.probeIndexedDbCheckpointLineage()
  }, IDB_PROBE_PATH) as IndexedDbCheckpointLineageProbe

  expect(result).toEqual({
    putCandidateSurfacePresent: false,
    unbackedUpdateRejection: 'InvalidStateError',
    unbackedUpdateCandidateRows: 0,
    updateConcurrencyOutcomes: ['InvalidStateError', 'resolved'],
    updateConcurrencyCandidateRows: 1,
    concurrentKinds: ['exact', 'installed'],
    concurrentObjectConverged: true,
    candidateRowsBeforeResolution: 1,
    candidateBeforeObjectDecision: 'exact',
    candidateRowsAfterResolution: 0,
    resolutionReplayDecision: 'exact',
    revisionDecision: 'revision-conflict',
    ownershipDecision: 'ownership-conflict',
    invalidDecision: 'invalid',
    crossLineageOwnershipDecision: 'ownership-conflict',
    unresolvedCandidateRejection: 'InvalidStateError',
    resolvedRange: '0:1',
  })
})

test('commits prepared ZIP admission through fresh browser durability authorities', async ({
  page,
}) => {
  const key = crypto.randomUUID()
  const result = await page.evaluate(async ({ path, fixtureKey }) => {
    const harness = await import(path) as typeof import('./durable-preparation-harness')
    return harness.proveFreshPreparedZipAdmission(fixtureKey)
  }, { path: PREPARATION_HARNESS_PATH, fixtureKey: key }) as FreshPreparedZipAdmissionProof

  expect(result).toEqual({
    lifecycle: 'receiving',
    contentRequests: '0',
    traceNames: [
      'receive.preparation.started',
      'receive.preparation.sealed',
      'receive.preparation_admission.accepted',
    ],
    receiptCount: 1,
    manifestPageCount: 1,
    layoutHandlePresent: true,
  })
})

test('admits product-bound workspace ZIP before requesting content', async ({ page }) => {
  const key = crypto.randomUUID()
  const result = await page.evaluate(async ({ path, fixtureKey }) => {
    const harness = await import(path) as typeof import('./durable-preparation-harness')
    return harness.proveProductPreparedZipAdmission(fixtureKey)
  }, { path: PREPARATION_HARNESS_PATH, fixtureKey: key }) as ProductPreparedZipAdmissionProof

  expect(result).toEqual({
    admission: 'accepted',
    lifecycle: 'receiving',
    traceNames: [
      'receive.preparation.started',
      'receive.preparation.sealed',
      'receive.preparation_admission.accepted',
      'receive.materialization.paused',
      'receive.operation.discarded',
    ],
    cleanup: 'discarded',
  })
})

test('admits catalog-derived TransferJob evidence before requesting workspace content', async ({
  page,
}) => {
  const result = await page.evaluate(async (path) => {
    const harness = await import(path) as typeof import('./durable-preparation-harness')
    return harness.proveTransferJobPreparedZip()
  }, PREPARATION_HARNESS_PATH) as TransferJobPreparedZipProof

  expect(result).toMatchObject({
    worker: 'Succeeded',
    lifecycle: 'waiting-to-save',
    evidence: {
      entryCount: '3',
      fileCount: '1',
      directoryCount: '2',
      selectedRawBytes: '68',
      generationCount: 2,
      entries: [
        {
          kind: 'directory',
          role: 'result-root',
          sourceSegmentCount: 0,
          artifactSegmentCount: 1,
          modifiedTimePresent: false,
        },
        {
          kind: 'directory',
          role: 'necessary-ancestor',
          sourceSegmentCount: 1,
          artifactSegmentCount: 2,
          modifiedTimePresent: true,
        },
        {
          kind: 'file',
          sourceSegmentCount: 2,
          artifactSegmentCount: 3,
          modifiedTimePresent: true,
        },
      ],
    },
    cleanup: 'discarded',
  })
  expect(result.workspaceTraceNames).toContain('receive.preparation.sealed')
  expect(result.workspaceTraceNames).toContain('receive.preparation_admission.accepted')
  expect(result.transferTraceNames).toContain('materialization_completed')
  expect(result.transferTraceNames).not.toContain('materialization_failed')
})

test('reopens workspace admission authority from a fresh page', async ({ page }) => {
  const key = crypto.randomUUID()
  const cut = await page.evaluate(async ({ path, fixtureKey }) => {
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.createFreshPageWorkspaceResumeCut(fixtureKey)
  }, { path: RECOVERY_HARNESS_PATH, fixtureKey: key }) as FreshPageWorkspaceResumeCut
  expect(cut.lifecycle).toBe('resumable-receive')

  await page.reload()
  const reopened = await page.evaluate(async ({ path, fixture }) => {
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.reopenFreshPageWorkspaceResume(fixture)
  }, {
    path: RECOVERY_HARNESS_PATH,
    fixture: cut.fixture as FreshPageWorkspaceResumeFixture,
  }) as FreshPageWorkspaceResumeProof

  expect(reopened).toEqual({
    lifecycle: 'receiving',
    admittedContentReopened: true,
    cleanup: 'clean',
  })
})

test('recovers a FileCheckpoint, seals once, and retries the retained package after reload', async ({
  page,
}) => {
  const key = crypto.randomUUID()
  const crashCut = await page.evaluate(async ({ path, fixtureKey }) => {
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.createOriginPrivateReceiveCrashCut(fixtureKey)
  }, { path: RECOVERY_HARNESS_PATH, fixtureKey: key }) as ReceiveCrashCutResult
  expect(crashCut).toMatchObject({
    ranges: ['0:3'],
    lifecycle: 'receiving',
    contentRequests: '1',
  })

  await page.reload()
  const recovered = await page.evaluate(async ({ path, fixture }) => {
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.recoverReceiveAndSealPackage(fixture)
  }, {
    path: RECOVERY_HARNESS_PATH,
    fixture: crashCut.fixture as DurableReceiveFixture,
  }) as RecoveredPackageResult
  expect(recovered).toMatchObject({
    recoveredRanges: ['0:3'],
    packageBytes: [1, 2, 3, 4, 5],
    recoveryDecision: 'resume-receive',
    lifecycle: 'waiting-to-save',
    contentRequests: '1',
    packageSeals: 1,
    publicationAttempts: 1,
  })

  await page.reload()
  const retried = await page.evaluate(async ({ path, fixture }) => {
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.retryRetainedPackagePublication(fixture)
  }, {
    path: RECOVERY_HARNESS_PATH,
    fixture: recovered.fixture as DurablePackageFixture,
  }) as PublicationRetryResult
  expect(retried).toEqual({
    packageBytes: [1, 2, 3, 4, 5],
    packageDigest: recovered.fixture.package.digest,
    originalExpiry: recovered.fixture.originalExpiry,
    restoredExpiry: recovered.fixture.originalExpiry,
    contentRequests: '0',
    packageSeals: 0,
    publicationAttempts: 1,
    cleanup: 'clean',
  })
})
