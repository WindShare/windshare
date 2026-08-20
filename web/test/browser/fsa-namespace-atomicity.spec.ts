import { expect, test, type Page } from '@playwright/test'

import { requireOriginPrivateStorage } from './browser-storage-support'
import type { FsaNamespaceFixture } from './fsa-namespace-atomicity-harness'

const HARNESS_PATH = '/test/browser/fsa-namespace-atomicity-harness.ts'

interface NamespaceHarness {
  exerciseTaskRootRestart(fixture: FsaNamespaceFixture): Promise<{
    firstCollisionIndex: number
    suffixPersisted: boolean
    rootIdentityPersisted: boolean
    newTaskIsolated: boolean
    directoryCount: number
  }>
  exerciseSingleFileLayout(fixture: FsaNamespaceFixture): Promise<{
    emptyBeforeRevision: boolean
    revisionOpenedBeforeCreation: boolean
    noExtraRoot: boolean
    prefixVisible: boolean
    restartReusedFile: boolean
    completedBytes: number
  }>
  holdTaskRoot(fixture: FsaNamespaceFixture): Promise<void>
  probeCompetingTask(fixture: FsaNamespaceFixture): Promise<{
    busy: boolean
    scope: string | null
  }>
  releaseHeldTask(fixture: FsaNamespaceFixture): Promise<void>
  exerciseFailedPreExecutionActivation(fixture: FsaNamespaceFixture): Promise<{
    rootAbsentBeforeExecution: boolean
    rootAbsentAfterDetach: boolean
  }>
}

test('FSA task roots keep suffix and ownership across restart', async ({ browserName, page }) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  const fixture = testFixture('task-root')
  expect(await callHarness(page, 'exerciseTaskRootRestart', fixture)).toEqual({
    firstCollisionIndex: 1,
    suffixPersisted: true,
    rootIdentityPersisted: true,
    newTaskIsolated: true,
    directoryCount: 3,
  })
})

test('single-file DirectoryTree has no extra root and resumes its visible prefix', async ({
  browserName,
  page,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  const fixture = testFixture('single-file')
  expect(await callHarness(page, 'exerciseSingleFileLayout', fixture)).toEqual({
    emptyBeforeRevision: true,
    revisionOpenedBeforeCreation: true,
    noExtraRoot: true,
    prefixVisible: true,
    restartReusedFile: true,
    completedBytes: 4,
  })
})

test('FSA parent Web Lock spans tabs', async ({ browserName, context, page }) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  const competitor = await context.newPage()
  await competitor.goto('/')
  await requireOriginPrivateStorage(competitor, browserName)
  const fixture = testFixture('cross-tab')
  await callHarness(page, 'holdTaskRoot', fixture)
  expect(await callHarness(competitor, 'probeCompetingTask', fixture)).toEqual({
    busy: true,
    scope: 'fsa-parent',
  })
  await callHarness(page, 'releaseHeldTask', fixture)
  await competitor.close()
})

test('failed activation before DirectTree execution leaves no visible task root', async ({
  browserName,
  page,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  expect(await callHarness(
    page,
    'exerciseFailedPreExecutionActivation',
    testFixture('pre-execution-failure'),
  )).toEqual({ rootAbsentBeforeExecution: true, rootAbsentAfterDetach: true })
})

function testFixture(label: string): FsaNamespaceFixture {
  const nonce = crypto.randomUUID()
  return Object.freeze({
    databaseName: `fsa-${label}-${nonce}`,
    parentName: `fsa-${label}-${nonce}`,
  })
}

async function callHarness<T>(
  page: Page,
  method: keyof NamespaceHarness,
  fixture: FsaNamespaceFixture,
): Promise<T> {
  return page.evaluate(async ({ path, operation, value }) => {
    const harness = (await import(path)) as NamespaceHarness
    return harness[operation](value) as Promise<T>
  }, { path: HARNESS_PATH, operation: method, value: fixture })
}
