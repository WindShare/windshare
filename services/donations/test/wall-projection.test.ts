import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  PUBLIC_WALL_LIMIT,
  type PublicWallResponse,
} from '../src/model'
import {
  PUBLIC_WALL_RANKABLE_SQL,
  PUBLIC_WALL_RECENT_SQL,
  SELECT_PUBLIC_WALL_PROJECTION_SQL,
} from '../src/wall-queries'
import { WallStore } from '../src/wall-store'
import {
  createInMemoryWallStoreWithDb,
  createWallStoreFromDb,
} from './wall-harness'

interface CapturedLog {
  readonly level: 'info' | 'warn' | 'error'
  readonly payload: Record<string, unknown>
}

function captureLogs(): CapturedLog[] {
  const logs: CapturedLog[] = []
  const push = (level: 'info' | 'warn' | 'error') => (line: unknown) => {
    if (typeof line !== 'string') return
    logs.push({ level, payload: JSON.parse(line) as Record<string, unknown> })
  }
  vi.spyOn(console, 'log').mockImplementation(push('info'))
  vi.spyOn(console, 'warn').mockImplementation(push('warn'))
  vi.spyOn(console, 'error').mockImplementation(push('error'))
  return logs
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('PublicWallProjectionStore materialized snapshots and incremental ranking', () => {
  it('serves cold DO reads from a single projection row and updates published writes without scanning donations', () => {
    const { store, db } = createInMemoryWallStoreWithDb()

    // Seed an early founding whale plus 65 newer small donations so the whale drops off entries[50].
    const whale = store.commitDonation({
      provider: 'afdian',
      externalId: 'WHALE-1',
      displayName: 'Founding Whale',
      amountMinor: 500_000,
      currency: 'CNY',
      minorDigits: 2,
      message: 'Keep building!',
      isPublic: true,
      donatedAt: 1000,
    }, 1000)

    for (let i = 0; i < PUBLIC_WALL_LIMIT + 15; i += 1) {
      store.commitDonation({
        provider: 'kofi',
        externalId: `small-${i}`,
        displayName: `Donor ${i}`,
        amountMinor: 500 + (i === 5 ? 10_000 : 0),
        currency: 'USD',
        minorDigits: 2,
        message: `Note ${i}`,
        isPublic: true,
        donatedAt: 2000 + i,
      }, 2000 + i)
    }

    const expectedBeforeCold = store.getPublicWallSnapshot()

    // Verify the recent-50 SQL uses idx_donations_public_wall with no temp B-tree sort.
    const plan = db.prepare(`EXPLAIN QUERY PLAN ${PUBLIC_WALL_RECENT_SQL}`)
      .all(PUBLIC_WALL_LIMIT) as Array<{ detail: string }>
    const planText = plan.map(row => row.detail).join('\n')
    expect(planText).toContain('idx_donations_public_wall')
    expect(planText).not.toContain('USE TEMP B-TREE')

    // Cold-starting a fresh WallStore reads ONLY the persisted projection row, never touching donations.
    const executedQueries: string[] = []
    const coldStore = createWallStoreFromDb(db, {
      onSqlExec: query => {
        executedQueries.push(query)
      },
    })
    executedQueries.length = 0

    const coldSnapshot = coldStore.getPublicWallSnapshot()
    expect(coldSnapshot).toEqual(expectedBeforeCold)
    expect(executedQueries).toEqual([SELECT_PUBLIC_WALL_PROJECTION_SQL])

    // Subsequent warm read hits in-memory cache with zero SQLite queries.
    executedQueries.length = 0
    expect(coldStore.getPublicWallSnapshot()).toEqual(expectedBeforeCold)
    expect(executedQueries).toHaveLength(0)

    // Incremental published writes (new small donor, higher donation from same named donor,
    // manual donation, and batch commit) must NEVER execute a full donations ranking scan.
    executedQueries.length = 0
    coldStore.commitDonation({
      provider: 'kofi',
      externalId: 'inc-small',
      displayName: 'Incremental Small',
      amountMinor: 600,
      currency: 'USD',
      minorDigits: 2,
      message: null,
      isPublic: true,
      donatedAt: 3000,
    }, 3000)
    coldStore.commitDonation({
      provider: 'afdian',
      externalId: 'whale-upgrade',
      displayName: '  Ｆｏｕｎｄｉｎｇ Ｗｈａｌｅ ',
      amountMinor: 800_000,
      currency: 'CNY',
      minorDigits: 2,
      message: 'Doubling down!',
      isPublic: true,
      donatedAt: 3005,
    }, 3005)
    const manual = coldStore.createManualDonation({
      provider: 'direct',
      externalId: 'manual-top-2',
      displayName: 'OnChain Patron',
      amountMinor: 90_000,
      currency: 'USD',
      minorDigits: 2,
      message: null,
      donatedAt: 3010,
      idempotencyKey: null,
    }, 3010)
    expect(manual.ok).toBe(true)
    coldStore.commitDonationsBatch([
      {
        provider: 'afdian',
        externalId: 'batch-pub-1',
        displayName: 'Batch Supporter',
        amountMinor: 2_000,
        currency: 'CNY',
        minorDigits: 2,
        message: null,
        isPublic: true,
        donatedAt: 3015,
      },
    ], 3015)

    expect(executedQueries.some(sql => sql === PUBLIC_WALL_RANKABLE_SQL)).toBe(false)
    expect(executedQueries.some(sql => sql === PUBLIC_WALL_RECENT_SQL)).toBe(true)

    const updatedWall = JSON.parse(coldStore.getPublicWallSnapshot().json) as PublicWallResponse
    expect(updatedWall.entries).toHaveLength(PUBLIC_WALL_LIMIT)
    expect(updatedWall.entries[0]?.displayName).toBe('Batch Supporter')
    expect(updatedWall.topSupporters.map(e => ({ name: e.displayName, amount: e.amountMinor }))).toEqual([
      { name: '  Ｆｏｕｎｄｉｎｇ Ｗｈａｌｅ ', amount: 800_000 },
      { name: 'OnChain Patron', amount: 90_000 },
      { name: 'Donor 5', amount: 10_500 },
    ])
    expect(updatedWall.topSupporters.some(e => e.id === whale.record.id)).toBe(false)
  })

  it('keeps wall_version and ETag stable on non-published writes and bumps only on published visibility changes', () => {
    const { store, db } = createInMemoryWallStoreWithDb()
    store.commitDonation({
      provider: 'kofi',
      externalId: 'pub-base',
      displayName: 'Base Donor',
      amountMinor: 2_000,
      currency: 'USD',
      minorDigits: 2,
      message: 'Public',
      isPublic: true,
      donatedAt: 1000,
    }, 1000)

    const baseline = store.getPublicWallSnapshot()
    const observedQueries: string[] = []
    const observedStore = createWallStoreFromDb(db, {
      onSqlExec: query => {
        observedQueries.push(query)
      },
    })
    expect(observedStore.getPublicWallSnapshot()).toEqual(baseline)
    observedQueries.length = 0

    // Private, below-threshold, and pending-review donations must not mutate wall_version or projection.
    const priv = observedStore.commitDonation({
      provider: 'kofi',
      externalId: 'priv-1',
      displayName: 'Private',
      amountMinor: 100_000,
      currency: 'USD',
      minorDigits: 2,
      message: 'Secret',
      isPublic: false,
      donatedAt: 1010,
    }, 1010)
    expect(priv.record.visibility.kind).toBe('private_donor')

    const below = observedStore.commitDonation({
      provider: 'kofi',
      externalId: 'below-1',
      displayName: 'Tiny',
      amountMinor: 100,
      currency: 'USD',
      minorDigits: 2,
      message: null,
      isPublic: true,
      donatedAt: 1020,
    }, 1020)
    expect(below.record.visibility.kind).toBe('below_threshold')

    const pending = observedStore.commitDonation({
      provider: 'afdian',
      externalId: 'pending-url',
      displayName: 'Link Donor',
      amountMinor: 50_000,
      currency: 'CNY',
      minorDigits: 2,
      message: 'https://example.org',
      isPublic: true,
      donatedAt: 1030,
    }, 1030)
    expect(pending.record.visibility.kind).toBe('pending_review')

    // Hiding and restoring a non-published record leaves the public wall untouched.
    expect(observedStore.moderateDonationVisibility(pending.record.id, { action: 'hide', reason: 'review' }, 1040).ok).toBe(true)
    expect(observedStore.moderateDonationVisibility(pending.record.id, { action: 'restore' }, 1050).ok).toBe(true)

    // Intra-batch insert-then-redact produces zero net public wall change.
    observedStore.commitDonationsBatch([
      {
        provider: 'kofi',
        externalId: 'same-batch-priv',
        displayName: 'Temp',
        amountMinor: 5_000,
        currency: 'USD',
        minorDigits: 2,
        message: 'Temp',
        isPublic: true,
        donatedAt: 1060,
      },
      {
        provider: 'kofi',
        externalId: 'same-batch-priv',
        displayName: null,
        amountMinor: 5_000,
        currency: 'USD',
        minorDigits: 2,
        message: null,
        isPublic: false,
        donatedAt: 1060,
      },
    ], 1060)

    expect(observedStore.getPublicWallSnapshot()).toEqual(baseline)
    expect(observedQueries.some(sql => sql.includes('public_wall_projection'))).toBe(false)

    // Approving pending_review transitions it into published -> incremental bump without full scan.
    observedQueries.length = 0
    expect(observedStore.moderateDonationVisibility(pending.record.id, { action: 'approve' }, 1070).ok).toBe(true)
    const afterApprove = observedStore.getPublicWallSnapshot()
    expect(afterApprove.version).toBe(baseline.version + 1)
    expect(afterApprove.etag).not.toBe(baseline.etag)
    expect(observedQueries.some(sql => sql === PUBLIC_WALL_RANKABLE_SQL)).toBe(false)
    const approvedWall = JSON.parse(afterApprove.json) as PublicWallResponse
    expect(approvedWall.topSupporters[0]?.id).toBe(pending.record.id)
  })

  it('skips full scans when hiding non-Top-3 entries and rebuilds once when removing a Top-3 supporter', () => {
    const logs = captureLogs()
    const { store, db } = createInMemoryWallStoreWithDb()
    const queries: string[] = []
    const trackedStore = createWallStoreFromDb(db, {
      onSqlExec: query => {
        queries.push(query)
      },
    })

    // Seed 5 published donations so #1..#3 are Top 3 and #4..#5 are outside Top 3.
    const seeded = [
      { ext: 'top-1', name: 'Rank One', minor: 50_000, at: 100 },
      { ext: 'top-2', name: 'Rank Two', minor: 40_000, at: 110 },
      { ext: 'top-3', name: 'Rank Three', minor: 30_000, at: 120 },
      { ext: 'rank-4', name: 'Rank Four', minor: 20_000, at: 130 },
      { ext: 'rank-5', name: 'Rank Five', minor: 10_000, at: 140 },
    ].map(item => store.commitDonation({
      provider: 'kofi',
      externalId: item.ext,
      displayName: item.name,
      amountMinor: item.minor,
      currency: 'USD',
      minorDigits: 2,
      message: null,
      isPublic: true,
      donatedAt: item.at,
    }, item.at).record)

    // Warm persisted projection on trackedStore, then reset spies.
    trackedStore.getPublicWallSnapshot()
    queries.length = 0
    logs.length = 0

    // Hiding #5 (outside Top 3) must NOT run PUBLIC_WALL_RANKABLE_SQL.
    const rank5 = seeded[4]!
    expect(trackedStore.moderateDonationVisibility(rank5.id, { action: 'hide', reason: 'cleanup' }, 200).ok).toBe(true)
    expect(queries.filter(sql => sql === PUBLIC_WALL_RANKABLE_SQL)).toHaveLength(0)
    expect(logs.filter(l => l.payload.event === 'donation.wall_projection_rebuilt')).toHaveLength(0)

    // Hiding #1 (inside Top 3) triggers exactly one bounded streaming rebuild and promotes #4.
    const rank1 = seeded[0]!
    expect(trackedStore.moderateDonationVisibility(rank1.id, { action: 'hide', reason: 'audit' }, 210).ok).toBe(true)
    expect(queries.filter(sql => sql === PUBLIC_WALL_RANKABLE_SQL)).toHaveLength(1)
    expect(logs).toContainEqual({
      level: 'info',
      payload: {
        event: 'donation.wall_projection_rebuilt',
        reason: 'top_supporter_removed',
        wall_version: 7,
        scanned_rows: 3,
      },
    })
    const wallAfterHideTop1 = JSON.parse(trackedStore.getPublicWallSnapshot().json) as PublicWallResponse
    expect(wallAfterHideTop1.topSupporters.map(e => e.displayName)).toEqual([
      'Rank Two',
      'Rank Three',
      'Rank Four',
    ])

    // Restoring #1 re-promotes it incrementally in O(1) without scanning donations.
    queries.length = 0
    expect(trackedStore.moderateDonationVisibility(rank1.id, { action: 'restore' }, 220).ok).toBe(true)
    expect(queries.filter(sql => sql === PUBLIC_WALL_RANKABLE_SQL)).toHaveLength(0)
    const wallAfterRestore = JSON.parse(trackedStore.getPublicWallSnapshot().json) as PublicWallResponse
    expect(wallAfterRestore.topSupporters.map(e => e.displayName)).toEqual([
      'Rank One',
      'Rank Two',
      'Rank Three',
    ])
  })
})

describe('PublicWallProjectionStore self-healing and transaction safety', () => {
  it('self-heals missing projections, ranking fingerprint upgrades, and corrupt projection payloads', () => {
    const logs = captureLogs()
    const { store, db } = createInMemoryWallStoreWithDb()
    store.commitDonation({
      provider: 'kofi',
      externalId: 'seed-1',
      displayName: 'Alice',
      amountMinor: 25_000,
      currency: 'USD',
      minorDigits: 2,
      message: 'Hello',
      isPublic: true,
      donatedAt: 1000,
    }, 1000)
    const original = store.getPublicWallSnapshot()

    // 1. Simulate upgrading a pre-existing DB with no public_wall_projection row.
    db.prepare('DELETE FROM public_wall_projection').run()
    logs.length = 0
    const upgradedStore = createWallStoreFromDb(db)
    const rebuiltAfterUpgrade = upgradedStore.getPublicWallSnapshot()
    expect(rebuiltAfterUpgrade).toEqual(original)
    expect(logs).toContainEqual({
      level: 'info',
      payload: {
        event: 'donation.wall_projection_rebuilt',
        reason: 'missing_projection',
        wall_version: original.version,
        scanned_rows: 1,
      },
    })

    // 2. Simulate deploying updated FX rates / projection fingerprint.
    logs.length = 0
    const upgradedFingerprint = 'proj-v1-updatedfx0000000'
    const fxUpgradedQueries: string[] = []
    const fxUpgradedStore = createWallStoreFromDb(db, {
      storeOptions: { projectionFingerprint: upgradedFingerprint },
      onSqlExec: query => {
        fxUpgradedQueries.push(query)
      },
    })
    fxUpgradedQueries.length = 0
    const afterFxDeploy = fxUpgradedStore.getPublicWallSnapshot()
    expect(afterFxDeploy).toEqual(original)
    expect(fxUpgradedQueries.filter(sql => sql === PUBLIC_WALL_RANKABLE_SQL)).toHaveLength(1)
    expect(logs).toContainEqual({
      level: 'info',
      payload: {
        event: 'donation.wall_projection_rebuilt',
        reason: 'fingerprint_mismatch',
        wall_version: original.version,
        scanned_rows: 1,
      },
    })

    // Cold read with the same upgraded fingerprint now hits the persisted row in O(1).
    const secondColdQueries: string[] = []
    const secondColdStore = createWallStoreFromDb(db, {
      storeOptions: { projectionFingerprint: upgradedFingerprint },
      onSqlExec: query => {
        secondColdQueries.push(query)
      },
    })
    secondColdQueries.length = 0
    expect(secondColdStore.getPublicWallSnapshot()).toEqual(original)
    expect(secondColdQueries).toEqual([SELECT_PUBLIC_WALL_PROJECTION_SQL])

    // 3. Corrupt snapshot_json self-heals on read with a warn log.
    logs.length = 0
    db.prepare("UPDATE public_wall_projection SET snapshot_json = '' WHERE singleton_id = 1").run()
    const recoveredRead = createWallStoreFromDb(db, {
      storeOptions: { projectionFingerprint: upgradedFingerprint },
    }).getPublicWallSnapshot()
    expect(recoveredRead).toEqual(original)
    expect(logs).toContainEqual({
      level: 'warn',
      payload: {
        event: 'donation.wall_projection_rebuilt',
        reason: 'corrupt_projection',
        wall_version: original.version,
        scanned_rows: 1,
      },
    })

    // 4. Corrupt top_supporters_json self-heals on write with a warn log.
    logs.length = 0
    db.prepare("UPDATE public_wall_projection SET top_supporters_json = '{not-json' WHERE singleton_id = 1").run()
    const writeRecoveryStore = createWallStoreFromDb(db, {
      storeOptions: { projectionFingerprint: upgradedFingerprint },
    })
    writeRecoveryStore.commitDonation({
      provider: 'kofi',
      externalId: 'seed-2',
      displayName: 'Bob',
      amountMinor: 30_000,
      currency: 'USD',
      minorDigits: 2,
      message: null,
      isPublic: true,
      donatedAt: 1010,
    }, 1010)
    expect(logs).toContainEqual({
      level: 'warn',
      payload: {
        event: 'donation.wall_projection_rebuilt',
        reason: 'corrupt_projection',
        wall_version: original.version + 1,
        scanned_rows: 2,
      },
    })
    const recoveredWall = JSON.parse(writeRecoveryStore.getPublicWallSnapshot().json) as PublicWallResponse
    expect(recoveredWall.topSupporters.map(e => e.displayName)).toEqual(['Bob', 'Alice'])
  })

  it('never caches an uncommitted snapshot in memory when a write transaction rolls back', () => {
    const { db } = createInMemoryWallStoreWithDb()
    let failOnCommit = false

    const sqlPort = {
      exec(query: string, ...bindings: unknown[]): Iterable<Record<string, unknown>> {
        const stmt = db.prepare(query)
        const trimmed = query.trimStart().toUpperCase()
        if (trimmed.startsWith('SELECT')) {
          return stmt.iterate(...(bindings as never[])) as Iterable<Record<string, unknown>>
        }
        stmt.run(...(bindings as never[]))
        return []
      },
    }
    const runTx = <T,>(closure: () => T): T => {
      db.exec('BEGIN')
      try {
        const result = closure()
        if (failOnCommit) {
          throw new Error('simulated commit failure')
        }
        db.exec('COMMIT')
        return result
      } catch (error) {
        db.exec('ROLLBACK')
        throw error
      }
    }

    const store = new WallStore(sqlPort, runTx)
    const baseline = store.getPublicWallSnapshot()

    failOnCommit = true
    expect(() => store.commitDonation({
      provider: 'kofi',
      externalId: 'tx-rollback-1',
      displayName: 'Uncommitted',
      amountMinor: 99_000,
      currency: 'USD',
      minorDigits: 2,
      message: null,
      isPublic: true,
      donatedAt: 1000,
    }, 1000)).toThrow('simulated commit failure')

    failOnCommit = false
    expect(store.getPublicWallSnapshot()).toEqual(baseline)
    expect(createWallStoreFromDb(db).getPublicWallSnapshot()).toEqual(baseline)
  })
})
