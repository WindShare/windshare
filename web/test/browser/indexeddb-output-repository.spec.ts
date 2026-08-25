import { expect, test } from '@playwright/test'

import { requireOriginPrivateStorage } from './browser-storage-support'

const PROBE_PATH = '/test/browser/indexeddb-output-repository-probe.ts'

test.beforeEach(async ({ browserName, page }) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
})

test('semantic output repository is atomic, indexed, concurrent, and genuinely paged', async ({
  page,
}) => {
  const result = await page.evaluate(async (path) => {
    const probe = await import(path) as typeof import('./indexeddb-output-repository-probe')
    return probe.probeIndexedDbOutputRepository(`output-repository-${crypto.randomUUID()}`)
  }, PROBE_PATH)

  expect(result).toEqual({
    concurrentFinals: 8,
    idempotentRetry: 'idempotent',
    indexedClassification: 'exact',
    operationWideGetAllCalls: 0,
    firstPageEntries: 128,
    secondPageEntries: 10,
    stableDirectoryRetry: 'idempotent',
    sealPages: '2',
    candidateSealRejected: true,
    pathConflictRejected: true,
    retiredRows: expect.any(Number),
  })
  expect(result.retiredRows).toBeGreaterThan(138)
})

test('faults after every final write roll back the checkpoint/proof/entry unit', async ({ page }) => {
  const result = await page.evaluate(async (path) => {
    const probe = await import(path) as typeof import('./indexeddb-output-repository-probe')
    return probe.probeIndexedDbFinalFaultCuts(`output-fault-${crypto.randomUUID()}`)
  }, PROBE_PATH)

  expect(result).toEqual(['final-checkpoint', 'final-proof', 'final-ledger-entry'])
})

test('final commit rejects candidates, stale predecessors, and foreign handles', async ({ page }) => {
  const result = await page.evaluate(async (path) => {
    const probe = await import(path) as typeof import('./indexeddb-output-repository-probe')
    return probe.probeIndexedDbFinalRejections(`output-rejection-${crypto.randomUUID()}`)
  }, PROBE_PATH)

  expect(result).toEqual({
    candidateRejected: true,
    stalePredecessorRejected: true,
    foreignHandleRejected: true,
  })
})

test('explicit owned-file restart is atomic, idempotent, and metadata-only', async ({ page }) => {
  const result = await page.evaluate(async (path) => {
    const probe = await import(path) as typeof import('./indexeddb-output-repository-probe')
    return probe.probeIndexedDbOwnedFileRestart(`output-restart-${crypto.randomUUID()}`)
  }, PROBE_PATH)

  expect(result).toEqual({
    first: 'restart',
    ambiguousRetry: 'idempotent',
    staleRejected: true,
    foreignHandleRejected: true,
    fileBytes: [1, 2, 3, 4, 5, 6, 7, 8],
  })
})

test('v10 migration clears only origin metadata and fails closed when blocked', async ({ page }) => {
  const result = await page.evaluate(async (path) => {
    const probe = await import(path) as typeof import('./indexeddb-output-repository-probe')
    return probe.probeIndexedDbOutputMigration(`output-migration-${crypto.randomUUID()}`)
  }, PROBE_PATH)

  expect(result).toEqual({
    oldRowsRemaining: 0,
    storeCount: 19,
    exactIndexesPresent: true,
    publishedSentinelBytes: [91, 92, 93, 94],
    blockedUpgradeRejected: true,
  })
})
