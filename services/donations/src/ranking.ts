import {
  TOP_SUPPORTERS_LIMIT,
  isKnownCurrency,
  type KnownCurrency,
  type PublicWallEntry,
} from './model'

const MAX_RANKABLE_MINOR_DIGITS = 4

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

interface ScoredSupporter {
  readonly entry: PublicWallEntry
  readonly score: bigint
}

function compareScoredSupporters(a: ScoredSupporter, b: ScoredSupporter): number {
  if (a.score !== b.score) {
    return a.score > b.score ? -1 : 1
  }
  if (a.entry.donatedAt !== b.entry.donatedAt) {
    return a.entry.donatedAt - b.entry.donatedAt
  }
  if (a.entry.id === b.entry.id) return 0
  return a.entry.id < b.entry.id ? -1 : 1
}

function claimNamedSupporterSlot(
  displayName: string | null,
  seenNamed: Set<string>,
): boolean {
  if (displayName === null) return true
  const dedupKey = normalizeDonorDedupKey(displayName)
  if (dedupKey.length === 0) return true
  if (seenNamed.has(dedupKey)) return false
  seenNamed.add(dedupKey)
  return true
}

export function selectTopSupporters(
  entries: readonly PublicWallEntry[],
  limit: number = TOP_SUPPORTERS_LIMIT,
): PublicWallEntry[] {
  if (limit <= 0) return []
  const scored: ScoredSupporter[] = []
  for (const entry of entries) {
    const score = computeComparableUsdScore(entry.amountMinor, entry.currency, entry.minorDigits)
    if (score !== null) {
      scored.push({ entry, score })
    }
  }
  scored.sort(compareScoredSupporters)

  const selected: PublicWallEntry[] = []
  const seenNamed = new Set<string>()
  for (const { entry } of scored) {
    // Deduplicate named supporters by their highest single donation rather than
    // summing across orders, so unauthenticated display-name collisions cannot hijack a slot.
    if (!claimNamedSupporterSlot(entry.displayName, seenNamed)) continue
    selected.push(entry)
    if (selected.length >= limit) break
  }
  return selected
}
