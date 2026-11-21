import type { Language } from '../preferences/model'

export const CRYPTO_PRESET_AMOUNTS = ['5', '10', '25'] as const
export const DEFAULT_CRYPTO_AMOUNT = '10'
export const MIN_CRYPTO_USD_MINOR = 500
export const MAX_CRYPTO_USD_MINOR = 10_000_000
export const MAX_DISPLAY_NAME_CODEPOINTS = 32
export const MAX_MESSAGE_CODEPOINTS = 140

const DECIMAL_INPUT_PATTERN = /^(\d+)(?:\.(\d{1,2}))?$/
// Reserve short symbols for unambiguous primary currencies; other ISO codes (including JPY)
// fall back to explicit code prefixes so CNY ¥1,000 and JPY 1,000 cannot collide on the wall.
const CURRENCY_SYMBOLS: Readonly<Record<string, string>> = Object.freeze({
  CNY: '¥',
  USD: '$',
  EUR: '€',
  GBP: '£',
})

const MS_PER_SECOND = 1000
// ECMAScript Date valid time value bound is ±8.64e15 ms (±8.64e12 s).
export const MAX_EPOCH_SECONDS = 8_640_000_000_000
const SECONDS_PER_MINUTE = 60
const SECONDS_PER_HOUR = 3600
const SECONDS_PER_DAY = 86_400
const RELATIVE_DAY_LIMIT = 30

export type CryptoAmountValidation =
  | { readonly ok: true; readonly normalized: string; readonly amountMinor: number }
  | { readonly ok: false; readonly reason: 'invalid' | 'below_min' | 'above_max' }

export interface CryptoCheckoutDraft {
  readonly rawAmount: string
  readonly displayName: string
  readonly message: string
}

export type CryptoCheckoutValidationReason =
  | 'invalid_amount'
  | 'amount_below_min'
  | 'amount_above_max'
  | 'display_name_too_long'
  | 'message_too_long'

export type CryptoCheckoutValidation =
  | {
      readonly ok: true
      readonly amountUsd: string
      readonly displayName: string | null
      readonly message: string | null
    }
  | {
      readonly ok: false
      readonly reason: CryptoCheckoutValidationReason
    }

const AMOUNT_REASON_MAP: Readonly<Record<'invalid' | 'below_min' | 'above_max', CryptoCheckoutValidationReason>> = {
  invalid: 'invalid_amount',
  below_min: 'amount_below_min',
  above_max: 'amount_above_max',
}

function addThousandsSeparators(digits: string): string {
  let out = ''
  const len = digits.length
  for (let i = 0; i < len; i += 1) {
    if (i > 0 && (len - i) % 3 === 0) out += ','
    out += digits[i]
  }
  return out
}

export function formatDonationAmount(
  amountMinor: number,
  currency: string,
  minorDigits: number,
): string {
  const safeDigits = Number.isFinite(minorDigits)
    ? Math.max(0, Math.min(4, Math.floor(minorDigits)))
    : 0
  const safeMinor = Number.isFinite(amountMinor)
    ? Math.max(0, Math.floor(amountMinor))
    : 0
  const upperCurrency = currency.trim().toUpperCase()
  const symbol = CURRENCY_SYMBOLS[upperCurrency]
  const prefix = symbol ?? `${upperCurrency} `

  if (safeDigits === 0) {
    return `${prefix}${addThousandsSeparators(String(safeMinor))}`
  }

  const scale = 10 ** safeDigits
  const whole = Math.floor(safeMinor / scale)
  const remainder = safeMinor % scale
  const wholeText = addThousandsSeparators(String(whole))
  if (remainder === 0) {
    return `${prefix}${wholeText}`
  }
  const fractionText = String(remainder).padStart(safeDigits, '0')
  return `${prefix}${wholeText}.${fractionText}`
}

