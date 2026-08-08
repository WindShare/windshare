import { expect, test, type Page } from '@playwright/test'

import { requireOriginPrivateStorage } from './browser-storage-support'
import type {
  FsaNamespaceFixture,
  FsaNamespaceOwnership,
} from './fsa-namespace-atomicity-harness'

const HARNESS_PATH = '/test/browser/fsa-namespace-atomicity-harness.ts'
const TEST_ROOT_IDENTITY = 'GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
const FIRST_TEST_INTENT = 'HAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
const SECOND_TEST_INTENT = 'IAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'

interface NamespaceHarness {
  holdFirstIntent(fixture: FsaNamespaceFixture): Promise<FsaNamespaceOwnership>
  probeCompetingIntent(
    fixture: FsaNamespaceFixture,
  ): Promise<{ readonly name: string; readonly scope?: string }>
  verifyRecoveryAndOwnership(
    fixture: FsaNamespaceFixture,
    ownership: FsaNamespaceOwnership,
  ): Promise<{
    readonly fileCreateError: string
    readonly directoryCreated: boolean
    readonly fileRemoveError: string
    readonly directoryRemoveError: string
    readonly filePreserved: boolean
    readonly directoryPreserved: boolean
  }>
}

test('FSA root authority spans intents and tabs, then recovers after refresh', async ({
  browserName,
  context,
  page,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  const competitor = await context.newPage()
  await competitor.goto('/')
  await requireOriginPrivateStorage(competitor, browserName)
  const nonce = crypto.randomUUID()
  const fixture = Object.freeze({
    databaseName: `fsa-namespace-${nonce}`,
    rootName: `fsa-namespace-${nonce}`,
    rootIdentity: TEST_ROOT_IDENTITY,
    firstIntent: FIRST_TEST_INTENT,
    secondIntent: SECOND_TEST_INTENT,
  })

  const ownership = await callHarness<FsaNamespaceOwnership>(
    page,
    'holdFirstIntent',
    fixture,
  )
  expect(await callHarness(competitor, 'probeCompetingIntent', fixture)).toEqual({
    name: 'InvalidStateError',
    scope: 'file-system-root',
  })

  // Reload simulates losing the page-local opener; the browser releases both
  // Web Locks while IndexedDB retains the two independent intent namespaces.
  await page.reload()
  expect(await callHarness(
    competitor,
    'verifyRecoveryAndOwnership',
    fixture,
    ownership,
  )).toEqual({
    fileCreateError: 'InvalidModificationError',
    directoryCreated: false,
    fileRemoveError: 'InvalidStateError',
    directoryRemoveError: 'InvalidStateError',
    filePreserved: true,
    directoryPreserved: true,
  })
  await competitor.close()
})

async function callHarness<T>(
  page: Page,
  method: keyof NamespaceHarness,
  fixture: FsaNamespaceFixture,
  ownership?: FsaNamespaceOwnership,
): Promise<T> {
  return page.evaluate(async ({ path, operation, value, created }) => {
    const harness = (await import(path)) as NamespaceHarness
    if (operation === 'holdFirstIntent') return harness.holdFirstIntent(value) as Promise<T>
    if (operation === 'probeCompetingIntent') return harness.probeCompetingIntent(value) as Promise<T>
    if (created === undefined) throw new Error('Namespace ownership is required')
    return harness.verifyRecoveryAndOwnership(value, created) as Promise<T>
  }, {
    path: HARNESS_PATH,
    operation: method,
    value: fixture,
    created: ownership,
  })
}
