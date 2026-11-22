import { createHash, createHmac, timingSafeEqual } from 'node:crypto'

const HEX_SHA512_PATTERN = /^[0-9a-f]{128}$/i
const INTEGER_TIMESTAMP_PATTERN = /^\d+$/

export interface AfdianSignedBody {
  readonly user_id: string
  readonly params: string
  readonly ts: number
  readonly sign: string
}

export interface AfdianOrderRecord {
  readonly outTradeNo: string
  readonly userId: string
  readonly totalAmount: string
  readonly status: number
  readonly remark: string | null
  readonly donatedAt: number | null
}

// Ko-fi webhook payload projected without PII or credentials
// (email and verification_token are intentionally discarded before domain handling).
export interface KofiWebhookPayload {
  readonly kofiTransactionId: string
  readonly type: string
  readonly amount: string | number
  readonly currency: string
  readonly isPublic: boolean
  readonly fromName: string | null
  readonly message: string | null
  readonly donatedAt: number | null
}

export interface NowPaymentsIpnPayload {
  readonly paymentId: string
  readonly paymentStatus: string
  readonly orderId: string | null
  readonly priceAmount: string | number
  readonly priceCurrency: string
  readonly donatedAt: number | null
}

export function constantTimeEqual(left: string, right: string): boolean {
  // Hashing both inputs normalizes byte length so timingSafeEqual never leaks secret length.
  const leftDigest = createHash('sha256').update(left, 'utf8').digest()
  const rightDigest = createHash('sha256').update(right, 'utf8').digest()
  return timingSafeEqual(leftDigest, rightDigest)
}

export function md5Hex(value: string): string {
  // Afdian Open API requires lowercase MD5 signatures for query-order / query-sponsor.
  // eslint-disable-next-line sonarjs/hashing
  return createHash('md5').update(value, 'utf8').digest('hex')
}

export function hmacSha256Hex(secret: string, payload: string): string {
  return createHmac('sha256', secret).update(payload, 'utf8').digest('hex')
}

export function hmacSha512Hex(secret: string, payload: string): string {
  return createHmac('sha512', secret).update(payload, 'utf8').digest('hex')
}

function isPlainRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function readNonEmptyString(value: unknown): string | null {
  if (typeof value !== 'string') return null
  const trimmed = value.trim()
  return trimmed.length > 0 ? trimmed : null
}

function readOptionalString(value: unknown): string | null {
  return typeof value === 'string' ? value : null
}

function readAmountField(value: unknown): string | number | null {
  if (typeof value === 'number') return Number.isFinite(value) ? value : null
  if (typeof value === 'string') {
    const trimmed = value.trim()
    return trimmed.length > 0 ? trimmed : null
  }
  return null
}

export function parseIsoTimestampSeconds(raw: unknown): number | null {
  if (typeof raw !== 'string') return null
  const trimmed = raw.trim()
  if (trimmed.length === 0) return null
  const millis = Date.parse(trimmed)
  if (!Number.isFinite(millis) || millis <= 0) return null
  return Math.floor(millis / 1000)
}

export function parseUnixTimestampSeconds(raw: unknown): number | null {
  if (typeof raw === 'number') {
    return Number.isSafeInteger(raw) && raw > 0 ? raw : null
  }
  if (typeof raw === 'string') {
    const trimmed = raw.trim()
    if (!INTEGER_TIMESTAMP_PATTERN.test(trimmed)) return null
    const parsed = Number(trimmed)
    return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : null
  }
  return null
}

export function buildAfdianSignedBody(
  userId: string,
  token: string,
  params: Record<string, unknown>,
  tsSeconds: number,
): AfdianSignedBody {
  const paramsJson = JSON.stringify(params)
  const sign = md5Hex(`${token}params${paramsJson}ts${tsSeconds}user_id${userId}`)
  return {
    user_id: userId,
    params: paramsJson,
    ts: tsSeconds,
    sign,
  }
}

export function parseAfdianWebhookPayload(rawJson: unknown): { readonly outTradeNo: string } | null {
  if (!isPlainRecord(rawJson)) return null
  if (!isPlainRecord(rawJson.data)) return null
  if (!isPlainRecord(rawJson.data.order)) return null
  const outTradeNo = readNonEmptyString(rawJson.data.order.out_trade_no)
  if (outTradeNo === null) return null
  return { outTradeNo }
}

