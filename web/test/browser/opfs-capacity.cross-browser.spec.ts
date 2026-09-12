import { expect, test, type Page } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'
import type { CapacityAction } from './opfs/opfs-capacity-harness'

const HARNESS_PATH = '/test/browser/opfs/opfs-capacity-harness.ts'

const STAGING_HARNESS_PATH = '/test/browser/opfs/staging-capacity-harness.ts'

async function stage(page: Page, actor: string,
  kind: Parameters<typeof import('./opfs/staging-capacity-harness').stagingCapacityAction>[1],
  operationId = actor) {
  return page.evaluate(async ({ path, actor, kind, operationId }) => {
    const harness = await import(path) as typeof import('./opfs/staging-capacity-harness')
    return harness.stagingCapacityAction(actor, kind, 500n, operationId)
  }, { path: STAGING_HARNESS_PATH, actor, kind, operationId })
}

async function openStage(page: Page, databaseName: string, actor: string, quota = 1000n) {
  await page.evaluate(async ({ path, databaseName, actor, quota }) => {
    const harness = await import(path) as typeof import('./opfs/staging-capacity-harness')
    await harness.openStagingCapacity(databaseName, actor, quota)
  }, { path: STAGING_HARNESS_PATH, databaseName, actor, quota })
}

async function closeStages(pages: readonly Page[]) {
  for (const page of pages) {
    await page.evaluate(async path => {
      const harness = await import(path) as typeof import('./opfs/staging-capacity-harness')
      await harness.closeStagingCapacity()
    }, STAGING_HARNESS_PATH)
  }
}

test('shares full-file staging admission atomically with existing workspace growth across tabs', async ({ page, context }) => {
  const other = await context.newPage()
  const pages = [page, other]
  const databaseName = `windshare-staging-capacity-${crypto.randomUUID()}`
  await Promise.all(pages.map(current => current.goto(BROWSER_CONTRACT_HOST_PATH)))
  try {
    await open(page, databaseName, 0, 'workspace')
    await openStage(other, databaseName, 'staging')
    expect(await action(page, { token: 'workspace', kind: 'claim' })).toBe('accepted')
    const results = await Promise.all([
      action(page, { token: 'workspace', kind: 'reserve', target: '500' }),
      stage(other, 'staging', 'reserve'),
    ])
    expect(results.filter(result => result === 'accepted' || result === 'admitted')).toHaveLength(1)
    if (results[0] === 'accepted') {
      expect(results[1]).toBe('quota-insufficient')
      await action(page, { token: 'workspace', kind: 'release-reservation' })
      expect(await stage(other, 'staging', 'reserve')).toBe('admitted')
    } else {
      expect(results[0]).toBe('QuotaExceededError')
    }
    expect(await action(page, { token: 'workspace', kind: 'reserve', target: '500' })).toBe('QuotaExceededError')
    expect(await stage(other, 'staging', 'cancel')).toBe('ok')
    expect(await action(page, { token: 'workspace', kind: 'reserve', target: '500' })).toBe('accepted')
  } finally {
    await closeStages(pages)
    await cleanup(pages, databaseName)
    await other.close()
  }
})

