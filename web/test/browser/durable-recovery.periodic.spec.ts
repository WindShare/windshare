import { mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { expect, test } from '@playwright/test'

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

const HARNESS_PATH = '/test/browser/durable-recovery-harness.ts'

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
