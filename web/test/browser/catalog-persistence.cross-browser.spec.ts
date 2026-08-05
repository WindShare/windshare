import { expect, test } from '@playwright/test'

const PROBE_MODULE = '/test/browser/catalog-persistence-probe.ts'

test.beforeEach(async ({ page }) => {
  await page.goto('/')
})

test('rebuilds an incompatible local database without reusing legacy ownership', async ({ page }) => {
  const result = await page.evaluate(async (modulePath) => {
    const probe = await import(modulePath) as typeof import('./catalog-persistence-probe')
    return probe.probeIncompatibleDatabaseRebuild()
  }, PROBE_MODULE)

  expect(result).toEqual({
    oldOwnershipNotReused: true,
    rebuiltTransactionCommitted: true,
    rebuiltTransactionReopened: true,
  })
})
