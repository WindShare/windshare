import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'

const PROBE_PATH = '/test/browser/browser-delivery-staging-root-probe.ts'

test('staging creation receipt and final root coexist under the real unique owned-object index and recover after reload', async ({ page }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const input = { path: PROBE_PATH, databaseName: 'delivery-root-' + crypto.randomUUID(), parentName: 'delivery-root-' + crypto.randomUUID() }
  const prepared = await page.evaluate(async ({ path, databaseName, parentName }) => {
    const probe = await import(path) as typeof import('./browser-delivery-staging-root-probe')
    return probe.prepareStagingRootPromotion(databaseName, parentName)
  }, input)
  expect(prepared).toEqual({ retainedHandles: 2, uniqueOwnedObjects: 2 })
  await page.reload()
  const finished = await page.evaluate(async ({ path, databaseName, parentName }) => {
    const probe = await import(path) as typeof import('./browser-delivery-staging-root-probe')
    return probe.finishStagingRootPromotion(databaseName, parentName)
  }, input)
  expect(finished).toEqual({ retainedHandles: 1, finalRootObject: true })
})
