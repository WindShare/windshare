import { describe, expect, it } from 'vitest'
import { parseManualDonationInput } from '../src/admin/manual'
import {
  KNOWN_CURRENCIES,
  KNOWN_CURRENCY_DECIMALS,
  PUBLIC_WALL_LIMIT,
  type PublicWallEntry,
  type PublicWallResponse,
} from '../src/model'
import {
  RANKING_RULE_SIGNATURE,
  REFERENCE_MICRO_USD_PER_MAJOR,
  TopSupportersAccumulator,
  computeComparableUsdScore,
  normalizeDonorDedupKey,
  selectTopSupporters,
} from '../src/ranking'
import { computePublicWallProjectionFingerprint } from '../src/wall-projection'
import { createInMemoryWallStore } from './wall-harness'

function makeEntry(overrides: Partial<PublicWallEntry> & { readonly id: string }): PublicWallEntry {
  return {
    provider: 'direct',
    displayName: 'Supporter',
    amountMinor: 1000,
    currency: 'USD',
    minorDigits: 2,
    message: null,
    donatedAt: 1_700_000_000,
    ...overrides,
  }
}

describe('top supporters ranking semantics', () => {
  it('computes exact BigInt comparable scores across currencies and minor digit precisions', () => {
    // $100.00 USD (2 digits) == $100 (0 digits)
    expect(computeComparableUsdScore(10_000, 'USD', 2)).toBe(100_000_000_000_0n)
    expect(computeComparableUsdScore(100, 'USD', 0)).toBe(100_000_000_000_0n)

    // ¥1,000 CNY (~$138) > €100 EUR (~$108) > $100 USD > 10,000 JPY (~$65)
    const cny1000 = computeComparableUsdScore(100_000, 'CNY', 2)
    const eur100 = computeComparableUsdScore(10_000, 'EUR', 2)
    const usd100 = computeComparableUsdScore(10_000, 'USD', 2)
    const jpy10000 = computeComparableUsdScore(10_000, 'JPY', 0)
    expect(cny1000).not.toBeNull()
    expect(eur100).not.toBeNull()
    expect(usd100).not.toBeNull()
    expect(jpy10000).not.toBeNull()
    expect(cny1000! > eur100!).toBe(true)
    expect(eur100! > usd100!).toBe(true)
    expect(usd100! > jpy10000!).toBe(true)

    // CAD with 0, 2, and 4 minor digits representing 50 CAD produce identical scores.
    expect(computeComparableUsdScore(50, 'cad', 0)).toBe(computeComparableUsdScore(5000, 'CAD', 2))
    expect(computeComparableUsdScore(500_000, 'CAD', 4)).toBe(computeComparableUsdScore(5000, 'CAD', 2))

    // Reject unknown currencies, zero/negative amounts, and out-of-range minorDigits.
    expect(computeComparableUsdScore(1000, 'XYZ', 2)).toBeNull()
    expect(computeComparableUsdScore(0, 'USD', 2)).toBeNull()
    expect(computeComparableUsdScore(-100, 'USD', 2)).toBeNull()
    expect(computeComparableUsdScore(1000, 'USD', -1)).toBeNull()
    expect(computeComparableUsdScore(1000, 'USD', 5)).toBeNull()

    // Known currency catalog, minor-unit precision map, and ranking rate table share one exact key set.
    expect(Object.keys(KNOWN_CURRENCY_DECIMALS)).toEqual([...KNOWN_CURRENCIES])
    expect(Object.keys(REFERENCE_MICRO_USD_PER_MAJOR)).toEqual([...KNOWN_CURRENCIES])
    expect(computeComparableUsdScore(100_000, 'KRW', 0)).toBe(720_000_000_000n)
    expect(computeComparableUsdScore(10_000, 'NZD', 2)).toBe(590_000_000_000n)
  })

  it('deduplicates named supporters by NFKC case-insensitive key keeping highest single order while keeping anonymous independent', () => {
    expect(normalizeDonorDedupKey('  Ａｌｉｃｅ  ')).toBe('alice')

    const entries: PublicWallEntry[] = [
      // Same person with case + full-width variants: keep only $200, never sum to $350.
      makeEntry({ id: 'alice-small', displayName: 'alice', amountMinor: 15_000, currency: 'USD', donatedAt: 100 }),
      makeEntry({ id: 'alice-big', displayName: 'Ａｌｉｃｅ', amountMinor: 20_000, currency: 'USD', donatedAt: 200 }),
      // Earlier tie at $200 wins over later $200.
      makeEntry({ id: 'bob-earlier', displayName: 'Bob', amountMinor: 20_000, currency: 'USD', donatedAt: 150 }),
      // Anonymous entries stand independently and both can rank.
      makeEntry({ id: 'anon-1', displayName: null, amountMinor: 18_000, currency: 'USD', donatedAt: 120 }),
      makeEntry({ id: 'anon-2', displayName: null, amountMinor: 17_000, currency: 'USD', donatedAt: 130 }),
      // Unknown currency is excluded from ranking.
      makeEntry({ id: 'unknown-huge', displayName: 'Mystery', amountMinor: 999_999_99, currency: 'ZZZ', donatedAt: 50 }),
    ]

    const top3 = selectTopSupporters(entries)
    expect(top3.map(e => e.id)).toEqual(['bob-earlier', 'alice-big', 'anon-1'])

    const top5 = selectTopSupporters(entries, 5)
    expect(top5.map(e => e.id)).toEqual(['bob-earlier', 'alice-big', 'anon-1', 'anon-2'])
  })

  it('breaks exact amount and timestamp ties deterministically by id ASC', () => {
    const top = selectTopSupporters([
      makeEntry({ id: 'id-c', displayName: 'Carol', amountMinor: 5000, donatedAt: 1000 }),
      makeEntry({ id: 'id-a', displayName: 'Alice', amountMinor: 5000, donatedAt: 1000 }),
      makeEntry({ id: 'id-b', displayName: 'Bob', amountMinor: 5000, donatedAt: 1000 }),
      makeEntry({ id: 'id-d', displayName: 'Dave', amountMinor: 5000, donatedAt: 1000 }),
    ])
    expect(top.map(e => e.id)).toEqual(['id-a', 'id-b', 'id-c'])
  })

  it('matches full sort-and-dedup reference results across arbitrary orderings and stays idempotent on duplicate ids', () => {
    const referenceSelect = (entries: readonly PublicWallEntry[], limit: number): PublicWallEntry[] => {
      if (limit <= 0) return []
      const scored = entries
        .map(entry => ({
          entry,
          score: computeComparableUsdScore(entry.amountMinor, entry.currency, entry.minorDigits),
        }))
        .filter((item): item is { entry: PublicWallEntry; score: bigint } => item.score !== null)
        .toSorted((a, b) => {
          if (a.score !== b.score) return a.score > b.score ? -1 : 1
          if (a.entry.donatedAt !== b.entry.donatedAt) return a.entry.donatedAt - b.entry.donatedAt
          if (a.entry.id === b.entry.id) return 0
          return a.entry.id < b.entry.id ? -1 : 1
        })
      const seenNamed = new Set<string>()
      const result: PublicWallEntry[] = []
      for (const { entry } of scored) {
        const key = entry.displayName === null ? '' : normalizeDonorDedupKey(entry.displayName)
        if (key.length > 0) {
          if (seenNamed.has(key)) continue
          seenNamed.add(key)
        }
        result.push(entry)
        if (result.length >= limit) break
      }
      return result
    }

    // Deterministic LCG to test varied permutations without flaky randomness.
    let state = 0x5eed1234
    const nextInt = (maxExclusive: number): number => {
      state = (Math.imul(state, 1_664_525) + 1_013_904_223) >>> 0
      return state % maxExclusive
    }
    const namePool: readonly (string | null)[] = [null, 'Alice', 'Ａｌｉｃｅ', '  alice ', 'Bob', 'BOB', 'Carol', 'Dave', '   ']
    const currencyPool = ['USD', 'CNY', 'EUR', 'JPY', 'KRW', 'ZZZ'] as const

    for (let trial = 0; trial < 120; trial += 1) {
      const count = 1 + nextInt(25)
      const dataset: PublicWallEntry[] = []
      for (let i = 0; i < count; i += 1) {
        const currency = currencyPool[nextInt(currencyPool.length)] ?? 'USD'
        const minorDigits = currency === 'JPY' || currency === 'KRW' ? 0 : 2
        dataset.push(makeEntry({
          id: `t${trial}-e${String(i).padStart(2, '0')}`,
          displayName: namePool[nextInt(namePool.length)] ?? null,
          amountMinor: (1 + nextInt(12)) * 500,
          currency,
          minorDigits,
          donatedAt: 1_700_000_000 + nextInt(6),
        }))
      }
      for (const limit of [0, 1, 2, 3, 5]) {
        expect(selectTopSupporters(dataset, limit).map(e => e.id)).toEqual(
          referenceSelect(dataset, limit).map(e => e.id),
        )
      }
    }

    // Re-adding an identical anonymous entry must not claim two Top Supporter slots.
    const anon = makeEntry({ id: 'anon-same', displayName: null, amountMinor: 50_000, donatedAt: 100 })
    const acc = new TopSupportersAccumulator(3)
    for (const item of [anon, anon]) {
      acc.add(item)
    }
    expect(acc.result().map(e => e.id)).toEqual(['anon-same'])

    // Projection fingerprint is deterministic and sensitive to ranking-rule changes.
    const defaultFp = computePublicWallProjectionFingerprint()
    expect(defaultFp).toMatch(/^proj-v1-[0-9a-f]{16}$/)
    expect(computePublicWallProjectionFingerprint(RANKING_RULE_SIGNATURE)).toBe(defaultFp)
    expect(computePublicWallProjectionFingerprint(`${RANKING_RULE_SIGNATURE}|CNY:140000`)).not.toBe(defaultFp)
  })
})

