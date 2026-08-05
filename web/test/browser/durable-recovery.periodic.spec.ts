import { mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { expect, test, type Page } from '@playwright/test'

import {
  removePersistentBrowserProfile,
  requireOriginPrivateStorage,
} from './browser-storage-support'

interface DurableRecoveryHarness {
  createCheckpoint(outputSessionId: string): Promise<readonly string[]>
  reopenCheckpoint(outputSessionId: string): Promise<{
    readonly ranges: readonly string[]
    readonly coversPrefix: boolean
    readonly durability: string
  }>
  createPersistentHandleCheckpoint(outputSessionId: string): Promise<readonly string[]>
  reopenPersistentHandleCheckpoint(outputSessionId: string): Promise<readonly string[]>
  createCrashCut(outputSessionId: string, phase: CheckpointCrashPhase): Promise<boolean>
  holdOutputSession(outputSessionId: string): Promise<void>
  competingSessionError(outputSessionId: string): Promise<string | undefined>
  releaseOutputSession(outputSessionId: string): Promise<void>
  completePersistentHandleOutput(outputSessionId: string): Promise<{
    readonly bytes: readonly number[]
    readonly metadataRetired: boolean
  }>
  completeOriginPrivateOutput(outputSessionId: string): Promise<{
    readonly exported: readonly number[]
    readonly reopenedRanges: readonly string[]
  }>
}

type CheckpointCrashPhase =
  | 'DataWritten'
  | 'DataFlushed'
  | 'JournalWritten'
  | 'JournalFlushed'
  | 'CheckpointCommitted'
  | 'CheckpointVerified'

const HARNESS_PATH = '/test/browser/durable-recovery-harness.ts'

test.beforeEach(async ({ browserName, page }) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
})

test('OPFS data and committed journal survive a fresh browser process', async ({
  browser,
  browserName,
}, testInfo) => {
  if (!browser.isConnected()) throw new Error('Playwright worker browser is unavailable')
  const storageOrigin = testInfo.project.use.baseURL
  if (typeof storageOrigin !== 'string') throw new Error('Durable recovery test requires baseURL')
  const profile = await mkdtemp(join(tmpdir(), 'windshare-durable-output-'))
  const browserType = browser.browserType()
  expect(browserType.name()).toBe(browserName)
  const outputSessionId = `process-${crypto.randomUUID()}`
  try {
    const firstContext = await browserType.launchPersistentContext(profile, { headless: true })
    const firstPage = firstContext.pages()[0] ?? await firstContext.newPage()
    await firstPage.goto(storageOrigin)
    expect(await createCheckpoint(firstPage, outputSessionId)).toEqual(['0:3'])
    await firstContext.close()

    const secondContext = await browserType.launchPersistentContext(profile, { headless: true })
    const secondPage = secondContext.pages()[0] ?? await secondContext.newPage()
    await secondPage.goto(storageOrigin)
    expect(await reopenCheckpoint(secondPage, outputSessionId)).toEqual({
      ranges: ['0:3'],
      coversPrefix: true,
      durability: 'ProcessRestart',
    })
    await secondContext.close()
  } finally {
    await removePersistentBrowserProfile(profile)
  }
})

test('real OPFS and IndexedDB crash cuts publish only atomically installed checkpoints', async ({
  browser,
  browserName,
}, testInfo) => {
  if (!browser.isConnected()) throw new Error('Playwright worker browser is unavailable')
  const storageOrigin = testInfo.project.use.baseURL
  if (typeof storageOrigin !== 'string') throw new Error('Durable recovery test requires baseURL')
  const profile = await mkdtemp(join(tmpdir(), 'windshare-durable-cuts-'))
  const browserType = browser.browserType()
  expect(browserType.name()).toBe(browserName)
  const phases: readonly CheckpointCrashPhase[] = [
    'DataWritten',
    'DataFlushed',
    'JournalWritten',
    'JournalFlushed',
    'CheckpointCommitted',
    'CheckpointVerified',
  ]
  const sessions = new Map<CheckpointCrashPhase, string>()
  try {
    const firstContext = await browserType.launchPersistentContext(profile, { headless: true })
    const firstPage = firstContext.pages()[0] ?? await firstContext.newPage()
    await firstPage.goto(storageOrigin)
    for (const phase of phases) {
      const outputSessionId = `cut-${phase}-${crypto.randomUUID()}`
      sessions.set(phase, outputSessionId)
      expect(await createCrashCut(firstPage, outputSessionId, phase)).toBe(true)
    }
    await firstContext.close()

    const secondContext = await browserType.launchPersistentContext(profile, { headless: true })
    const secondPage = secondContext.pages()[0] ?? await secondContext.newPage()
    await secondPage.goto(storageOrigin)
    for (const phase of phases) {
      const outputSessionId = sessions.get(phase)
      if (outputSessionId === undefined) throw new Error('Crash-cut session identity is missing')
      const reopened = await reopenCheckpoint(secondPage, outputSessionId)
      expect(reopened.ranges).toEqual(
        phase === 'CheckpointCommitted' || phase === 'CheckpointVerified'
          ? ['0:3']
          : [],
      )
    }
    await secondContext.close()
  } finally {
    await removePersistentBrowserProfile(profile)
  }
})

