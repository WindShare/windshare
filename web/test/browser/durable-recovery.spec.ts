import { expect, test } from '@playwright/test'

import { requireOriginPrivateStorage } from './browser-storage-support'
import type {
  DurablePackageFixture,
  DurableReceiveFixture,
  FreshPageWorkspaceResumeCut,
  FreshPageWorkspaceResumeFixture,
  FreshPageWorkspaceResumeProof,
  FreshPreparedZipAdmissionProof,
  ProductPreparedZipAdmissionProof,
  PublicationRetryResult,
  TransferJobPreparedZipProof,
  ReceiveCrashCutResult,
  RecoveredPackageResult,
} from './durable-recovery-harness'
import type { IndexedDbV6Probe } from './durable-recovery-idb-probe'

const HARNESS_PATH = '/test/browser/durable-recovery-harness.ts'
const IDB_PROBE_PATH = '/test/browser/durable-recovery-idb-probe.ts'

test.beforeEach(async ({ browserName, page }) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
})

test('v6 repositories replace resume authority and fail closed across IndexedDB boundaries', async ({
  page,
}) => {
  const result = await page.evaluate(async (path) => {
    const probe = await import(path) as typeof import('./durable-recovery-idb-probe')
    return probe.probeIndexedDbV6Replacement()
  }, IDB_PROBE_PATH) as IndexedDbV6Probe

  expect(result).toEqual({
    blockedUpgrade: 'InvalidStateError',
    blockedRequestClosedLate: true,
    versionChange: 'InvalidStateError',
    schemaVersion: 6,
    v6StoresPresent: true,
    legacyStoreRetainedForCleanup: true,
    legacyRowsVisibleToV6: false,
  })
})

test('commits prepared ZIP admission through fresh browser durability authorities', async ({
  page,
}) => {
  const key = crypto.randomUUID()
  const result = await page.evaluate(async ({ path, fixtureKey }) => {
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.proveFreshPreparedZipAdmission(fixtureKey)
  }, { path: HARNESS_PATH, fixtureKey: key }) as FreshPreparedZipAdmissionProof

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
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.proveProductPreparedZipAdmission(fixtureKey)
  }, { path: HARNESS_PATH, fixtureKey: key }) as ProductPreparedZipAdmissionProof

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
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.proveTransferJobPreparedZip()
  }, HARNESS_PATH) as TransferJobPreparedZipProof

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
  expect(result.transferTraceNames).toContain('receive.materialization.completed')
  expect(result.transferTraceNames).not.toContain('receive.materialization.failed')
})

test('reopens workspace admission authority from a fresh page', async ({ page }) => {
  const key = crypto.randomUUID()
  const cut = await page.evaluate(async ({ path, fixtureKey }) => {
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.createFreshPageWorkspaceResumeCut(fixtureKey)
  }, { path: HARNESS_PATH, fixtureKey: key }) as FreshPageWorkspaceResumeCut
  expect(cut.lifecycle).toBe('resumable-receive')

  await page.reload()
  const reopened = await page.evaluate(async ({ path, fixture }) => {
    const harness = await import(path) as typeof import('./durable-recovery-harness')
    return harness.reopenFreshPageWorkspaceResume(fixture)
  }, {
    path: HARNESS_PATH,
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
  }, { path: HARNESS_PATH, fixtureKey: key }) as ReceiveCrashCutResult
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
    path: HARNESS_PATH,
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
    path: HARNESS_PATH,
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
