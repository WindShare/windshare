import {
  TOP_SUPPORTERS_LIMIT,
  isKnownCurrency,
  type KnownCurrency,
  type PublicWallEntry,
} from './model'

const MAX_RANKABLE_MINOR_DIGITS = 4
const RANKING_RULE_VERSION = 1

// Fixed reference micro-USD per 1 major currency unit, used solely to establish a
// stable cross-currency ordering for Top Supporters without rewriting original amounts.
export const REFERENCE_MICRO_USD_PER_MAJOR: Readonly<Record<KnownCurrency, bigint>> = Object.freeze({
  USD: 1_000_000n,
  CNY: 138_000n,
  EUR: 1_080_000n,
  GBP: 1_270_000n,
  JPY: 6_500n,
  HKD: 128_000n,
  TWD: 31_000n,
  CAD: 720_000n,
  AUD: 650_000n,
  SGD: 745_000n,
  CHF: 1_130_000n,
  KRW: 720n,
  NZD: 590_000n,
})

function compareCodeUnitStrings(a: string, b: string): number {
  if (a === b) return 0
  return a < b ? -1 : 1
}

// Canonical rule signature embedded into the persisted wall projection fingerprint so
// FX-rate updates, newly added currencies, or limit changes trigger automatic rebuilds.
export const RANKING_RULE_SIGNATURE = [
  `v${RANKING_RULE_VERSION}`,
  `limit=${TOP_SUPPORTERS_LIMIT}`,
  `maxDigits=${MAX_RANKABLE_MINOR_DIGITS}`,
  ...Object.entries(REFERENCE_MICRO_USD_PER_MAJOR)
    .toSorted(([a], [b]) => compareCodeUnitStrings(a, b))
    .map(([currency, rate]) => `${currency}:${rate.toString()}`),
].join('|')

export interface RankableSupporterEntry {
  readonly id: string
  readonly displayName: string | null
  readonly amountMinor: number
  readonly currency: string
  readonly minorDigits: number
  readonly donatedAt: number
}

// Scale every entry onto a common 10^-4 major-unit denominator so currencies with
// 0..4 minor digits compare via exact BigInt arithmetic without float rounding.
export function computeComparableUsdScore(
  amountMinor: number,
  currency: string,
  minorDigits: number,
): bigint | null {
  if (!Number.isSafeInteger(amountMinor) || amountMinor <= 0) return null
  if (
    !Number.isSafeInteger(minorDigits)
    || minorDigits < 0
    || minorDigits > MAX_RANKABLE_MINOR_DIGITS
  ) {
    return null
  }
  const normalizedCurrency = currency.trim().toUpperCase()
  if (!isKnownCurrency(normalizedCurrency)) return null
  const microUsdPerMajor = REFERENCE_MICRO_USD_PER_MAJOR[normalizedCurrency]
  const scale = 10n ** BigInt(MAX_RANKABLE_MINOR_DIGITS - minorDigits)
  return BigInt(amountMinor) * microUsdPerMajor * scale
}

export function normalizeDonorDedupKey(displayName: string): string {
  return displayName.normalize('NFKC').trim().toLowerCase()
}

interface ScoredSupporter<T extends RankableSupporterEntry> {
  readonly entry: T
  readonly score: bigint
  readonly dedupKey: string | null
}

function extractNamedDedupKey(displayName: string | null): string | null {
  if (displayName === null) return null
  const dedupKey = normalizeDonorDedupKey(displayName)
  return dedupKey.length > 0 ? dedupKey : null
}

function compareScoredSupporters<T extends RankableSupporterEntry>(
  a: ScoredSupporter<T>,
  b: ScoredSupporter<T>,
): number {
  if (a.score !== b.score) {
    return a.score > b.score ? -1 : 1
  }
  if (a.entry.donatedAt !== b.entry.donatedAt) {
    return a.entry.donatedAt - b.entry.donatedAt
  }
  if (a.entry.id === b.entry.id) return 0
  return a.entry.id < b.entry.id ? -1 : 1
}

// Single-pass bounded Top-K accumulator with named-donor deduplication.
// Because deduplication keeps each named donor's highest single donation (never summing),
// once K distinct-key (or anonymous) entries outrank a candidate, that candidate can never
// enter Top-K regardless of what entries arrive later.
export class TopSupportersAccumulator<T extends RankableSupporterEntry = PublicWallEntry> {
  private readonly limit: number
  private readonly best: ScoredSupporter<T>[] = []

  constructor(limit: number = TOP_SUPPORTERS_LIMIT) {
    this.limit = Math.max(0, limit)
  }

  add(entry: T): void {
    if (this.limit === 0) return
    const score = computeComparableUsdScore(entry.amountMinor, entry.currency, entry.minorDigits)
    if (score === null) return
    // Keep incremental merges idempotent even when an anonymous entry is re-supplied.
    if (this.best.some(item => item.entry.id === entry.id)) return

    this.insertCandidate({
      entry,
      score,
      dedupKey: extractNamedDedupKey(entry.displayName),
    })
  }

  result(): T[] {
    return this.best.map(item => item.entry)
  }

  private insertCandidate(candidate: ScoredSupporter<T>): void {
    if (candidate.dedupKey !== null) {
      const existingIndex = this.best.findIndex(item => item.dedupKey === candidate.dedupKey)
      if (existingIndex !== -1) {
        const existing = this.best[existingIndex]
        if (existing !== undefined && compareScoredSupporters(candidate, existing) < 0) {
          this.best[existingIndex] = candidate
          this.bubbleLeft(existingIndex)
        }
        return
      }
    }

    if (this.best.length < this.limit) {
      this.best.push(candidate)
      this.bubbleLeft(this.best.length - 1)
      return
    }

    const worstIndex = this.best.length - 1
    const worst = this.best[worstIndex]
    if (worst !== undefined && compareScoredSupporters(candidate, worst) < 0) {
      this.best[worstIndex] = candidate
      this.bubbleLeft(worstIndex)
    }
  }

  private bubbleLeft(startIndex: number): void {
    for (let index = startIndex; index > 0; index -= 1) {
      const current = this.best[index]
      const previous = this.best[index - 1]
      if (current === undefined || previous === undefined) break
      if (compareScoredSupporters(current, previous) >= 0) break
      this.best[index] = previous
      this.best[index - 1] = current
    }
  }
}

export function selectTopSupporters<T extends RankableSupporterEntry>(
  entries: Iterable<T>,
  limit: number = TOP_SUPPORTERS_LIMIT,
): T[] {
  if (limit <= 0) return []
  const accumulator = new TopSupportersAccumulator<T>(limit)
  for (const entry of entries) {
    accumulator.add(entry)
  }
  return accumulator.result()
}