describe('WallStore long-term Top 3 persistence and manual donations', () => {
  it('retains all-time Top 3 donors beyond the 50-entry recent wall cap and updates on hide/restore', () => {
    const store = createInMemoryWallStore()

    // Early large donor who will fall outside the latest 50 entries.
    const founding = store.commitDonation({
      provider: 'afdian',
      externalId: 'EARLY-BIG',
      displayName: 'Founding Patron',
      amountMinor: 88_800,
      currency: 'CNY',
      minorDigits: 2,
      message: 'Keep shipping!',
      isPublic: true,
      donatedAt: 1000,
    }, 1000)

    // Manual direct crypto donor (#1 overall).
    const whale = store.createManualDonation({
      provider: 'direct',
      externalId: '0xabc123',
      displayName: 'OnChain Whale',
      amountMinor: 25_000,
      currency: 'USD',
      minorDigits: 2,
      message: 'Direct USDT transfer',
      donatedAt: 1010,
      idempotencyKey: null,
    }, 1010)
    expect(whale.ok).toBe(true)
    if (!whale.ok) throw new Error('expected manual donation')

    // Duplicate (provider, externalId) rejected cleanly.
    const dup = store.createManualDonation({
      provider: 'direct',
      externalId: '0xabc123',
      displayName: 'Duplicate',
      amountMinor: 1000,
      currency: 'USD',
      minorDigits: 2,
      message: null,
      donatedAt: 1020,
      idempotencyKey: null,
    }, 1020)
    expect(dup).toEqual({ ok: false, code: 'duplicate_external_id' })

    // Push 55 newer small donations so founding & whale drop off recent entries[50].
    for (let i = 0; i < PUBLIC_WALL_LIMIT + 5; i += 1) {
      store.commitDonation({
        provider: 'kofi',
        externalId: `small-${i}`,
        displayName: `Recent ${i}`,
        amountMinor: 500 + (i === 10 ? 5000 : 0), // Recent 10 gave $55 (#3)
        currency: 'USD',
        minorDigits: 2,
        message: null,
        isPublic: true,
        donatedAt: 2000 + i,
      }, 2000 + i)
    }

    const wall = JSON.parse(store.getPublicWallSnapshot().json) as PublicWallResponse
    expect(wall.entries).toHaveLength(PUBLIC_WALL_LIMIT)
    expect(wall.entries.some(e => e.id === founding.record.id)).toBe(false)
    expect(wall.entries.some(e => e.id === whale.record.id)).toBe(false)
    expect(wall.topSupporters.map(e => e.displayName)).toEqual([
      'OnChain Whale',
      'Founding Patron',
      'Recent 10',
    ])

    // Hiding #1 immediately removes it from Top 3 and promotes the next earliest $5 donor.
    const hidden = store.moderateDonationVisibility(whale.record.id, { action: 'hide', reason: 'test' }, 3000)
    expect(hidden.ok).toBe(true)
    const wallAfterHide = JSON.parse(store.getPublicWallSnapshot().json) as PublicWallResponse
    expect(wallAfterHide.topSupporters.map(e => e.displayName)).toEqual([
      'Founding Patron',
      'Recent 10',
      'Recent 0',
    ])

    // Restoring #1 brings it back to #1.
    const restored = store.moderateDonationVisibility(whale.record.id, { action: 'restore' }, 3010)
    expect(restored.ok).toBe(true)
    const wallAfterRestore = JSON.parse(store.getPublicWallSnapshot().json) as PublicWallResponse
    expect(wallAfterRestore.topSupporters.map(e => e.displayName)).toEqual([
      'OnChain Whale',
      'Founding Patron',
      'Recent 10',
    ])
  })

  it('validates manual donation payloads strictly', () => {
    expect(parseManualDonationInput({ amount: '100', currency: 'usd' })).toEqual({
      ok: true,
      input: {
        provider: 'direct',
        externalId: null,
        displayName: null,
        amountMinor: 10_000,
        currency: 'USD',
        minorDigits: 2,
        message: null,
        donatedAt: null,
        idempotencyKey: null,
      },
    })
    expect(parseManualDonationInput({ amount: '50000', currency: 'krw' })).toEqual({
      ok: true,
      input: {
        provider: 'direct',
        externalId: null,
        displayName: null,
        amountMinor: 50_000,
        currency: 'KRW',
        minorDigits: 0,
        message: null,
        donatedAt: null,
        idempotencyKey: null,
      },
    })
    expect(parseManualDonationInput({ amount: '50000.00', currency: 'KRW' })).toEqual({
      ok: true,
      input: {
        provider: 'direct',
        externalId: null,
        displayName: null,
        amountMinor: 50_000,
        currency: 'KRW',
        minorDigits: 0,
        message: null,
        donatedAt: null,
        idempotencyKey: null,
      },
    })
    expect(parseManualDonationInput({ amount: '50000.5', currency: 'KRW' })).toEqual({
      ok: false,
      code: 'invalid_amount',
    })
    expect(parseManualDonationInput({ amount: '80', currency: 'NZD' })).toEqual({
      ok: true,
      input: {
        provider: 'direct',
        externalId: null,
        displayName: null,
        amountMinor: 8_000,
        currency: 'NZD',
        minorDigits: 2,
        message: null,
        donatedAt: null,
        idempotencyKey: null,
      },
    })

    expect(parseManualDonationInput({ amount: '0', currency: 'USD' })).toEqual({
      ok: false,
      code: 'invalid_amount',
    })
    expect(parseManualDonationInput({ amount: '10', currency: 'USD', provider: 'bogus' })).toEqual({
      ok: false,
      code: 'invalid_manual_donation',
    })
    expect(parseManualDonationInput({
      amount: '10',
      currency: 'USD',
      displayName: 'a'.repeat(33),
    })).toEqual({
      ok: false,
      code: 'invalid_manual_donation',
    })
    expect(parseManualDonationInput({
      amount: '10',
      currency: 'USD',
      donatedAt: -1,
    })).toEqual({
      ok: false,
      code: 'invalid_manual_donation',
    })
  })
})
