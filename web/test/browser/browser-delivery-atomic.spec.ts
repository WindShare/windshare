import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'

const PROBE = '/test/browser/browser-delivery-atomic-probe.ts'

for (const [preference, failure] of [['automatic', 'delivery'], ['direct', 'target-proof']] as const) {
  test(`direct ${preference} placement and ${failure} failure share checkpoint commit authority across reload`, async ({ page }) => {
    await page.goto(BROWSER_CONTRACT_HOST_PATH)
    const fixture = { databaseName: 'delivery-atomic-' + crypto.randomUUID(), parentName: 'delivery-atomic-' + crypto.randomUUID() }
    const prepared = await page.evaluate(async ({ path, fixture, preference }) => {
      const probe = await import(path) as typeof import('./browser-delivery-atomic-probe')
      return probe.prepareAtomicDirect(fixture, preference)
    }, { path: PROBE, fixture, preference })
    expect(prepared).toMatchObject({ initialAborted: true, absentAfterAbort: true,
      candidateCount: 0, speculativeFiles: 0, initialState: 'receiving', retainedBytes: '3' })
    await page.reload()
    const finished = await page.evaluate(async ({ path, prepared, failure }) => {
      const probe = await import(path) as typeof import('./browser-delivery-atomic-probe')
      return probe.finishAtomicDirect(prepared, failure)
    }, { path: PROBE, prepared, failure })
    expect(finished).toEqual({ resumedBytes: '3', finalAborted: true, stateAfterAbort: 'receiving',
      retainedAfterAbort: '3', savedAfterAbort: '0', proofCountAfterAbort: 0, finalTransactions: 1,
      finalState: 'cleaned', finalBytes: '8', contentsMatch: true })
  })
}
