import {
  KNOWN_CURRENCY_DECIMALS,
  MAX_CRYPTO_USD_MINOR,
  MIN_CRYPTO_USD_MINOR,
  type DonationConfig,
  type WallVisibility,
} from './model'

const MAX_INFERRED_MINOR_DIGITS = 4
const ISO_CURRENCY_PATTERN = /^[A-Z]{3}$/
const DECIMAL_AMOUNT_PATTERN = /^(\d+)(?:\.(\d+))?$/
const ZERO_WIDTH_PATTERN = /[\u200B-\u200D\u2060\uFEFF]/g
const COLLAPSIBLE_WHITESPACE_PATTERN = /[\r\n\t]+/g

// Linear-time patterns without nested quantifiers to stay ReDoS-safe on untrusted UGC.
const EXTERNAL_URL_PATTERN = /(?:https?:\/\/|ftp:\/\/|\bwww\.[a-z0-9-]|\b(?:t\.me|telegram\.me|discord\.gg|wa\.me|bit\.ly|tinyurl\.com)\/)/i
const EMAIL_PATTERN = /[a-z0-9._%+-]{1,64}@[a-z0-9-]{1,63}(?:\.[a-z0-9-]{1,63})+/i
const MAINLAND_MOBILE_PATTERN = /(?<!\d)1[3-9]\d(?:[\s-]?\d{4}){2}(?!\d)/

export interface ParsedMoney {
  readonly amountMinor: number
  readonly currency: string
  readonly minorDigits: number
  readonly knownCurrency: boolean
}

export interface VisibilityClassificationInput {
  readonly amountMinor: number
  readonly currency: string
  readonly displayName: string | null
  readonly message: string | null
  readonly isPublic: boolean
  readonly config: DonationConfig
}

function isDisallowedControlCode(codePoint: number): boolean {
  if (codePoint === 0x09 || codePoint === 0x0a || codePoint === 0x0d) return false
  return (codePoint >= 0x00 && codePoint <= 0x1f) || (codePoint >= 0x7f && codePoint <= 0x9f)
}

function stripControlChars(value: string): string {
  let result = ''
  for (const ch of value) {
    const codePoint = ch.codePointAt(0)
    if (codePoint !== undefined && !isDisallowedControlCode(codePoint)) {
      result += ch
    }
  }
  return result
}

export function sanitizePlainText(
  raw: string | null | undefined,
  maxCodePoints: number,
): string | null {
  if (raw === null || raw === undefined) return null
  const cleaned = stripControlChars(raw)
    .replace(ZERO_WIDTH_PATTERN, '')
    .replace(COLLAPSIBLE_WHITESPACE_PATTERN, ' ')
    .trim()
  if (cleaned.length === 0) return null
  const codePoints = Array.from(cleaned)
  const truncated = codePoints.length > maxCodePoints
    ? codePoints.slice(0, maxCodePoints).join('').trimEnd()
    : cleaned
  return truncated.length === 0 ? null : truncated
}

function trimTrailingDecimalZeros(fixed: string): string {
  if (!fixed.includes('.')) return fixed
  let end = fixed.length
  while (end > 0 && fixed[end - 1] === '0') end -= 1
  if (end > 0 && fixed[end - 1] === '.') end -= 1
  return fixed.slice(0, end)
}

function normalizeDecimalString(rawAmount: string | number): string | null {
  if (typeof rawAmount === 'number') {
    if (!Number.isFinite(rawAmount) || rawAmount <= 0) return null
    const text = rawAmount.toString()
    if (!text.includes('e') && !text.includes('E')) return text
    // Expand scientific notation without introducing float rounding drift beyond 8 decimals.
    return trimTrailingDecimalZeros(rawAmount.toFixed(8))
  }
  const trimmed = rawAmount.trim()
  return trimmed.length > 0 ? trimmed : null
}

function resolveMinorDigits(
  fraction: string,
  knownDigits: number | undefined,
): number | null {
  if (knownDigits !== undefined) {
    return fraction.length <= knownDigits ? knownDigits : null
  }
  return fraction.length <= MAX_INFERRED_MINOR_DIGITS ? fraction.length : null
}

export function parseFixedPointAmount(
  rawAmount: string | number,
  rawCurrency: string,
): ParsedMoney | null {
  const currency = rawCurrency.trim().toUpperCase()
  if (!ISO_CURRENCY_PATTERN.test(currency)) return null

  const normalized = normalizeDecimalString(rawAmount)
  if (normalized === null) return null

  const match = DECIMAL_AMOUNT_PATTERN.exec(normalized)
  if (match === null) return null

  const wholePart = match[1] ?? ''
  const fractionPart = match[2] ?? ''
  const knownDigits = KNOWN_CURRENCY_DECIMALS[currency]
  const minorDigits = resolveMinorDigits(fractionPart, knownDigits)
  if (minorDigits === null) return null

  const paddedFraction = fractionPart.padEnd(minorDigits, '0')
  const combinedDigits = `${wholePart}${paddedFraction}`
  const amountMinor = Number(combinedDigits)
  if (!Number.isSafeInteger(amountMinor) || amountMinor <= 0) return null

  return {
    amountMinor,
    currency,
    minorDigits,
    knownCurrency: knownDigits !== undefined,
  }
}

export function parseCryptoUsdAmount(rawAmount: string | number): number | null {
  const parsed = parseFixedPointAmount(rawAmount, 'USD')
  if (parsed === null) return null
  if (parsed.amountMinor < MIN_CRYPTO_USD_MINOR || parsed.amountMinor > MAX_CRYPTO_USD_MINOR) {
    return null
  }
  return parsed.amountMinor
}

export function formatUsdMinorForInvoice(usdMinor: number): string {
  const whole = Math.floor(usdMinor / 100)
  const fraction = String(usdMinor % 100).padStart(2, '0')
  return `${whole}.${fraction}`
}

export function containsExternalUrl(value: string | null): boolean {
  return value !== null && EXTERNAL_URL_PATTERN.test(value)
}

export function containsContactPii(value: string | null): boolean {
  if (value === null) return false
  return EMAIL_PATTERN.test(value) || MAINLAND_MOBILE_PATTERN.test(value)
}

function isBelowAutomaticThreshold(
  currency: string,
  amountMinor: number,
  config: DonationConfig,
): boolean {
  if (currency === 'CNY') return amountMinor < config.minPublicCnyMinor
  if (currency === 'USD') return amountMinor < config.minPublicUsdMinor
  return false
}

export function determineInitialVisibility(input: VisibilityClassificationInput): WallVisibility {
  if (!input.isPublic) {
    return { kind: 'private_donor' }
  }
  if (input.currency !== 'CNY' && input.currency !== 'USD') {
    return { kind: 'pending_review', reason: 'unsupported_currency' }
  }
  if (isBelowAutomaticThreshold(input.currency, input.amountMinor, input.config)) {
    return { kind: 'below_threshold' }
  }
  if (containsExternalUrl(input.displayName) || containsExternalUrl(input.message)) {
    return { kind: 'pending_review', reason: 'url_detected' }
  }
  if (containsContactPii(input.displayName) || containsContactPii(input.message)) {
    return { kind: 'pending_review', reason: 'pii_detected' }
  }
  if (input.config.moderationEnabled) {
    return { kind: 'pending_review', reason: 'moderation_mode' }
  }
  return { kind: 'published' }
}