test('a persisted directory handle and checkpoint survive a fresh browser process', async ({
  browser,
  browserName,
}, testInfo) => {
  if (!browser.isConnected()) throw new Error('Playwright worker browser is unavailable')
  const storageOrigin = testInfo.project.use.baseURL
  if (typeof storageOrigin !== 'string') throw new Error('Durable recovery test requires baseURL')
  const profile = await mkdtemp(join(tmpdir(), 'windshare-durable-handle-'))
  const browserType = browser.browserType()
  expect(browserType.name()).toBe(browserName)
  const outputSessionId = `process-handle-${crypto.randomUUID()}`
  try {
    const firstContext = await browserType.launchPersistentContext(profile, { headless: true })
    const firstPage = firstContext.pages()[0] ?? await firstContext.newPage()
    await firstPage.goto(storageOrigin)
    expect(await callHarness<readonly string[]>(
      firstPage,
      outputSessionId,
      'createPersistentHandleCheckpoint',
    )).toEqual(['0:3'])
    await firstContext.close()

    const secondContext = await browserType.launchPersistentContext(profile, { headless: true })
    const secondPage = secondContext.pages()[0] ?? await secondContext.newPage()
    await secondPage.goto(storageOrigin)
    expect(await callHarness<readonly string[]>(
      secondPage,
      outputSessionId,
      'reopenPersistentHandleCheckpoint',
    )).toEqual(['0:3'])
    await secondContext.close()
  } finally {
    await removePersistentBrowserProfile(profile)
  }
})

async function createCheckpoint(page: Page, outputSessionId: string): Promise<readonly string[]> {
  return page.evaluate(async ({ path, sessionId }) => {
    const harness = (await import(path)) as DurableRecoveryHarness
    return harness.createCheckpoint(sessionId)
  }, { path: HARNESS_PATH, sessionId: outputSessionId })
}

async function reopenCheckpoint(
  page: Page,
  outputSessionId: string,
): Promise<Awaited<ReturnType<DurableRecoveryHarness['reopenCheckpoint']>>> {
  return page.evaluate(async ({ path, sessionId }) => {
    const harness = (await import(path)) as DurableRecoveryHarness
    return harness.reopenCheckpoint(sessionId)
  }, { path: HARNESS_PATH, sessionId: outputSessionId })
}

async function createCrashCut(
  page: Page,
  outputSessionId: string,
  phase: CheckpointCrashPhase,
): Promise<boolean> {
  return page.evaluate(async ({ path, sessionId, cut }) => {
    const harness = (await import(path)) as DurableRecoveryHarness
    return harness.createCrashCut(sessionId, cut)
  }, { path: HARNESS_PATH, sessionId: outputSessionId, cut: phase })
}

async function callHarness<T>(
  page: Page,
  outputSessionId: string,
  operation: keyof DurableRecoveryHarness,
): Promise<T> {
  return page.evaluate(async ({ path, sessionId, method }) => {
    const harness = (await import(path)) as DurableRecoveryHarness
    const call = harness[method] as (id: string) => Promise<unknown>
    return call(sessionId) as Promise<T>
  }, { path: HARNESS_PATH, sessionId: outputSessionId, method: operation })
}