test('reclaims a vanished tab export claim while preserving that task staging obligation', async ({ page, context }) => {
  const other = await context.newPage()
  const pages = [page, other]
  const databaseName = `windshare-staging-export-${crypto.randomUUID()}`
  await Promise.all(pages.map(current => current.goto(BROWSER_CONTRACT_HOST_PATH)))
  try {
    await openStage(page, databaseName, 'first', 2000n)
    await openStage(other, databaseName, 'second', 2000n)
    expect(await stage(page, 'first', 'reserve')).toBe('admitted')
    expect(await stage(other, 'second', 'reserve')).toBe('admitted')
    expect(await stage(page, 'first', 'queue')).toBe('ok')
    expect(await stage(other, 'second', 'queue')).toBe('ok')
    expect(await stage(page, 'first', 'export')).toBe('started')
    const waiting = stage(other, 'second', 'export')
    await expect.poll(() => other.evaluate(async () => {
      const locks = await navigator.locks.query()
      return locks.pending?.some(lock => lock.name === 'windshare/browser-delivery/local-copy/v1') ?? false
    })).toBe(true)
    // Closing the owner releases its real Web Lock, but leaves the committed exporting record.
    await page.close()
    expect(await waiting).toBe('started')
    expect(await other.evaluate(async path => {
      const harness = await import(path) as typeof import('./opfs/staging-capacity-harness')
      return (await harness.stagingCapacitySnapshot('second')).reservedStagingBytes
    }, STAGING_HARNESS_PATH)).toBe(1060n)
    expect(await stage(other, 'second', 'saved')).toBe('ok')
    expect(await stage(other, 'second', 'deleted')).toBe('ok')
    expect(await other.evaluate(async path => {
      const harness = await import(path) as typeof import('./opfs/staging-capacity-harness')
      return (await harness.stagingCapacitySnapshot('second')).reservedStagingBytes
    }, STAGING_HARNESS_PATH)).toBe(530n)
  } finally {
    const live = pages.filter(current => !current.isClosed())
    await closeStages(live)
    await cleanup(live, databaseName)
    await other.close()
  }
})

async function open(page: Page, databaseName: string, actor: number, token: string, now = 0) {
  await page.evaluate(async ({ path, databaseName, actor, token, now }) => {
    const harness = await import(path) as typeof import('./opfs/opfs-capacity-harness')
    await harness.openCapacitySession(databaseName, actor, token, now)
  }, { path: HARNESS_PATH, databaseName, actor, token, now })
}

async function action(page: Page, input: CapacityAction) {
  return page.evaluate(async ({ path, input }) => {
    const harness = await import(path) as typeof import('./opfs/opfs-capacity-harness')
    return harness.capacityAction(input)
  }, { path: HARNESS_PATH, input })
}

async function cleanup(pages: readonly Page[], databaseName: string) {
  for (const page of pages) {
    await page.evaluate(async path => {
      const harness = await import(path) as typeof import('./opfs/opfs-capacity-harness')
      harness.closeCapacitySessions()
    }, HARNESS_PATH)
  }
  await pages[0]!.evaluate(async ({ path, databaseName }) => {
    const harness = await import(path) as typeof import('./opfs/opfs-capacity-harness')
    await harness.deleteCapacityDatabase(databaseName)
  }, { path: HARNESS_PATH, databaseName })
}

test('serializes two-tab logical growth, retains headroom, and avoids counting settled usage twice', async ({
  page, context,
}) => {
  const other = await context.newPage()
  const pages = [page, other]
  const databaseName = `windshare-capacity-${crypto.randomUUID()}`
  await Promise.all(pages.map(current => current.goto(BROWSER_CONTRACT_HOST_PATH)))
  try {
    await Promise.all(pages.map((current, actor) => open(current, databaseName, actor, `owner-${actor}`)))
    expect(await Promise.all(pages.map((current, actor) =>
      action(current, { token: `owner-${actor}`, kind: 'claim' })))).toEqual(['accepted', 'accepted'])
    const claims = await Promise.all(pages.map((current, actor) =>
      action(current, { token: `owner-${actor}`, kind: 'reserve', target: '500', headroom: '50' })))
    expect([...claims].sort()).toEqual(['QuotaExceededError', 'accepted'])
    const winner = claims.indexOf('accepted')
    const loser = 1 - winner
    const winningPage = pages[winner]!
    const losingPage = pages[loser]!
    const winningToken = `owner-${winner}`
    const losingToken = `owner-${loser}`
    expect(await action(winningPage, { token: winningToken, kind: 'settle', current: '500' })).toBe('accepted')
    // 500 occupied + 300 pending + 64 task metadata + 36 write metadata + 100 reserve = 1000.
    expect(await action(losingPage, { token: losingToken, kind: 'reserve',
      target: '300', headroom: '36', usage: '500' })).toBe('accepted')
    expect(await action(winningPage, { token: winningToken, kind: 'reserve',
      current: '500', target: '400', quota: '0', reservation: 'fill' })).toBe('accepted')
    expect(await action(winningPage, { token: winningToken, kind: 'reserve',
      current: '500', target: '400', quota: '0', headroom: '1', reservation: 'extra-metadata' }))
      .toBe('QuotaExceededError')
    expect(await action(winningPage, { token: winningToken, kind: 'release-reservation', reservation: 'fill' }))
      .toBe('accepted')
    expect(await action(losingPage, { token: losingToken, kind: 'release-reservation' })).toBe('accepted')
    expect(await action(winningPage, { token: winningToken, kind: 'release' })).toBe('accepted')
    // Releasing ownership must retain occupied bytes even when the browser estimate is stale.
    expect(await action(losingPage, { token: losingToken, kind: 'reserve', target: '369' }))
      .toBe('QuotaExceededError')
    expect(await action(winningPage, { token: winningToken, kind: 'forget' })).toBe('accepted')
    expect(await action(losingPage, { token: losingToken, kind: 'reserve', target: '868' })).toBe('accepted')
    expect(await action(winningPage, { token: winningToken, kind: 'heartbeat' })).toBe('InvalidStateError')
  } finally {
    await cleanup(pages, databaseName)
    await other.close()
  }
})

