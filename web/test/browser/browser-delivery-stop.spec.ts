import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH, requireOriginPrivateStorage } from './browser-storage-support'
import type { StopStorageCut } from './browser-delivery-stop-probe'

const PROBE = '/test/browser/browser-delivery-stop-probe.ts'
const PREFIX = [1, 3, 5, 7, 9, 11, 13, 15]
const AUTHENTICATED_SIZE = String(256 * 1024 ** 2)
const RESERVED_PREFIX = [{ exactSize: AUTHENTICATED_SIZE, verifiedBytes: String(PREFIX.length) }]

for (const cut of ['stop', 'pause', 'cleanup-failure'] satisfies StopStorageCut[]) {
  test(`real browser folder ${cut} settles incomplete staging and reopens its storage obligations`, async ({ page, browserName, context }) => {
    await page.goto(BROWSER_CONTRACT_HOST_PATH)
    await requireOriginPrivateStorage(page, browserName)
    const key = crypto.randomUUID()
    const prepared = await page.evaluate(async ({ path, parentName, cut }) => {
      const probe = await import(path) as typeof import('./browser-delivery-stop-probe')
      return probe.prepareStopStorage(parentName, cut)
    }, { path: PROBE, parentName: 'stop-' + key, cut })

    expect(prepared.before).toMatchObject({
      deliveryState: 'receiving', stageBytes: PREFIX, reservations: RESERVED_PREFIX,
      reservedBytes: AUTHENTICATED_SIZE, retainedBytes: String(PREFIX.length),
    })
    expect(prepared.lifecycle).toBe(cut === 'pause' ? 'resumable-receive' : 'partial-directory')
    if (cut === 'stop') {
      expect(prepared.after).toMatchObject({ deliveryState: 'discarded', stageBytes: null,
        reservations: [], reservedBytes: '0', targetBytes: [] })
    } else {
      expect(prepared.after).toMatchObject({
        deliveryState: cut === 'pause' ? 'receiving' : 'discarding',
        stageBytes: PREFIX, reservations: RESERVED_PREFIX, reservedBytes: AUTHENTICATED_SIZE,
      })
      if (cut === 'cleanup-failure') {
        expect(prepared.injectedFailures).toBeGreaterThan(0)
        expect(prepared.after.actions).toContain('cleanup-staging')
      }
    }

    await page.reload()
    if (cut === 'cleanup-failure') {
      await page.evaluate(async path => { await import(path) }, PROBE)
      await context.setOffline(true)
    }
    const reopened = await page.evaluate(async ({ path, fixture, cut }) => {
      const probe = await import(path) as typeof import('./browser-delivery-stop-probe')
      return probe.reopenStopStorage(fixture, cut)
    }, { path: PROBE, fixture: prepared.fixture, cut })
    expect(reopened.before).toEqual(prepared.after)
    if (cut === 'pause') {
      expect(reopened.resumedRanges).toEqual(['0:' + PREFIX.length])
      expect(reopened.after).toMatchObject({ stageBytes: PREFIX, reservations: RESERVED_PREFIX,
        lifecycle: 'resumable-receive' })
    } else {
      expect(reopened.after).toMatchObject({
        deliveryState: 'discarded', stageBytes: null, reservations: [], reservedBytes: '0',
        lifecycle: 'partial-directory', targetBytes: [],
      })
    }
  })
}

test('Stop preserves complete staging for a local save after reopening terminal folder output', async ({ page, browserName }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)
  const prepared = await page.evaluate(async ({ path, parentName }) => {
    const probe = await import(path) as typeof import('./browser-delivery-stop-probe')
    return probe.prepareStopStorage(parentName, 'complete-stage')
  }, { path: PROBE, parentName: 'complete-stop-' + crypto.randomUUID() })
  expect(prepared.before).toMatchObject({ deliveryState: 'receiving', stageBytes: PREFIX,
    targetBytes: [], reservations: [{ exactSize: '8', verifiedBytes: '8' }] })
  expect(prepared.after).toMatchObject({ deliveryState: 'staged-complete', stageBytes: PREFIX,
    targetBytes: [], lifecycle: 'partial-directory', continuation: 'save-staged-files' })
  expect(prepared.after.actions).toContain('save-staged-files')
  expect(prepared.after.actions).not.toContain('discard-incomplete-staging')

  await page.reload()
  const reopened = await page.evaluate(async ({ path, fixture }) => {
    const probe = await import(path) as typeof import('./browser-delivery-stop-probe')
    return probe.reopenStopStorage(fixture, 'complete-stage')
  }, { path: PROBE, fixture: prepared.fixture })
  expect(reopened.before).toEqual(prepared.after)
  expect(reopened.after).toMatchObject({ deliveryState: 'cleaned', stageBytes: null,
    targetBytes: PREFIX, reservations: [], reservedBytes: '0',
    lifecycle: 'partial-directory', lifecycleGeneration: prepared.after.lifecycleGeneration })
})

test('retained complete-stage save refuses a replaced target and preserves both contents', async ({ page, browserName }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)
  const prepared = await page.evaluate(async ({ path, parentName }) => {
    const probe = await import(path) as typeof import('./browser-delivery-stop-probe')
    return probe.prepareStopStorage(parentName, 'complete-stage')
  }, { path: PROBE, parentName: 'replaced-stop-' + crypto.randomUUID() })
  await page.reload()
  const rejected = await page.evaluate(async ({ path, fixture }) => {
    const probe = await import(path) as typeof import('./browser-delivery-stop-probe')
    return probe.replaceStoppedTargetAndSave(fixture)
  }, { path: PROBE, fixture: prepared.fixture })
  expect(rejected.after).toMatchObject({ deliveryState: 'staged-complete', stageBytes: PREFIX,
    targetBytes: [20, 40, 60], reservations: [{ exactSize: '8', verifiedBytes: '8' }],
    lifecycle: 'partial-directory', lifecycleGeneration: prepared.after.lifecycleGeneration })
  expect(rejected.failures).toHaveLength(2)
  for (const failure of rejected.failures) expect(failure).toMatch(/owned|ownership|existing/iu)
})