export function parseCryptoAmountInput(raw: string): CryptoAmountValidation {
  const trimmed = raw.trim()
  const match = DECIMAL_INPUT_PATTERN.exec(trimmed)
  if (match === null) {
    return { ok: false, reason: 'invalid' }
  }
  const whole = match[1] ?? '0'
  const fraction = (match[2] ?? '').padEnd(2, '0')
  const amountMinor = Number(`${whole}${fraction}`)
  if (!Number.isSafeInteger(amountMinor) || amountMinor <= 0) {
    return { ok: false, reason: 'invalid' }
  }
  if (amountMinor < MIN_CRYPTO_USD_MINOR) {
    return { ok: false, reason: 'below_min' }
  }
  if (amountMinor > MAX_CRYPTO_USD_MINOR) {
    return { ok: false, reason: 'above_max' }
  }
  const normalizedWhole = String(Math.floor(amountMinor / 100))
  const normalizedFrac = amountMinor % 100
  const normalized = normalizedFrac === 0
    ? normalizedWhole
    : `${normalizedWhole}.${String(normalizedFrac).padStart(2, '0')}`
  return { ok: true, normalized, amountMinor }
}

export function isValidEpochSeconds(value: number): boolean {
  return Number.isFinite(value) && Math.abs(value) <= MAX_EPOCH_SECONDS
}

export function formatIsoTimestamp(epochSeconds: number): string | undefined {
  if (!isValidEpochSeconds(epochSeconds)) {
    return undefined
  }
  const date = new Date(epochSeconds * MS_PER_SECOND)
  return Number.isFinite(date.getTime()) ? date.toISOString() : undefined
}

export function formatRelativeTime(
  donatedAtSec: number,
  nowSec: number,
  language: Language,
): string {
  if (!isValidEpochSeconds(donatedAtSec)) {
    return ''
  }
  const safeNow = Number.isFinite(nowSec) ? nowSec : donatedAtSec
  const elapsed = Math.max(0, Math.floor(safeNow - donatedAtSec))
  if (elapsed < SECONDS_PER_MINUTE) {
    return language === 'zh-CN' ? '刚刚' : 'just now'
  }
  if (elapsed < SECONDS_PER_HOUR) {
    const minutes = Math.floor(elapsed / SECONDS_PER_MINUTE)
    return language === 'zh-CN' ? `${minutes} 分钟前` : `${minutes}m ago`
  }
  if (elapsed < SECONDS_PER_DAY) {
    const hours = Math.floor(elapsed / SECONDS_PER_HOUR)
    return language === 'zh-CN' ? `${hours} 小时前` : `${hours}h ago`
  }
  const days = Math.floor(elapsed / SECONDS_PER_DAY)
  if (days < RELATIVE_DAY_LIMIT) {
    return language === 'zh-CN' ? `${days} 天前` : `${days}d ago`
  }
  const date = new Date(donatedAtSec * MS_PER_SECOND)
  const year = date.getUTCFullYear()
  const month = String(date.getUTCMonth() + 1).padStart(2, '0')
  const day = String(date.getUTCDate()).padStart(2, '0')
  return `${year}-${month}-${day}`
}

export function countCodePoints(value: string): number {
  return Array.from(value).length
}

export function validateCryptoCheckoutInput(draft: CryptoCheckoutDraft): CryptoCheckoutValidation {
  const amount = parseCryptoAmountInput(draft.rawAmount)
  if (!amount.ok) {
    return { ok: false, reason: AMOUNT_REASON_MAP[amount.reason] }
  }
  if (countCodePoints(draft.displayName) > MAX_DISPLAY_NAME_CODEPOINTS) {
    return { ok: false, reason: 'display_name_too_long' }
  }
  if (countCodePoints(draft.message) > MAX_MESSAGE_CODEPOINTS) {
    return { ok: false, reason: 'message_too_long' }
  }
  const trimmedName = draft.displayName.trim()
  const trimmedMessage = draft.message.trim()
  return {
    ok: true,
    amountUsd: amount.normalized,
    displayName: trimmedName.length > 0 ? trimmedName : null,
    message: trimmedMessage.length > 0 ? trimmedMessage : null,
  }
}
