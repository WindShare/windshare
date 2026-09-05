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
const RECOVERY_HARNESS_PATH = '/test/browser/durable-recovery-harness.ts'
const PREPARATION_HARNESS_PATH = '/test/browser/durable-preparation-harness.ts'

test.beforeEach(async ({ browserName, page }) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
})

test('reopens compatible-name translation without changing materialization-relative checkpoint lineage', async ({
  page,
}) => {
  const key = `compatible-${crypto.randomUUID()}`
  const cut = await page.evaluate(async ({ path, fixtureKey }) => {
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.createCompatibleNameRecoveryCut(fixtureKey)
  }, { path: RECOVERY_HARNESS_PATH, fixtureKey: key }) as CompatibleNameRecoveryCut
  expect(cut).toMatchObject({
    materializationRelativeCheckpointPath: ['logical-checkpoint.bin'],
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
    materializationRelativeCheckpointPath: ['logical-checkpoint.bin'],
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

test('catches up committed names locally after reload and preserves real receive continuation', async ({ page, context }) => {
  const harnessPath = '/test/browser/compatible-name-catch-up-harness.ts'
  const cut = await page.evaluate(async ({ path, key }) => {
    const harness = await import(path) as typeof import('./compatible-name-catch-up-harness')
    return harness.createActiveCompatibleNameCatchUpCut(key)
  }, { path: harnessPath, key: crypto.randomUUID() })
  expect(cut).toMatchObject({
    committedCount: 1,
    observedCount: 0,
    restoreCommandAvailable: false,
    footer: 'active',
    sidecarSync: 'pending',
    terminalSettlement: 'none',
    pendingOutcomePresent: false,
    lifecycle: 'receiving',
    durableReceiveLeasePresent: true,
    continuation: 'resume-receive',
  })
  expect(cut.injectedWriteFailures).toBeGreaterThan(0)
  const otherPage = await context.newPage()
  try {
    await otherPage.goto('/')
    const liveOperationExposed = await otherPage.evaluate(async ({ path, fixture }) => {
      const harness = await import(path) as typeof import('./compatible-name-catch-up-harness')
      return harness.retainedOperationPresent(fixture)
    }, { path: harnessPath, fixture: cut.fixture })
    expect(liveOperationExposed).toBe(false)
  } finally {
    await otherPage.close()
  }
  await page.reload()
  // Load the local harness before disabling networking; no sender or transport can help recovery.
  await page.evaluate(async path => { await import(path) }, harnessPath)
  await context.setOffline(true)
  const proof = await page.evaluate(async ({ path, fixture }) => {
    const harness = await import(path) as typeof import('./compatible-name-catch-up-harness')
    return harness.catchUpActiveCompatibleNamesAfterReload(fixture)
  }, { path: harnessPath, fixture: cut.fixture })
  expect(proof).toMatchObject({
    lifecycle: 'receiving',
    lifecycleUnchanged: true,
    restoreCommandAvailable: true,
    continuationBefore: 'resume-receive',
    continuationAfter: 'resume-receive',
    footer: 'active',
    committedCount: 1,
    sidecarSync: 'current',
    terminalSettlement: 'none',
    pendingOutcomePresent: false,
  })
  expect(proof.actionsBefore).toContain('continue')
  expect(proof.actionsBefore).toContain('catch-up')
  expect(proof.actionsAfter).toContain('continue')
  expect(proof.actionsAfter).not.toContain('catch-up')
  expect(proof.sidecarName).toBe(proof.scriptName.replace(/\.ps1$/u, '.data'))
  const resumed = await page.evaluate(async ({ path, fixture }) => {
    const harness = await import(path) as typeof import('./compatible-name-catch-up-harness')
    return harness.resumeAfterActiveCompatibleNameCatchUp(fixture)
  }, { path: harnessPath, fixture: cut.fixture })
  expect(resumed).toMatchObject({
    lifecycle: 'receiving',
    retainedFileRecovery: 'preserve',
    resumedRanges: ['0:2'],
    physicalBytes: [1, 2, 3, 4],
    committedOrdinal: 2,
    sidecarCommittedCount: 2,
    reopenedRepairSummaryCount: 1,
    incompleteTailTruncated: true,
  })
  await context.setOffline(false)
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
