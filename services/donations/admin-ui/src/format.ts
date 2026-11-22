import { MAX_CRYPTO_USD_MINOR, type DonationRecord } from '../../src/model'

const CURRENCY_SYMBOLS: Readonly<Record<string, string>> = {
  CNY: '¥',
  USD: '$',
  EUR: '€',
  GBP: '£',
  JPY: '¥',
}

const MAJOR_DECIMAL_PATTERN = /^(\d+)(?:\.(\d{1,2}))?$/

export function formatDonationMoney(
  item: Pick<DonationRecord, 'amountMinor' | 'currency' | 'minorDigits'>,
): string {
  const numeric = formatMinorDecimal(item.amountMinor, item.minorDigits)
  const symbol = CURRENCY_SYMBOLS[item.currency]
  if (symbol === undefined) {
    return `${item.currency} ${numeric}`
  }
  if (item.currency === 'JPY') {
    return `${symbol}${numeric} JPY`
  }
  return `${symbol}${numeric}`
}

export function formatMinorDecimal(amountMinor: number, minorDigits: number): string {
  if (minorDigits <= 0) {
    return String(amountMinor)
  }
  const scale = 10 ** minorDigits
  const whole = Math.floor(amountMinor / scale)
  const frac = String(amountMinor % scale).padStart(minorDigits, '0')
  return `${whole}.${frac}`
}

// Parse a 2-decimal major-unit string ("10", "10.5", "10.00") into exact minor integer units
// using string math so floating-point rounding drift is impossible.
export function parseMajorTwoDecimalToMinor(raw: string): number | null {
  const trimmed = raw.trim()
  const match = MAJOR_DECIMAL_PATTERN.exec(trimmed)
  if (match === null) return null
  const whole = match[1] ?? ''
  const frac = (match[2] ?? '').padEnd(2, '0')
  const minor = Number(`${whole}${frac}`)
  if (!Number.isSafeInteger(minor) || minor < 0 || minor > MAX_CRYPTO_USD_MINOR) {
    return null
  }
  return minor
}

export function formatDateTime(unixSec: number, locale: string): {
  readonly label: string
  readonly iso: string
} {
  const date = new Date(unixSec * 1000)
  return {
    iso: date.toISOString(),
    label: date.toLocaleString(locale, {
      dateStyle: 'medium',
      timeStyle: 'short',
    }),
  }
}

export function formatCount(value: number, locale: string): string {
  return value.toLocaleString(locale)
}
