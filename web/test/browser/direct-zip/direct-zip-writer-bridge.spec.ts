import { expect, test } from '@playwright/test'

test('Direct ZIP writer persists bootstrap, partial-member recovery, and closing as fenced cuts', async ({ page }) => {
  await page.goto('/')
  const result = await page.evaluate(async name => {
    const path = '/test/browser/direct-zip/direct-zip-writer-bridge-probe.ts'
    const probe = await import(path) as typeof import('./direct-zip-writer-bridge-probe')
    return probe.probeDirectZipWriterBridge(name)
  }, 'direct-zip-writer-' + crypto.randomUUID())
  expect(result).toMatchObject({
    bootstrapFault: true, layoutCountAfterFault: 0, bootstrapStateAbsent: true,
    retiredBeforePublish: true, promotionFailed: true, candidateDurable: true, resumedOffset: '3',
    storedCompletion: true, epochCount: 4, staleClosingRejected: true,
    prematurePublicationRejected: true, publishedCommitted: true,
  })
  expect(BigInt(result.completionBytes)).toBeGreaterThan(6n)
})
