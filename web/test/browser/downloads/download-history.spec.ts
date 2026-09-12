import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from '../contract-host'

const PROBE_MODULE = '/test/browser/downloads/download-history-probe.ts'

test('restores identifiable Downloads after refresh and consumes only original history tokens', async ({ page }) => {
  const key = 'refresh-' + Date.now()
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await page.evaluate(async ({ modulePath, key }) => {
    const probe = await import(modulePath) as typeof import('./download-history-probe')
    await probe.seedDownloadHistory(key)
  }, { modulePath: PROBE_MODULE, key })
  await page.reload()
  const result = await page.evaluate(async ({ modulePath, key }) => {
    const probe = await import(modulePath) as typeof import('./download-history-probe')
    return probe.inspectAndForgetDownloadHistory(key)
  }, { modulePath: PROBE_MODULE, key })
  expect(result).toEqual({
    labels: ['Holiday photos', 'Holiday photos'],
    destinations: ['Browser downloads', 'Browser downloads'],
    times: [1001, 1000], distinctIdentities: true, sameShareIsNotAssumed: true,
    continuations: ['history-only', 'history-only'], actions: [['forget'], ['forget']],
    copiedRejected: true, remaining: 1,
  })
})

test('a saved-looking label never grants permission to forget unfinished output', async ({ page }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const result = await page.evaluate(async (modulePath) => {
    const probe = await import(modulePath) as typeof import('./download-history-probe')
    return probe.rejectUnfinishedHistoryRemoval('unfinished-' + Date.now())
  }, PROBE_MODULE)
  expect(result).toEqual({ rejected: true, retained: true })
})
