import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from '../browser-storage-support'

test('Direct ZIP writer recovers partial progress and commits the remaining content with its tail', async ({ page }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const result = await page.evaluate(async name => {
    const path = '/test/browser/direct-zip/direct-zip-writer-bridge-probe.ts'
    const probe = await import(path) as typeof import('./direct-zip-writer-bridge-probe')
    return probe.probeDirectZipWriterBridge(name)
  }, 'direct-zip-writer-' + crypto.randomUUID())
  expect(result).toMatchObject({
    bootstrapFault: true, layoutCountAfterFault: 0, bootstrapStateAbsent: true,
    retiredBeforePublish: true, promotionFailed: true, candidateDurable: true, resumedOffset: '3',
    storedCompletion: true, epochCount: 3, staleCandidateRejected: true,
    completionGenerationAdvance: '1',
    prematurePublicationRejected: true, publishedCommitted: true,
  })
  expect(BigInt(result.completionBytes)).toBeGreaterThan(6n)
})

for (const fault of ['before-publish', 'unknown-tail'] as const) {
  test(`Direct ZIP retains predecessor member rollback after a failed final ${fault} cut`, async ({ page }) => {
    await page.goto(BROWSER_CONTRACT_HOST_PATH)
    const result = await page.evaluate(async input => {
      const path = '/test/browser/direct-zip/direct-zip-writer-bridge-probe.ts'
      const probe = await import(path) as typeof import('./direct-zip-writer-bridge-probe')
      return probe.probeDirectZipWriterBridge(input.name, input.fault)
    }, { name: 'direct-zip-rollback-' + crypto.randomUUID(), fault })
    expect(result).toMatchObject({
      storedCompletion: true, publishedCommitted: true, epochCount: 4,
      rollbackRecovery: { sameJournalSavedOffset: '4', truncated: fault === 'unknown-tail', candidateRetired: true },
    })
  })
}
