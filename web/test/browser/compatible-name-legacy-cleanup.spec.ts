import { expect, test } from '@playwright/test'

test('obsolete restoration records stay cleanup-only and forgetting preserves downloaded files', async ({ page }) => {
  await page.goto('/')
  const result = await page.evaluate(async () => {
    const path = '/test/browser/compatible-name-legacy-cleanup-probe.ts'
    const probe = await import(path) as typeof import('./compatible-name-legacy-cleanup-probe')
    return probe.probeLegacyCompatibleNameCleanup('old-repair-' + crypto.randomUUID())
  })
  expect(result).toEqual({
    continuation: 'cleanup-incompatible',
    staleRejected: true,
    busyRejected: true,
    result: { kind: 'record-forgotten' },
    remainingOperations: 0,
    onlyForeignRowsRemain: true,
    fileContent: 'previously downloaded content',
  })
})
