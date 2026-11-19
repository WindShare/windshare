import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'
import { UNKNOWN_SPEED_STAGING_THRESHOLD_BYTES } from '../../src/output/planning/file-receiving-placement'

const PROBE_PATH = '/test/browser/browser-delivery-resume-probe.ts'

test('folder staging can resume and discard the same owned object after a source revision changes and the page reloads', async ({ page }) => {
  const fixture = { databaseName: 'delivery-resume-' + crypto.randomUUID(), parentName: 'delivery-resume-' + crypto.randomUUID() }
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const prepared = await page.evaluate(async ({ path, fixture }) => {
    const probe = await import(path) as typeof import('./browser-delivery-resume-probe')
    return probe.prepareBrowserDeliveryResume(fixture)
  }, { path: PROBE_PATH, fixture })
  expect(prepared.readmitted).toEqual([{ start: 0n, end: 3n }])
  expect(prepared.objectId).toBeTruthy()
  expect(prepared.reopenedObjectId).toBe(prepared.objectId)

  await page.reload()
  const { intent, objectId } = prepared
  const resumed = await page.evaluate(async ({ path, ...input }) => {
    const probe = await import(path) as typeof import('./browser-delivery-resume-probe')
    return probe.resumeAndDiscardBrowserDelivery(input)
  }, { path: PROBE_PATH, fixture, intent, objectId })
  expect(resumed).toEqual({
    readmitted: [{ start: 0n, end: 6n }],
    resumedObjectId: objectId,
    bytes: [1, 2, 3, 4, 5, 6, 7],
    reservedObjectId: objectId,
    reservedSize: UNKNOWN_SPEED_STAGING_THRESHOLD_BYTES,
    fileRemoved: true,
    handleRemoved: true,
    capacityReleased: true,
    state: 'discarded',
  })
})