function parseSingleAfdianOrder(item: unknown): AfdianOrderRecord | null {
  if (!isPlainRecord(item)) return null
  const outTradeNo = readNonEmptyString(item.out_trade_no)
  const userId = readNonEmptyString(item.user_id) ?? ''
  const totalAmount = readAmountField(item.total_amount)
  const status = typeof item.status === 'number' ? item.status : Number(item.status)
  if (outTradeNo === null || totalAmount === null || !Number.isInteger(status)) {
    return null
  }
  return {
    outTradeNo,
    userId,
    totalAmount: String(totalAmount),
    status,
    remark: readOptionalString(item.remark),
    donatedAt: parseUnixTimestampSeconds(item.create_time),
  }
}

export function parseAfdianOrderQueryResponse(
  rawJson: unknown,
): { readonly ok: true; readonly orders: readonly AfdianOrderRecord[] } | { readonly ok: false; readonly reason: string } {
  if (!isPlainRecord(rawJson)) return { ok: false, reason: 'invalid_json' }
  if (rawJson.ec !== 200) return { ok: false, reason: `afdian_ec_${String(rawJson.ec)}` }
  if (!isPlainRecord(rawJson.data) || !Array.isArray(rawJson.data.list)) {
    return { ok: false, reason: 'missing_order_list' }
  }
  const orders: AfdianOrderRecord[] = []
  for (const item of rawJson.data.list) {
    const parsed = parseSingleAfdianOrder(item)
    if (parsed !== null) orders.push(parsed)
  }
  return { ok: true, orders }
}

function extractSponsorEntry(
  item: unknown,
): { readonly userId: string; readonly displayName: string | null } | null {
  if (!isPlainRecord(item)) return null
  const userObj = isPlainRecord(item.user) ? item.user : null
  const userId = readNonEmptyString(userObj?.user_id) ?? readNonEmptyString(item.user_id)
  if (userId === null) return null
  const displayName = readNonEmptyString(userObj?.name) ?? readNonEmptyString(item.user_name)
  return { userId, displayName }
}

export function parseAfdianSponsorPage(
  rawJson: unknown,
): { readonly ok: true; readonly sponsors: ReadonlyMap<string, string | null> }
  | { readonly ok: false; readonly reason: string } {
  if (!isPlainRecord(rawJson)) return { ok: false, reason: 'invalid_json' }
  if (rawJson.ec !== 200) return { ok: false, reason: `afdian_ec_${String(rawJson.ec)}` }
  if (!isPlainRecord(rawJson.data) || !Array.isArray(rawJson.data.list)) {
    return { ok: false, reason: 'missing_sponsor_list' }
  }
  const sponsors = new Map<string, string | null>()
  for (const item of rawJson.data.list) {
    const entry = extractSponsorEntry(item)
    if (entry !== null && !sponsors.has(entry.userId)) {
      sponsors.set(entry.userId, entry.displayName)
    }
  }
  return { ok: true, sponsors }
}

export function parseAfdianSponsorName(
  rawJson: unknown,
  targetUserId: string,
): { readonly ok: true; readonly matched: boolean; readonly displayName: string | null }
  | { readonly ok: false; readonly reason: string } {
  const parsed = parseAfdianSponsorPage(rawJson)
  if (!parsed.ok) return parsed
  if (!parsed.sponsors.has(targetUserId)) {
    return { ok: true, matched: false, displayName: null }
  }
  return { ok: true, matched: true, displayName: parsed.sponsors.get(targetUserId) ?? null }
}

export function parseKofiFormDataObject(rawFormBody: string): Record<string, unknown> | null {
  const params = new URLSearchParams(rawFormBody)
  const rawData = params.get('data')
  if (rawData === null || rawData.trim().length === 0) return null
  try {
    const parsed: unknown = JSON.parse(rawData)
    return isPlainRecord(parsed) ? parsed : null
  } catch {
    return null
  }
}

export function verifyKofiWebhookToken(
  rawObject: Record<string, unknown>,
  expectedToken: string,
): boolean {
  const payloadToken = readNonEmptyString(rawObject.verification_token)
  if (payloadToken === null) return false
  return constantTimeEqual(payloadToken, expectedToken)
}

