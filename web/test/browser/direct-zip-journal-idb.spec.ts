import { expect, test } from '@playwright/test'

import { CHECKPOINT_DATABASE_VERSION } from '../../src/output/browser/indexeddb-database'

const PROBE_PATH = '/test/browser/direct-zip-journal-idb-probe.ts'

test.beforeEach(async ({ page }) => {
  await page.goto('/')
})

test('Direct ZIP bootstrap cut rolls back every new authority row on a late store fault', async ({
  page,
}) => {
  const result = await page.evaluate(async ({ name, path }) => {
    const probe = await import(path) as typeof import('./direct-zip-journal-idb-probe')
    return probe.probeDirectZipBootstrapAtomicity(name)
  }, { name: `direct-zip-atomic-${crypto.randomUUID()}`, path: PROBE_PATH })

  expect(result.failureName).not.toBe('none')
  expect(result.rolledBackCandidateCount).toBe(0)
  expect(result.rolledBackLeaseCount).toBe(0)
  expect(result.candidateDigest).toBeDefined()
  expect(result.leaseGeneration).toBe('2')
  expect(result.staleLeaseFailure).toBe('InvalidStateError')
  expect(result.startupCandidateDigests).toEqual([result.candidateDigest])
})

test('current migration removes incompatible receive authority and starts Direct ZIP empty', async ({
  page,
}) => {
  const result = await page.evaluate(async ({ name, path }) => {
    const probe = await import(path) as typeof import('./direct-zip-journal-idb-probe')
    return probe.probeIndexedDbCurrentMigration(name)
  }, { name: `direct-zip-migration-${crypto.randomUUID()}`, path: PROBE_PATH })

  expect(result).toEqual({
    version: CHECKPOINT_DATABASE_VERSION,
    legacyStoresPresent: [false, false, false, false],
    currentReceiveCounts: [0, 0, 0],
    invalidatedAuthorityCounts: [0, 0, 0, 0],
    directZipStoresPresent: [true, true, true, true, true],
  })
})

test('page staging, candidate binding, checkpoint promotion, and lifecycle share fenced cuts', async ({
  page,
}) => {
  const result = await page.evaluate(async ({ name, path }) => {
    const probe = await import(path) as typeof import('./direct-zip-journal-idb-probe')
    return probe.probeDirectZipJournalPromotion(name)
  }, { name: `direct-zip-promotion-${crypto.randomUUID()}`, path: PROBE_PATH })

  expect(result).toEqual({
    checkpointGeneration: '4',
    atomicCandidatePageFence: true,
    freshObservationBound: true,
    gatedRecoveryDigest: expect.any(String),
    gateCleared: true,
    candidatePresent: false,
    pageCount: 1,
    orphanPagesDeleted: '1',
    staleFenceFailure: 'InvalidStateError',
    leaseReplaced: true,
  })
})
