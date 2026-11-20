import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'
import { requireOriginPrivateStorage } from './browser-storage-support'

const REOPEN_PROBE = '/test/browser/browser-delivery-reopen-probe.ts'

for (const preference of ['direct', 'automatic'] as const) {
  test(`folder download can continue after a second revision reopen (${preference})`, async ({ page, browserName }) => {
    await page.goto(BROWSER_CONTRACT_HOST_PATH)
    await requireOriginPrivateStorage(page, browserName)
    const first = await page.evaluate(async ({ path, parentName, preference }) => {
      const probe = await import(path) as typeof import('./browser-delivery-reopen-probe')
      return probe.exhaustFolderFileReadmission(parentName, preference)
    }, { path: REOPEN_PROBE, parentName: 'reopen-' + crypto.randomUUID(), preference })
    expect(first).toMatchObject({
      worker: 'Paused', lifecycle: 'resumable-receive', siblingBytes: [7, 8],
      reads: [
        { name: 'payload.bin', offset: 0 },
        { name: 'payload.bin', offset: 2 },
        { name: 'sibling.bin', offset: 0 },
      ],
      decisions: [
        { decision: 'readmit', retained: '2', reused: '0' },
        { decision: 'readmitted', retained: '2', reused: '2' },
        { decision: 'retry-limit', retained: '4', reused: '0' },
      ],
    })

    await page.reload()
    const resumed = await page.evaluate(async ({ path, fixture }) => {
      const probe = await import(path) as typeof import('./browser-delivery-reopen-probe')
      return probe.continueReopenedFolder(fixture)
    }, { path: REOPEN_PROBE, fixture: first.fixture })
    expect(resumed).toMatchObject({
      reads: [{ name: 'payload.bin', offset: 4 }],
      siblingOpens: 0,
      lifecycle: 'published', worker: 'Succeeded',
      stagedBytesBefore: preference === 'automatic' ? '4' : '0',
      siblingBytes: [7, 8],
      payloadBytes: [1, 2, 3, 4, 5, 6],
    })
  })
}

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
    partialGeneration: '8', partialBytes: '3', retainedActions: ['save-staged-files'],
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
