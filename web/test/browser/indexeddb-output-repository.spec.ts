import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'

import { requireOriginPrivateStorage } from './browser-storage-support'

const PROBE_PATH = '/test/browser/indexeddb-output-repository-probe.ts'

test.beforeEach(async ({ browserName, page }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
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
    candidateCountBeforePending: '0',
    candidateCountWithPending: '1',
    retiredRows: 17,
  })
})

test('faults after final checkpoint write roll back cleanly', async ({ page }) => {
  const result = await page.evaluate(async (path) => {
    const probe = await import(path) as typeof import('./indexeddb-output-repository-probe')
    return probe.probeIndexedDbFinalFaultCuts(`output-fault-${crypto.randomUUID()}`)
  }, PROBE_PATH)

  expect(result).toEqual(['final-checkpoint'])
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

test('incompatible metadata resets without touching files or current records and rejects blocked upgrades', async ({ page }) => {
  const result = await page.evaluate(async (path) => {
    const probe = await import(path) as typeof import('./indexeddb-output-repository-probe')
    return probe.probeIndexedDbOutputReset(`output-migration-${crypto.randomUUID()}`)
  }, PROBE_PATH)

  expect(result).toEqual({
    oldRowsRemaining: 0,
    storeCount: 12,
    exactIndexesPresent: true,
    publishedSentinelBytes: [91, 92, 93, 94],
    blockedUpgradeRejected: true,
    currentRowsPreserved: true,
  })
})

for (const protocol of ['semantic', 'candidate'] as const) test(`${protocol} metadata admission survives reopen and preserves delivery history`, async ({ page }) => {
  const result = await page.evaluate(async ({ path, protocol }) => {
    const probe = await import(path) as typeof import('./indexeddb-output-repository-probe')
    return probe.probeMetadataResume(`metadata-resume-${crypto.randomUUID()}`, protocol)
  }, { path: PROBE_PATH, protocol })
  expect(result).toEqual({
    classification: 'metadata-match', sameRecord: true, sameObject: true,
    persisted: true, retry: true, staleRejected: true, historicalRejected: true,
    historyIntact: true, retainedBytes: [1, 2, 3, 4], fileBytes: [1, 2, 3, 4, 5, 6, 7, 8],
  })
})
