import { expect, test } from '@playwright/test'

const PROBE_PATH = '/test/browser/browser-delivery-idb-probe.ts'

test('retained staging and target proof remain distinct after reload and offline continuation', async ({ page, context }) => {
  await page.goto('/')
  const databaseName = 'delivery-reopen-' + crypto.randomUUID()
  const initial = await page.evaluate(async ({ path, databaseName }) => {
    const probe = await import(path) as typeof import('./browser-delivery-idb-probe')
    return probe.prepareBrowserDeliveryReopen(databaseName)
  }, { path: PROBE_PATH, databaseName })
  expect(initial).toEqual({
    missingStageRejected: true, missingTargetRejected: true, retainedState: 'copying',
    crashGapContinuation: 'save-staged-files', crashGapTargetBytes: '0',
  })

  await page.reload()
  await page.evaluate(async path => { await import(path) }, PROBE_PATH)
  await context.setOffline(true)
  const recovered = await page.evaluate(async ({ path, databaseName }) => {
    const probe = await import(path) as typeof import('./browser-delivery-idb-probe')
    return probe.finishBrowserDeliveryReopen(databaseName)
  }, { path: PROBE_PATH, databaseName })
  expect(recovered).toEqual({
    checkpointOnlyRejected: true,
    beforeContinuation: 'save-staged-files', beforeTargetSavedBytes: '0', beforeStagedBytes: '8',
    afterTargetSavedBytes: '8', afterStagedBytes: '0', afterState: 'cleaned',
  })
})

test('explicit redownload durably authorizes one reset while ordinary CAS and forged baselines remain forbidden', async ({ page }) => {
  await page.goto('/')
  const result = await page.evaluate(async path => {
    const probe = await import(path) as typeof import('./browser-delivery-idb-probe')
    return probe.restartDeliveryJournal('delivery-restart-' + crypto.randomUUID())
  }, PROBE_PATH)
  expect(result).toEqual({ forgedMutationRejected: true, ordinaryResetRejected: true,
    markerState: 'restart-authorized', reopenedState: 'receiving' })
})

test('file authority uses atomic exact CAS and bounded pages across independent repositories', async ({ page }) => {
  await page.goto('/')
  const result = await page.evaluate(async path => {
    const probe = await import(path) as typeof import('./browser-delivery-idb-probe')
    return probe.raceBrowserDeliveryJournal('delivery-race-' + crypto.randomUUID())
  }, PROBE_PATH)
  expect(result).toEqual({
    policyConflictRejected: true, sourceConflictRejected: true, winners: 1, losers: 1,
    retriedPlacementState: 'target-saved', pages: [2, 2], uniqueFiles: 4, expectedFiles: 4,
  })
})

test('direct target completion uses one atomic transaction while retaining exact final proof checks', async ({ page }) => {
  await page.goto('/')
  const result = await page.evaluate(async path => {
    const probe = await import(path) as typeof import('./browser-delivery-idb-probe')
    return probe.finalizeDirectDelivery('delivery-direct-' + crypto.randomUUID())
  }, PROBE_PATH)
  expect(result).toEqual({
    missingCheckpointRejected: true, missingFinalProofRejected: true, foreignProofRejected: true,
    finalizationTransactions: 1, state: 'cleaned', generation: '3', idempotent: true, targetBytes: '8',
  })
})

test('an aborted delivery cut preserves its predecessor and the persisted staging proof for retry', async ({ page }) => {
  await page.goto('/')
  const result = await page.evaluate(async path => {
    const probe = await import(path) as typeof import('./browser-delivery-idb-probe')
    return probe.abortBrowserDeliveryCut('delivery-abort-' + crypto.randomUUID())
  }, PROBE_PATH)
  expect(result).toEqual({ aborted: true, priorState: 'receiving', retryState: 'staged-complete' })
})
