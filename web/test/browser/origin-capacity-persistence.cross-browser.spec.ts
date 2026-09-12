import { expect, test, type Page } from '@playwright/test'

const HARNESS_PATH = '/test/browser/opfs/capacity-persistence-harness.ts'
type Harness = typeof import('./opfs/capacity-persistence-harness')

async function run(page: Page, scenario: 'incompatibleSchema' | 'expiryRollback' | 'settlementRollback') {
  await page.goto('/test/browser/contract-host.html')
  return page.evaluate(async ({ path, scenario }) => (await import(path) as Harness)[scenario](),
    { path: HARNESS_PATH, scenario })
}

test('rejects incompatible schema without deleting old accounting; an explicit reset creates a clean database', async ({ page }) => {
  expect(await run(page, 'incompatibleSchema')).toEqual({ failure: 'DataError', version: 3, preserved: 1, fresh: 0 })
})

for (const kind of ['legacy', 'nan'] as const) {
  test(`rejects ${kind} workspace rows in ZIP admission, release, and staged-file admission without changing them`, async ({ page }) => {
    await page.goto('/test/browser/contract-host.html')
    const result = await page.evaluate(async ({ path, kind }) => {
      return (await import(path) as Harness).corruptWorkspace(kind)
    }, { path: HARNESS_PATH, kind })
    expect(result).toEqual({
      claim: 'DataError', release: 'DataError', stage: 'DataError', entered: false, rows: 1, preserved: true,
    })
  })
}

test('rolls back expired-lease normalization if staged inventory is invalid', async ({ page }) => {
  expect(await run(page, 'expiryRollback')).toEqual({
    failure: 'DataError', rows: 1, token: 'old', occupied: '10', outstanding: '20', expires: 1,
  })
})

test('rolls back a queued object settlement when account arithmetic fails', async ({ page }) => {
  expect(await run(page, 'settlementRollback')).toEqual({ failure: 'RangeError', unchanged: true })
})
