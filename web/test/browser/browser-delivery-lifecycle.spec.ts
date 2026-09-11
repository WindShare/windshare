import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from './browser-storage-support'

const PROBE = '/test/browser/browser-delivery-lifecycle-probe.ts'

test('local target checkpoint crash cuts reopen with exact lifecycle authority and usable Downloads inventory', async ({ page }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const databaseName = 'delivery-lifecycle-' + crypto.randomUUID()
  expect(await page.evaluate(async ({ path, databaseName }) => {
    const probe = await import(path) as typeof import('./browser-delivery-lifecycle-probe')
    return probe.prepareLocalLifecycleCrash(databaseName)
  }, { path: PROBE, databaseName })).toBe('7')
  await page.reload()
  expect(await page.evaluate(async ({ path, databaseName }) => {
    const probe = await import(path) as typeof import('./browser-delivery-lifecycle-probe')
    return probe.reopenLocalLifecycleCrash(databaseName)
  }, { path: PROBE, databaseName })).toEqual({
    partialGeneration: '8', partialBytes: '3', retainedAction: 'save-staged-files',
    unchangedGeneration: '8', busySummaryAbsent: true,
  })
  await page.reload()
  expect(await page.evaluate(async ({ path, databaseName }) => {
    const probe = await import(path) as typeof import('./browser-delivery-lifecycle-probe')
    return probe.finishLocalLifecycleCrash(databaseName)
  }, { path: PROBE, databaseName })).toEqual({
    generation: '9', completedBytes: '8', completedFiles: '1', idempotent: true,
  })
})