export function parseKofiWebhookPayload(
  rawObject: Record<string, unknown>,
): { readonly ok: true; readonly payload: KofiWebhookPayload } | { readonly ok: false; readonly reason: 'invalid_payload' } {
  const kofiTransactionId = readNonEmptyString(rawObject.kofi_transaction_id)
  const type = readNonEmptyString(rawObject.type)
  const amount = readAmountField(rawObject.amount)
  const currency = readNonEmptyString(rawObject.currency)
  if (
    kofiTransactionId === null
    || type === null
    || amount === null
    || currency === null
  ) {
    return { ok: false, reason: 'invalid_payload' }
  }

  // Default to private if is_public is absent or non-boolean so privacy fails closed.
  const isPublic = rawObject.is_public === true

  return {
    ok: true,
    payload: {
      kofiTransactionId,
      type,
      amount,
      currency,
      isPublic,
      fromName: readOptionalString(rawObject.from_name),
      message: readOptionalString(rawObject.message),
      donatedAt: parseIsoTimestampSeconds(rawObject.timestamp),
    },
  }
}

export function isSupportedKofiType(type: string): boolean {
  // Ko-fi's webhook docs rebranded one-off donations to "Tip" while live payloads still send "Donation".
  return type === 'Donation' || type === 'Tip' || type === 'Subscription'
}

export function sortJsonKeysRecursively(value: unknown): unknown {
  if (Array.isArray(value)) {
    return value.map(sortJsonKeysRecursively)
  }
  if (!isPlainRecord(value)) {
    return value
  }
  // NOWPayments uses default JS UTF-16 code-unit sort (`Object.keys(obj).sort()`).
  const codeUnitKeys = Object.keys(value).sort((left, right) => {
    if (left < right) return -1
    if (left > right) return 1
    return 0
  })
  // Object.fromEntries defines own data properties so "__proto__" keys do not
  // invoke Object.prototype.__proto__'s setter or get dropped from JSON.stringify.
  return Object.fromEntries(
    codeUnitKeys.map((key): [string, unknown] => [key, sortJsonKeysRecursively(value[key])]),
  )
}

export function computeNowPaymentsSignature(
  payloadObject: Record<string, unknown>,
  ipnSecret: string,
): string {
  const canonical = JSON.stringify(sortJsonKeysRecursively(payloadObject))
  return hmacSha512Hex(ipnSecret, canonical)
}

export function verifyNowPaymentsSignature(
  payloadObject: Record<string, unknown>,
  signatureHeader: string,
  ipnSecret: string,
): boolean {
  const trimmed = signatureHeader.trim()
  if (!HEX_SHA512_PATTERN.test(trimmed)) return false
  const expected = computeNowPaymentsSignature(payloadObject, ipnSecret)
  return constantTimeEqual(expected, trimmed.toLowerCase())
}

function readPaymentId(value: unknown): string | null {
  if (typeof value === 'number' && Number.isSafeInteger(value) && value > 0) {
    return String(value)
  }
  return readNonEmptyString(value)
}

export function parseNowPaymentsIpnPayload(
  rawObject: Record<string, unknown>,
): { readonly ok: true; readonly ipn: NowPaymentsIpnPayload } | { readonly ok: false; readonly reason: 'invalid_payload' } {
  const paymentId = readPaymentId(rawObject.payment_id)
  const paymentStatus = readNonEmptyString(rawObject.payment_status)
  const priceAmount = readAmountField(rawObject.price_amount)
  const priceCurrency = readNonEmptyString(rawObject.price_currency)
  if (
    paymentId === null
    || paymentStatus === null
    || priceAmount === null
    || priceCurrency === null
  ) {
    return { ok: false, reason: 'invalid_payload' }
  }

  const donatedAt = parseIsoTimestampSeconds(rawObject.updated_at)
    ?? parseIsoTimestampSeconds(rawObject.created_at)

  return {
    ok: true,
    ipn: {
      paymentId,
      paymentStatus: paymentStatus.toLowerCase(),
      orderId: readNonEmptyString(rawObject.order_id),
      priceAmount,
      priceCurrency,
      donatedAt,
    },
  }
}
