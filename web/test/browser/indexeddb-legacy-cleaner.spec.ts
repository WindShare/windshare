import { expect, test } from '@playwright/test'

import { requireOriginPrivateStorage } from './browser-storage-support'

const PROBE_PATH = '/test/browser/indexeddb-legacy-cleaner-probe.ts'
const LEGACY_RECORD_COUNT = 16
const CURRENT_V9_STORE_COUNT = 15

test.beforeEach(async ({ browserName, page }) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
})

test('legacy IndexedDB cleanup preserves current state and published output', async ({ page }) => {
  const result = await page.evaluate(async (path) => {
    const probe = await import(path) as typeof import('./indexeddb-legacy-cleaner-probe')
    return probe.probeIndexedDbLegacyCleanupIsolation(
      `legacy-cleanup-${crypto.randomUUID()}`,
    )
  }, PROBE_PATH)

  expect(result).toEqual({
    first: { status: 'completed', removed: LEGACY_RECORD_COUNT },
    second: { status: 'nothing-to-clean', removed: 0 },
    legacyCounts: [0, 0, 0, 0, 0, 0, 0, 0],
    currentStoreCount: CURRENT_V9_STORE_COUNT,
    currentSentinelsPreserved: true,
    publishedSentinelBytes: [17, 34, 51, 68],
  })
})

test('concurrent legacy IndexedDB cleanup callers serialize one durable pass', async ({
  context,
  page,
}) => {
  const competitor = await context.newPage()
  const databaseName = `legacy-cleanup-race-${crypto.randomUUID()}`
  try {
    await competitor.goto('/')
    await page.evaluate(async ({ name, path }) => {
      const probe = await import(path) as typeof import('./indexeddb-legacy-cleaner-probe')
      await probe.seedIndexedDbLegacyCleanup(name)
    }, { name: databaseName, path: PROBE_PATH })
    const reports = await Promise.all([page, competitor].map((target) => target.evaluate(
      async ({ name, path }) => {
        const probe = await import(path) as typeof import('./indexeddb-legacy-cleaner-probe')
        return probe.runIndexedDbLegacyCleanup(name)
      },
      { name: databaseName, path: PROBE_PATH },
    )))
    expect(reports).toEqual(expect.arrayContaining([
      { status: 'completed', removed: LEGACY_RECORD_COUNT },
      { status: 'nothing-to-clean', removed: 0 },
    ]))
    expect(await page.evaluate(async ({ name, path }) => {
      const probe = await import(path) as typeof import('./indexeddb-legacy-cleaner-probe')
      return probe.legacyStoreCounts(name)
    }, { name: databaseName, path: PROBE_PATH })).toEqual([0, 0, 0, 0, 0, 0, 0, 0])
  } finally {
    try {
      await competitor.close()
    } finally {
      await page.evaluate(async ({ name, path }) => {
        const probe = await import(path) as typeof import('./indexeddb-legacy-cleaner-probe')
        await probe.deleteIndexedDbLegacyCleanupDatabase(name)
      }, { name: databaseName, path: PROBE_PATH })
    }
  }
})
