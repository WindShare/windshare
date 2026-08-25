import { mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { expect, test, type BrowserContext } from '@playwright/test'

import {
  removePersistentBrowserProfile,
  requireOriginPrivateStorage,
} from './browser-storage-support'
import type {
  DurablePackageFixture,
  DurableReceiveFixture,
  PublicationRetryResult,
  ReceiveCrashCutResult,
  RecoveredPackageResult,
} from './durable-recovery-harness'
import type {
  DirectTreeProcessCrashCut,
  DirectTreeProcessRecoveryFixture,
  DirectTreeProcessRecoveryProof,
} from './direct-tree-recovery-periodic-fixture'

const HARNESS_PATH = '/test/browser/durable-recovery-harness.ts'
const DIRECT_TREE_FIXTURE_PATH = '/test/browser/direct-tree-recovery-periodic-fixture.ts'

test('receive, package, and retained publication survive fresh browser processes', async ({
  browser,
  browserName,
}, testInfo) => {
  if (!browser.isConnected()) throw new Error('Playwright worker browser is unavailable')
  const storageOrigin = testInfo.project.use.baseURL
  if (typeof storageOrigin !== 'string') throw new Error('Durable recovery test requires baseURL')
  const profile = await mkdtemp(join(tmpdir(), 'windshare-w3c-durable-'))
  const browserType = browser.browserType()
  expect(browserType.name()).toBe(browserName)
  const key = crypto.randomUUID()
  try {
    const firstContext = await browserType.launchPersistentContext(profile, { headless: true })
    const firstPage = firstContext.pages()[0] ?? await firstContext.newPage()
    await firstPage.goto(storageOrigin)
    await requireOriginPrivateStorage(firstPage, browserName)
    const crashCut = await firstPage.evaluate(async ({ path, fixtureKey }) => {
      const harness = await import(path) as typeof import('./durable-recovery-harness')
      return harness.createOriginPrivateReceiveCrashCut(fixtureKey)
    }, { path: HARNESS_PATH, fixtureKey: key }) as ReceiveCrashCutResult
    expect(crashCut.ranges).toEqual(['0:3'])
    await firstContext.close()

    const secondContext = await browserType.launchPersistentContext(profile, { headless: true })
    const secondPage = secondContext.pages()[0] ?? await secondContext.newPage()
    await secondPage.goto(storageOrigin)
    await requireOriginPrivateStorage(secondPage, browserName)
    const recovered = await secondPage.evaluate(async ({ path, fixture }) => {
      const harness = await import(path) as typeof import('./durable-recovery-harness')
      return harness.recoverReceiveAndSealPackage(fixture)
    }, {
      path: HARNESS_PATH,
      fixture: crashCut.fixture as DurableReceiveFixture,
    }) as RecoveredPackageResult
    expect(recovered).toMatchObject({
      recoveredRanges: ['0:3'],
      packageBytes: [1, 2, 3, 4, 5],
      lifecycle: 'waiting-to-save',
    })
    await secondContext.close()

    const thirdContext = await browserType.launchPersistentContext(profile, { headless: true })
    const thirdPage = thirdContext.pages()[0] ?? await thirdContext.newPage()
    await thirdPage.goto(storageOrigin)
    await requireOriginPrivateStorage(thirdPage, browserName)
    const retried = await thirdPage.evaluate(async ({ path, fixture }) => {
      const harness = await import(path) as typeof import('./durable-recovery-harness')
      return harness.retryRetainedPackagePublication(fixture)
    }, {
      path: HARNESS_PATH,
      fixture: recovered.fixture as DurablePackageFixture,
    }) as PublicationRetryResult
    expect(retried).toMatchObject({
      packageDigest: recovered.fixture.package.digest,
      originalExpiry: recovered.fixture.originalExpiry,
      restoredExpiry: recovered.fixture.originalExpiry,
      contentRequests: '0',
      packageSeals: 0,
      cleanup: 'clean',
    })
    await thirdContext.close()
  } finally {
    await removePersistentBrowserProfile(profile)
  }
})

test('DirectTree recovery preserves only verified work across a Chromium process termination', async ({
  browser,
  browserName,
}, testInfo) => {
  if (!browser.isConnected()) throw new Error('Playwright worker browser is unavailable')
  const storageOrigin = testInfo.project.use.baseURL
  if (typeof storageOrigin !== 'string') throw new Error('DirectTree recovery test requires baseURL')
  const profile = await mkdtemp(join(tmpdir(), 'windshare-direct-tree-recovery-'))
  const browserType = browser.browserType()
  const key = crypto.randomUUID()
  let activeContext: BrowserContext | undefined
  try {
    const firstContext = await browserType.launchPersistentContext(profile, { headless: true })
    activeContext = firstContext
    const firstPage = firstContext.pages()[0] ?? await firstContext.newPage()
    await firstPage.goto(storageOrigin)
    await requireOriginPrivateStorage(firstPage, browserName)
    const crashCut = await firstPage.evaluate(async ({ path, fixtureKey }) => {
      const fixture = await import(path) as typeof import(
        './direct-tree-recovery-periodic-fixture'
      )
      return fixture.createDirectTreeProcessCrashCut(fixtureKey)
    }, {
      path: DIRECT_TREE_FIXTURE_PATH,
      fixtureKey: key,
    }) as DirectTreeProcessCrashCut
    expect(crashCut).toEqual({
      fixture: crashCut.fixture,
      storageEvidence: {
        backingStore: 'opfs',
        role: 'direct-tree-contract-surrogate',
        nativeNtfsPerformanceClaim: false,
      },
      lifecycle: 'receiving',
      pendingAcceptedRange: '0:2',
      checkpoints: [
        { file: 'completed.bin', ranges: ['0:4'], complete: true },
        { file: 'large.bin', ranges: ['0:4'], complete: false },
        { file: 'pending-small.bin', ranges: [], complete: false },
      ],
    })

    // Closing a launchPersistentContext terminates its dedicated Chromium process.
    // No page reload or graceful output-session close is allowed across this cut.
    await firstContext.close()
    activeContext = undefined

    const secondContext = await browserType.launchPersistentContext(profile, { headless: true })
    activeContext = secondContext
    const secondPage = secondContext.pages()[0] ?? await secondContext.newPage()
    await secondPage.goto(storageOrigin)
    await requireOriginPrivateStorage(secondPage, browserName)
    const recovered = await secondPage.evaluate(async ({ path, fixture: recoveryFixture }) => {
      const fixture = await import(path) as typeof import(
        './direct-tree-recovery-periodic-fixture'
      )
      return fixture.recoverDirectTreeAfterProcessTermination(recoveryFixture)
    }, {
      path: DIRECT_TREE_FIXTURE_PATH,
      fixture: crashCut.fixture as DirectTreeProcessRecoveryFixture,
    }) as DirectTreeProcessRecoveryProof
    expect(recovered).toEqual({
      storageEvidence: {
        backingStore: 'opfs',
        role: 'direct-tree-contract-surrogate',
        nativeNtfsPerformanceClaim: false,
      },
      lifecycle: {
        beforeRecovery: 'receiving',
        recoveryDecision: 'resume-receive',
        afterRecovery: 'resumable-receive',
        afterResume: 'receiving',
        afterSettlement: 'published',
        durableAfterSettlement: 'published',
      },
      workerStatus: 'Succeeded',
      revisionRequests: ['completed.bin', 'large.bin', 'pending-small.bin'],
      durableRangesBeforeFetch: [
        { file: 'completed.bin', ranges: ['0:4'] },
        { file: 'large.bin', ranges: ['0:4'] },
        { file: 'pending-small.bin', ranges: [] },
      ],
      contentRangeRequests: [
        { file: 'large.bin', range: '4:8' },
        { file: 'pending-small.bin', range: '0:4' },
      ],
      physicalFiles: [
        { file: 'completed.bin', bytes: [7, 7, 7, 7] },
        { file: 'large.bin', bytes: [7, 7, 7, 7, 7, 7, 7, 7] },
        { file: 'pending-small.bin', bytes: [7, 7, 7, 7] },
      ],
    })
    await secondContext.close()
    activeContext = undefined
  } finally {
    await activeContext?.close().catch(() => undefined)
    await removePersistentBrowserProfile(profile)
  }
})