test('reconciles expired uncertain growth and fences every stale owner mutation across tabs', async ({
  page, context,
}) => {
  const other = await context.newPage()
  const pages = [page, other]
  const databaseName = `windshare-capacity-${crypto.randomUUID()}`
  await Promise.all(pages.map(current => current.goto(BROWSER_CONTRACT_HOST_PATH)))
  try {
    await open(page, databaseName, 0, 'stale')
    expect(await action(page, { token: 'stale', kind: 'claim' })).toBe('accepted')
    expect(await action(page, { token: 'stale', kind: 'reserve', target: '500' })).toBe('accepted')
    await open(other, databaseName, 1, 'other', 1_001)
    expect(await action(other, { token: 'other', kind: 'claim', now: 1_001 })).toBe('accepted')
    // Expiration cannot make possibly written bytes available to another task.
    expect(await action(other, { token: 'other', kind: 'reserve', now: 1_001, target: '369' }))
      .toBe('QuotaExceededError')
    await open(other, databaseName, 0, 'fresh', 1_001)
    expect(await action(other, { token: 'fresh', kind: 'reclaim', now: 1_001, verified: '200' }))
      .toBe('accepted')
    for (const kind of ['reserve', 'settle', 'reconcile', 'heartbeat'] as const) {
      expect(await action(page, { token: 'stale', kind, now: 1_001, current: '0', target: '1' }))
        .toBe('InvalidStateError')
    }
    expect(await action(page, { token: 'stale', kind: 'release', now: 1_001 })).toBe('accepted')
    expect(await action(other, { token: 'fresh', kind: 'reconcile', now: 1_001, current: '200' }))
      .toBe('accepted')
    expect(await action(other, { token: 'fresh', kind: 'reserve', now: 1_001,
      current: '200', target: '300', reservation: 'recovered' })).toBe('accepted')
    expect(await action(other, { token: 'fresh', kind: 'settle', now: 1_001,
      current: '300', reservation: 'recovered' })).toBe('accepted')
    expect(await action(other, { token: 'fresh', kind: 'reconcile', now: 1_001, current: '200' }))
      .toBe('accepted')
    // Fresh recovery inventory replaces the expired high-water floor; it must not count the old reservation.
    expect(await action(other, { token: 'other', kind: 'reserve', now: 1_001, target: '636' }))
      .toBe('accepted')
    expect(await action(other, { token: 'fresh', kind: 'reserve', now: 1_001,
      current: '200', target: '201' })).toBe('QuotaExceededError')
  } finally {
    await cleanup(pages, databaseName)
    await other.close()
  }
})
