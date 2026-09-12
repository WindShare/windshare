import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'

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
  FreshProgressiveZipAdmissionProof,
  ProductProgressiveZipAdmissionProof,
  TransferJobProgressiveZipProof,
} from './durable-preparation-harness'
const RECOVERY_HARNESS_PATH = '/test/browser/durable-recovery-harness.ts'
const PREPARATION_HARNESS_PATH = '/test/browser/durable-preparation-harness.ts'

test.beforeEach(async ({ browserName, page }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
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
    await otherPage.goto(BROWSER_CONTRACT_HOST_PATH)
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

test('commits progressive ZIP admission through fresh browser durability authorities', async ({
  page,
}) => {
  const key = crypto.randomUUID()
  const result = await page.evaluate(async ({ path, fixtureKey }) => {
    const harness = await import(path) as typeof import('./durable-preparation-harness')
    return harness.proveFreshProgressiveZipAdmission(fixtureKey)
  }, { path: PREPARATION_HARNESS_PATH, fixtureKey: key }) as FreshProgressiveZipAdmissionProof

  expect(result).toEqual({
    lifecycle: 'receiving',
    contentRequests: '0',
    traceNames: ['receive.preparation_admission.accepted'],
    receiptCount: 1,
    manifestPageCount: 0,
    objectHandlePresent: true,
  })
})

test('admits product-bound workspace ZIP before requesting content', async ({ page }) => {
  const key = crypto.randomUUID()
  const result = await page.evaluate(async ({ path, fixtureKey }) => {
    const harness = await import(path) as typeof import('./durable-preparation-harness')
    return harness.proveProductProgressiveZipAdmission(fixtureKey)
  }, { path: PREPARATION_HARNESS_PATH, fixtureKey: key }) as ProductProgressiveZipAdmissionProof

  const { checkpointBeforePause, checkpointAfterPause, paused, ...summary } = result
  expect(summary).toEqual({
    admission: 'accepted',
    lifecycle: 'receiving',
    traceNames: [
      'receive.preparation_admission.accepted',
      'receive.capacity.reserved',
      'receive.opfs.checkpoint',
      'receive.capacity.released',
      'receive.materialization.paused',
      'receive.operation.discarded',
    ],
    checkpointStages: ['closed'],
    cleanup: 'discarded',
  })
  // No payload was written: pausing must reuse the durable initial cut without
  // an extra flush, while retaining resumable authority before capacity cleanup.
  expect(checkpointBeforePause).toMatchObject({
    generation: 1n,
    allocatedLength: 0n,
    physicalLength: 0n,
    entryCount: 0n,
    discoveryComplete: false,
    artifactState: 'receiving',
  })
  expect(checkpointAfterPause).toEqual(checkpointBeforePause)
  expect(paused).toMatchObject({
    kind: 'resumable-receive',
    payloadKind: 'opfs-zip',
    objectId: checkpointBeforePause.object.objectId,
    checkpointGeneration: checkpointBeforePause.generation,
    completedFileCount: 0n,
    completedBytes: 0n,
    discoveryComplete: false,
    occupiedBytes: 0n,
  })
})

test('receives catalog-discovered TransferJob files through progressive workspace output', async ({
  page,
}) => {
  const result = await page.evaluate(async (path) => {
    const harness = await import(path) as typeof import('./durable-preparation-harness')
    return harness.proveTransferJobProgressiveZip()
  }, PREPARATION_HARNESS_PATH) as TransferJobProgressiveZipProof

  expect(result).toMatchObject({
    worker: 'Succeeded',
    lifecycle: 'download-started',
    evidence: {
      admittedFilePaths: ['micro-share/pixel.png'],
      admissionCount: 1,
      discoveryCompleteCalls: 1,
    },
    cleanup: 'discarded',
  })
  expect(result.workspaceTraceNames).not.toContain('receive.preparation.started')
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

for (const cut of ['receiving', 'materialization-sealed'] as const) {
  test(`completed original ${cut} crash cut saves the same object offline`, async ({ page, context }) => {
    const created = await page.evaluate(async ({ path, key }) => {
      const harness = await import(path) as typeof import('./durable-recovery-harness')
      return harness.createOriginPrivateReceiveCrashCut(key, 'complete')
    }, { path: RECOVERY_HARNESS_PATH, key: crypto.randomUUID() })
    expect(created).toMatchObject({ lifecycle: 'receiving', ranges: ['0:5'] })
    const crashPath = '/test/browser/opfs/opfs-publication-crash-harness.ts'
    await page.reload()
    if (cut === 'materialization-sealed') {
      const sealed = await page.evaluate(async ({ path, fixture }) => {
        const harness = await import(path) as typeof import('./opfs/opfs-publication-crash-harness')
        return harness.recoverCompletedOriginal(fixture, true)
      }, { path: crashPath, fixture: created.fixture })
      expect(sealed).toMatchObject({ continuation: 'resume-local-finalization', state: cut })
      await page.reload()
    }
    await page.evaluate(async path => { await import(path) }, crashPath)
    await context.setOffline(true)
    const downloaded = page.waitForEvent('download')
    const recovered = await page.evaluate(async ({ path, fixture }) => {
      const harness = await import(path) as typeof import('./opfs/opfs-publication-crash-harness')
      return harness.recoverCompletedOriginal(fixture)
    }, { path: crashPath, fixture: created.fixture })
    expect(recovered).toMatchObject({
      priorState: cut, state: 'download-started',
      objectId: created.completedObjectId, bytes: [1, 2, 3, 4, 5],
    })
    expect(await (await downloaded).failure()).toBeNull()
  })
}

test('fresh inventory recovers an interrupted browser handoff and saves the same object offline', async ({ page, context }) => {
  const cut = await page.evaluate(async ({ path, key }) => {
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.createOriginPrivateReceiveCrashCut(key)
  }, { path: RECOVERY_HARNESS_PATH, key: crypto.randomUUID() })
  await page.reload()
  const fixture = await page.evaluate(async ({ path, cut }) => {
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return (await harness.recoverReceiveAndSealPackage(cut.fixture)).fixture
  }, { path: RECOVERY_HARNESS_PATH, cut })
  const crashPath = '/test/browser/opfs/opfs-publication-crash-harness.ts'
  await page.evaluate(async ({ path, fixture }) => {
    const harness = await import(path) as typeof import('./opfs/opfs-publication-crash-harness')
    return harness.interruptRetainedHandoff(fixture)
  }, { path: crashPath, fixture })
  await page.reload()
  await page.evaluate(async path => { await import(path) }, crashPath)
  await context.setOffline(true)
  const downloaded = page.waitForEvent('download')
  const recovered = await page.evaluate(async ({ path, fixture }) => {
    const harness = await import(path) as typeof import('./opfs/opfs-publication-crash-harness')
    return harness.recoverAndSaveInterruptedHandoff(fixture)
  }, { path: crashPath, fixture })
  expect(recovered).toEqual({
    priorState: 'handing-off', normalizedState: 'waiting-to-save', state: 'download-started',
    packageDigest: fixture.package.digest, objectId: fixture.rawOwnedObjectId,
  })
  const download = await downloaded
  expect(await download.failure()).toBeNull()
})

test('recovers a FileCheckpoint, reuses its original object, and retries offline after reload', async ({
  page,
  context,
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

  expect(recovered.fixture.package.packageOwnedObjectId).toBe(recovered.fixture.rawOwnedObjectId)
  await page.reload()
  await page.evaluate(async path => { await import(path) }, RECOVERY_HARNESS_PATH)
  await context.setOffline(true)
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
    contentRequests: '0',
    packageSeals: 0,
    publicationAttempts: 1,
    cleanup: 'clean',
  })
})
