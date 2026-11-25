import type { DonationsEnv } from '../env'
import { adminJsonResponse, getDonationWallStub } from '../http'
import {
  logAdminManualDonationCreated,
  logAdminManualDonationReplayed,
  logAdminManualIdempotencyConflict,
} from '../log'
import {
  DONATION_PROVIDERS,
  MAX_DISPLAY_NAME_CODEPOINTS,
  MAX_MANUAL_EXTERNAL_ID_CODEPOINTS,
  MAX_MANUAL_IDEMPOTENCY_KEY_LENGTH,
  MAX_MESSAGE_CODEPOINTS,
  type CreateManualDonationInput,
  type DonationProvider,
} from '../model'
import { parseFixedPointAmount, sanitizePlainText } from '../sanitize'
import type { RuntimeDeps } from '../webhooks/handlers'

export const IDEMPOTENCY_KEY_HEADER = 'Idempotency-Key'
const MANUAL_IDEMPOTENCY_KEY_PATTERN = /^[A-Za-z0-9._:-]+$/

export type ManualDonationParseResult =
  | { readonly ok: true; readonly input: CreateManualDonationInput }
  | { readonly ok: false; readonly code: 'invalid_amount' | 'invalid_manual_donation' | 'invalid_idempotency_key' }

function parseManualProvider(raw: unknown): DonationProvider | null {
  if (raw === undefined || raw === null) return 'direct'
  if (typeof raw !== 'string') return null
  const value = raw.trim()
  return (DONATION_PROVIDERS as readonly string[]).includes(value)
    ? (value as DonationProvider)
    : null
}

function parseOptionalBoundedText(
  raw: unknown,
  maxCodePoints: number,
): { readonly ok: true; readonly value: string | null } | { readonly ok: false } {
  if (raw === undefined || raw === null) {
    return { ok: true, value: null }
  }
  if (typeof raw !== 'string') {
    return { ok: false }
  }
  // Sanitize without truncating so overlong operator input fails explicitly
  // instead of silently dropping donor notes or on-chain transaction hashes.
  const sanitized = sanitizePlainText(raw)
  if (sanitized === null) {
    return { ok: true, value: null }
  }
  if (Array.from(sanitized).length > maxCodePoints) {
    return { ok: false }
  }
  return { ok: true, value: sanitized }
}

function parseOptionalDonatedAt(
  raw: unknown,
): { readonly ok: true; readonly value: number | null } | { readonly ok: false } {
  if (raw === undefined || raw === null) {
    return { ok: true, value: null }
  }
  if (typeof raw !== 'number' || !Number.isSafeInteger(raw) || raw <= 0) {
    return { ok: false }
  }
  return { ok: true, value: raw }
}

function parseOptionalIdempotencyKey(
  raw: string | null,
): { readonly ok: true; readonly value: string | null } | { readonly ok: false } {
  if (raw === null) return { ok: true, value: null }
  const trimmed = raw.trim()
  if (
    trimmed.length === 0
    || trimmed.length > MAX_MANUAL_IDEMPOTENCY_KEY_LENGTH
    || !MANUAL_IDEMPOTENCY_KEY_PATTERN.test(trimmed)
  ) {
    return { ok: false }
  }
  return { ok: true, value: trimmed }
}

export function parseManualDonationInput(
  data: Record<string, unknown>,
  rawIdempotencyKey: string | null = null,
): ManualDonationParseResult {
  const idempotencyKey = parseOptionalIdempotencyKey(rawIdempotencyKey)
  if (!idempotencyKey.ok) {
    return { ok: false, code: 'invalid_idempotency_key' }
  }

  const provider = parseManualProvider(data.provider)
  if (provider === null) {
    return { ok: false, code: 'invalid_manual_donation' }
  }

  const rawAmount = data.amount
  const rawCurrency = data.currency
  if (
    (typeof rawAmount !== 'string' && typeof rawAmount !== 'number')
    || typeof rawCurrency !== 'string'
  ) {
    return { ok: false, code: 'invalid_amount' }
  }
  const money = parseFixedPointAmount(rawAmount, rawCurrency)
  if (money === null) {
    return { ok: false, code: 'invalid_amount' }
  }

  const displayName = parseOptionalBoundedText(data.displayName, MAX_DISPLAY_NAME_CODEPOINTS)
  const message = parseOptionalBoundedText(data.message, MAX_MESSAGE_CODEPOINTS)
  const externalId = parseOptionalBoundedText(data.externalId, MAX_MANUAL_EXTERNAL_ID_CODEPOINTS)
  const donatedAt = parseOptionalDonatedAt(data.donatedAt)
  if (!displayName.ok || !message.ok || !externalId.ok || !donatedAt.ok) {
    return { ok: false, code: 'invalid_manual_donation' }
  }

  return {
    ok: true,
    input: {
      provider,
      externalId: externalId.value,
      displayName: displayName.value,
      amountMinor: money.amountMinor,
      currency: money.currency,
      minorDigits: money.minorDigits,
      message: message.value,
      donatedAt: donatedAt.value,
      idempotencyKey: idempotencyKey.value,
    },
  }
}

export async function handleCreateManualDonation(
  data: Record<string, unknown>,
  rawIdempotencyKey: string | null,
  env: DonationsEnv,
  deps: RuntimeDeps,
): Promise<Response> {
  const parsed = parseManualDonationInput(data, rawIdempotencyKey)
  if (!parsed.ok) {
    return adminJsonResponse({ error: parsed.code }, 400)
  }

  const result = await getDonationWallStub(env).createManualDonation(parsed.input, deps.nowSec())
  if (!result.ok) {
    if (result.code === 'idempotency_key_conflict' && parsed.input.idempotencyKey !== null) {
      logAdminManualIdempotencyConflict({
        idempotencyKey: parsed.input.idempotencyKey,
        existingDonationId: result.existingDonationId,
      })
    }
    return adminJsonResponse({ error: result.code }, 409)
  }

  if (result.replayed) {
    if (parsed.input.idempotencyKey !== null) {
      logAdminManualDonationReplayed({
        donationId: result.record.id,
        provider: result.record.provider,
        externalId: result.record.externalId,
        idempotencyKey: parsed.input.idempotencyKey,
      })
    }
    return adminJsonResponse({ record: result.record, counts: result.counts }, 200)
  }

  logAdminManualDonationCreated({
    donationId: result.record.id,
    provider: result.record.provider,
    externalId: result.record.externalId,
    currency: result.record.currency,
    amountMinor: result.record.amountMinor,
    idempotencyKey: parsed.input.idempotencyKey,
  })
  return adminJsonResponse({ record: result.record, counts: result.counts }, 201)
}
