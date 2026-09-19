import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from '../contract-host'
import { requireOriginPrivateStorage } from '../browser-storage-support'

const HARNESS_PATH = '/test/browser/opfs/persistence-activation-harness.ts'
type Harness = typeof import('./persistence-activation-harness')

test.beforeEach(async ({ page, browserName }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)
})

for (const granted of [true, false]) {
  test(`activates and discards file and ZIP tasks before optional permission settles (${granted})`, async ({ page }) => {
    const proof = await page.evaluate(async ({ path, granted }) => {
      const harness = await import(path) as Harness
      return harness.withPendingPermission(granted)
    }, { path: HARNESS_PATH, granted })
    expect(proof).toMatchObject({
      requests: 1,
      pauses: ['resumable-start', 'resumable-start'],
      beforePermission: ['discarded', 'discarded'],
      afterPermission: ['discarded', 'discarded'],
      activationLocks: [],
      transitions: ['requested', 'already_pending', granted ? 'granted' : 'not_granted'],
    })
  })
}

test('native persistence does not own workspace activation or cancellation', async ({ page }) => {
  const proof = await page.evaluate(async path => {
    const harness = await import(path) as Harness
    return harness.withNativePermission()
  }, HARNESS_PATH)
  expect(proof).toMatchObject({
    pauses: ['resumable-start', 'resumable-start'],
    beforePermission: ['discarded', 'discarded'],
    afterPermission: ['discarded', 'discarded'],
    activationLocks: [],
  })
  expect(proof.transitions).toContain('requested')
})
