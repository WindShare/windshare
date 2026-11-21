import type { DonationsEnv } from '../env'
import {
  DONATION_WALL_INSTANCE_NAME,
  MAX_WEBHOOK_BODY_BYTES,
  hasMediaType,
  jsonResponse,
  parseJsonObject,
  readBoundedText,
} from '../http'
import {
  logAfdianOrderSkipped,
  logAfdianSponsorLookupFallback,
  logDonationCommitted,
  logNowPaymentsIntentFallback,
  logNowPaymentsPartiallyPaid,
  logWebhookReceived,
  logWebhookRejected,
} from '../log'
import {
  MAX_DISPLAY_NAME_CODEPOINTS,
  MAX_MESSAGE_CODEPOINTS,
  type CommitDonationInput,
  type CommitDonationResult,
  type CryptoCheckoutIntent,
  type DonationProvider,
} from '../model'
import {
  lookupAfdianSponsorName,
  queryAfdianOrders,
  type FetchLike,
} from '../providers'
import { parseFixedPointAmount, sanitizePlainText, type ParsedMoney } from '../sanitize'
import {
  constantTimeEqual,
  isSupportedKofiType,
  parseAfdianWebhookPayload,
  parseKofiWebhookBody,
  parseNowPaymentsIpnPayload,
  verifyKofiToken,
  verifyNowPaymentsSignature,
  type AfdianOrderRecord,
} from '../verify'

const AFDIAN_PAID_STATUS = 2
const NO_STORE_HEADERS = Object.freeze({ 'Cache-Control': 'no-store' })

export interface RuntimeDeps {
  readonly fetchFn: FetchLike
  readonly nowSec: () => number
}

function getWallStub(env: DonationsEnv): DurableObjectStub {
  return env.DONATION_WALL.get(env.DONATION_WALL.idFromName(DONATION_WALL_INSTANCE_NAME))
}

async function donationExistsInWall(
  env: DonationsEnv,
  provider: DonationProvider,
  externalId: string,
): Promise<boolean> {
  const stub = getWallStub(env)
  const query = new URLSearchParams({ provider, externalId }).toString()
  const response = await stub.fetch(`https://donation-wall.internal/internal/donations/exists?${query}`)
  if (!response.ok) return false
  const body = await response.json() as { exists: boolean }
  return body.exists
}

export async function findExistingDonationsInWall(
  env: DonationsEnv,
  provider: DonationProvider,
  externalIds: readonly string[],
): Promise<Set<string>> {
  if (externalIds.length === 0) return new Set()
  const stub = getWallStub(env)
  const response = await stub.fetch('https://donation-wall.internal/internal/donations/exists-batch', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ provider, externalIds }),
  })
  if (!response.ok) {
    throw new Error(`Donation batch existence check failed with status ${response.status}`)
  }
  const body = await response.json() as { existingIds: string[] }
  return new Set(body.existingIds)
}

function logCommitResult(result: CommitDonationResult): void {
  logDonationCommitted({
    donationId: result.record.id,
    provider: result.record.provider,
    externalId: result.record.externalId,
    currency: result.record.currency,
    amountMinor: result.record.amountMinor,
    visibility: result.record.visibility.kind,
    deduplicated: result.deduplicated,
  })
}

async function commitDonationToWall(
  env: DonationsEnv,
  input: CommitDonationInput,
  nowSec: number,
): Promise<CommitDonationResult> {
  const stub = getWallStub(env)
  const response = await stub.fetch('https://donation-wall.internal/internal/donations/commit', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ input, nowSec }),
  })
  if (!response.ok) {
    throw new Error(`Donation commit failed with status ${response.status}`)
  }
  const result = await response.json() as CommitDonationResult
  logCommitResult(result)
  return result
}

export async function commitDonationsBatchToWall(
  env: DonationsEnv,
  inputs: readonly CommitDonationInput[],
  nowSec: number,
): Promise<CommitDonationResult[]> {
  if (inputs.length === 0) return []
  const stub = getWallStub(env)
  const response = await stub.fetch('https://donation-wall.internal/internal/donations/commit-batch', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ inputs, nowSec }),
  })
  if (!response.ok) {
    throw new Error(`Donation batch commit failed with status ${response.status}`)
  }
  const body = await response.json() as { results: CommitDonationResult[] }
  for (const result of body.results) {
    logCommitResult(result)
  }
  return body.results
}

export function toAfdianCommitInput(
  order: AfdianOrderRecord,
  money: ParsedMoney,
  rawDisplayName: string | null,
): CommitDonationInput {
  return {
    provider: 'afdian',
    externalId: order.outTradeNo,
    displayName: sanitizePlainText(rawDisplayName, MAX_DISPLAY_NAME_CODEPOINTS),
    amountMinor: money.amountMinor,
    currency: money.currency,
    minorDigits: money.minorDigits,
    message: sanitizePlainText(order.remark, MAX_MESSAGE_CODEPOINTS),
    isPublic: true,
    donatedAt: order.donatedAt,
  }
}

type CryptoIntentLookupResult =
  | { readonly kind: 'found'; readonly intent: CryptoCheckoutIntent }
  | { readonly kind: 'missing' }
  | { readonly kind: 'lookup_failed'; readonly status: number }

async function lookupCryptoIntent(
  env: DonationsEnv,
  intentId: string,
  nowSec: number,
): Promise<CryptoIntentLookupResult> {
  const stub = getWallStub(env)
  const response = await stub.fetch(
    `https://donation-wall.internal/internal/crypto-intents/${encodeURIComponent(intentId)}?nowSec=${nowSec}`,
  )
  if (!response.ok) {
    return { kind: 'lookup_failed', status: response.status }
  }
  const body = await response.json() as { intent: CryptoCheckoutIntent | null }
  if (body.intent === null) {
    return { kind: 'missing' }
  }
  return { kind: 'found', intent: body.intent }
}

async function resolveNowPaymentsIntent(
  env: DonationsEnv,
  paymentId: string,
  orderId: string | null,
  nowSec: number,
): Promise<CryptoCheckoutIntent | null> {
  if (orderId === null) return null
  const lookup = await lookupCryptoIntent(env, orderId, nowSec)
  if (lookup.kind === 'found') return lookup.intent
  if (lookup.kind === 'missing') {
    logNowPaymentsIntentFallback({
      paymentId,
      orderId,
      reason: 'not_found_or_expired',
    })
    return null
  }
  logNowPaymentsIntentFallback({
    paymentId,
    orderId,
    reason: 'lookup_failed',
    upstreamStatus: lookup.status,
  })
  return null
}

function afdianAckResponse(): Response {
  return jsonResponse({ ec: 200, em: '' }, 200, NO_STORE_HEADERS)
}

async function commitVerifiedAfdianOrder(
  env: DonationsEnv,
  order: AfdianOrderRecord,
  afdianToken: string,
  deps: RuntimeDeps,
): Promise<CommitDonationResult | null> {
  const money = parseFixedPointAmount(order.totalAmount, 'CNY')
  if (money === null) {
    logWebhookRejected({ provider: 'afdian', reason: 'invalid_payload' })
    return null
  }

  let rawDisplayName: string | null = null
  if (order.userId.length > 0) {
    const sponsor = await lookupAfdianSponsorName({
      userId: env.AFDIAN_USER_ID,
      token: afdianToken,
      sponsorUserId: order.userId,
      nowSec: deps.nowSec(),
      fetchFn: deps.fetchFn,
    })
    if (sponsor.ok) {
      rawDisplayName = sponsor.displayName
    } else {
      logAfdianSponsorLookupFallback({
        outTradeNo: order.outTradeNo,
        reason: sponsor.reason,
      })
    }
  }

  return commitDonationToWall(
    env,
    toAfdianCommitInput(order, money, rawDisplayName),
    deps.nowSec(),
  )
}

export async function handleAfdianWebhook(
  request: Request,
  env: DonationsEnv,
  providedSecret: string,
  deps: RuntimeDeps,
): Promise<Response> {
  if (!env.AFDIAN_WEBHOOK_SECRET || !env.AFDIAN_TOKEN) {
    return jsonResponse({ error: 'provider_not_configured' }, 503, NO_STORE_HEADERS)
  }
  if (!constantTimeEqual(providedSecret, env.AFDIAN_WEBHOOK_SECRET)) {
    logWebhookRejected({ provider: 'afdian', reason: 'invalid_signature' })
    return jsonResponse({ error: 'forbidden' }, 403, NO_STORE_HEADERS)
  }
  if (!hasMediaType(request, 'application/json')) {
    logWebhookRejected({ provider: 'afdian', reason: 'invalid_payload' })
    return jsonResponse({ error: 'unsupported_media_type' }, 400, NO_STORE_HEADERS)
  }

  const bodyResult = await readBoundedText(request, MAX_WEBHOOK_BODY_BYTES)
  if (!bodyResult.ok) {
    logWebhookRejected({ provider: 'afdian', reason: 'body_too_large' })
    return jsonResponse({ error: 'body_too_large' }, 413, NO_STORE_HEADERS)
  }

  const parsedJson = parseJsonObject(bodyResult.text)
  const webhook = parseAfdianWebhookPayload(parsedJson)
  if (webhook === null) {
    logWebhookRejected({ provider: 'afdian', reason: 'invalid_payload' })
    return jsonResponse({ error: 'invalid_payload' }, 400, NO_STORE_HEADERS)
  }

  logWebhookReceived({
    provider: 'afdian',
    externalId: webhook.outTradeNo,
    status: 'received',
  })

  // Check persistence before outbound verification so webhook retries never amplify upstream calls.
  if (await donationExistsInWall(env, 'afdian', webhook.outTradeNo)) {
    return afdianAckResponse()
  }

  const orderLookup = await queryAfdianOrders({
    userId: env.AFDIAN_USER_ID,
    token: env.AFDIAN_TOKEN,
    outTradeNo: webhook.outTradeNo,
    nowSec: deps.nowSec(),
    fetchFn: deps.fetchFn,
  })
  if (!orderLookup.ok) {
    logWebhookRejected({
      provider: 'afdian',
      reason: 'afdian_verify_failed',
      externalId: webhook.outTradeNo,
      upstreamReason: orderLookup.reason,
    })
    // Return 502 (not ec: 200) so Afdian retries after transient verification errors.
    return jsonResponse({ error: 'afdian_verify_failed' }, 502, NO_STORE_HEADERS)
  }

  const matchedOrder = orderLookup.orders.find(item => item.outTradeNo === webhook.outTradeNo)
  if (matchedOrder === undefined) {
    // Log before ACK so silent drops caused by upstream lag or parsing mismatches remain traceable.
    logAfdianOrderSkipped({ outTradeNo: webhook.outTradeNo, reason: 'order_not_found' })
    return afdianAckResponse()
  }
  if (matchedOrder.status !== AFDIAN_PAID_STATUS) {
    logAfdianOrderSkipped({
      outTradeNo: webhook.outTradeNo,
      reason: 'order_not_paid',
      orderStatus: matchedOrder.status,
    })
    return afdianAckResponse()
  }

  await commitVerifiedAfdianOrder(env, matchedOrder, env.AFDIAN_TOKEN, deps)
  return afdianAckResponse()
}

export async function handleKofiWebhook(
  request: Request,
  env: DonationsEnv,
  deps: RuntimeDeps,
): Promise<Response> {
  if (!env.KOFI_VERIFICATION_TOKEN) {
    return jsonResponse({ error: 'provider_not_configured' }, 503, NO_STORE_HEADERS)
  }
  if (!hasMediaType(request, 'application/x-www-form-urlencoded')) {
    logWebhookRejected({ provider: 'kofi', reason: 'invalid_payload' })
    return jsonResponse({ error: 'unsupported_media_type' }, 400, NO_STORE_HEADERS)
  }

  const bodyResult = await readBoundedText(request, MAX_WEBHOOK_BODY_BYTES)
  if (!bodyResult.ok) {
    logWebhookRejected({ provider: 'kofi', reason: 'body_too_large' })
    return jsonResponse({ error: 'body_too_large' }, 413, NO_STORE_HEADERS)
  }

  const parsed = parseKofiWebhookBody(bodyResult.text)
  if (!parsed.ok) {
    logWebhookRejected({ provider: 'kofi', reason: 'invalid_payload' })
    return jsonResponse({ error: 'invalid_payload' }, 400, NO_STORE_HEADERS)
  }

  if (!verifyKofiToken(parsed.payload.verificationToken, env.KOFI_VERIFICATION_TOKEN)) {
    logWebhookRejected({ provider: 'kofi', reason: 'invalid_signature' })
    return jsonResponse({ error: 'forbidden' }, 403, NO_STORE_HEADERS)
  }

  logWebhookReceived({
    provider: 'kofi',
    externalId: parsed.payload.kofiTransactionId,
    status: parsed.payload.type,
  })

  if (!isSupportedKofiType(parsed.payload.type)) {
    return jsonResponse({ ok: true, ignored: true }, 200, NO_STORE_HEADERS)
  }

  const money = parseFixedPointAmount(parsed.payload.amount, parsed.payload.currency)
  if (money === null) {
    logWebhookRejected({ provider: 'kofi', reason: 'invalid_payload' })
    return jsonResponse({ error: 'invalid_amount' }, 400, NO_STORE_HEADERS)
  }

  const displayName = parsed.payload.isPublic
    ? sanitizePlainText(parsed.payload.fromName, MAX_DISPLAY_NAME_CODEPOINTS)
    : null
  const message = parsed.payload.isPublic
    ? sanitizePlainText(parsed.payload.message, MAX_MESSAGE_CODEPOINTS)
    : null

  await commitDonationToWall(
    env,
    {
      provider: 'kofi',
      externalId: parsed.payload.kofiTransactionId,
      displayName,
      amountMinor: money.amountMinor,
      currency: money.currency,
      minorDigits: money.minorDigits,
      message,
      isPublic: parsed.payload.isPublic,
      donatedAt: parsed.payload.donatedAt,
    },
    deps.nowSec(),
  )
  return jsonResponse({ ok: true }, 200, NO_STORE_HEADERS)
}

async function verifyAndParseNowPaymentsRequest(
  request: Request,
  ipnSecret: string,
): Promise<
  | { readonly ok: true; readonly ipn: ReturnType<typeof parseNowPaymentsIpnPayload> & { readonly ok: true } }
  | { readonly ok: false; readonly response: Response }
> {
  if (!hasMediaType(request, 'application/json')) {
    logWebhookRejected({ provider: 'nowpayments', reason: 'invalid_payload' })
    return { ok: false, response: jsonResponse({ error: 'unsupported_media_type' }, 400, NO_STORE_HEADERS) }
  }

  const bodyResult = await readBoundedText(request, MAX_WEBHOOK_BODY_BYTES)
  if (!bodyResult.ok) {
    logWebhookRejected({ provider: 'nowpayments', reason: 'body_too_large' })
    return { ok: false, response: jsonResponse({ error: 'body_too_large' }, 413, NO_STORE_HEADERS) }
  }

  const signature = request.headers.get('x-nowpayments-sig')
  if (signature === null) {
    logWebhookRejected({ provider: 'nowpayments', reason: 'invalid_signature' })
    return { ok: false, response: jsonResponse({ error: 'forbidden' }, 403, NO_STORE_HEADERS) }
  }

  const rawObject = parseJsonObject(bodyResult.text)
  if (rawObject === null) {
    logWebhookRejected({ provider: 'nowpayments', reason: 'invalid_payload' })
    return { ok: false, response: jsonResponse({ error: 'invalid_payload' }, 400, NO_STORE_HEADERS) }
  }

  if (!verifyNowPaymentsSignature(rawObject, signature, ipnSecret)) {
    logWebhookRejected({ provider: 'nowpayments', reason: 'invalid_signature' })
    return { ok: false, response: jsonResponse({ error: 'forbidden' }, 403, NO_STORE_HEADERS) }
  }

  const parsed = parseNowPaymentsIpnPayload(rawObject)
  if (!parsed.ok) {
    logWebhookRejected({ provider: 'nowpayments', reason: 'invalid_payload' })
    return { ok: false, response: jsonResponse({ error: 'invalid_payload' }, 400, NO_STORE_HEADERS) }
  }

  return { ok: true, ipn: parsed }
}

export async function handleNowPaymentsWebhook(
  request: Request,
  env: DonationsEnv,
  deps: RuntimeDeps,
): Promise<Response> {
  if (!env.NOWPAYMENTS_IPN_SECRET) {
    return jsonResponse({ error: 'provider_not_configured' }, 503, NO_STORE_HEADERS)
  }

  const verified = await verifyAndParseNowPaymentsRequest(request, env.NOWPAYMENTS_IPN_SECRET)
  if (!verified.ok) return verified.response

  const { ipn } = verified.ipn
  logWebhookReceived({
    provider: 'nowpayments',
    externalId: ipn.paymentId,
    status: ipn.paymentStatus,
  })

  if (ipn.paymentStatus === 'partially_paid') {
    logNowPaymentsPartiallyPaid({
      paymentId: ipn.paymentId,
      orderId: ipn.orderId,
      priceCurrency: ipn.priceCurrency,
    })
    return jsonResponse({ ok: true, ignored: true }, 200, NO_STORE_HEADERS)
  }
  if (ipn.paymentStatus !== 'finished') {
    return jsonResponse({ ok: true, ignored: true }, 200, NO_STORE_HEADERS)
  }

  const money = parseFixedPointAmount(ipn.priceAmount, ipn.priceCurrency)
  if (money === null) {
    logWebhookRejected({ provider: 'nowpayments', reason: 'invalid_payload' })
    return jsonResponse({ error: 'invalid_amount' }, 400, NO_STORE_HEADERS)
  }

  const now = deps.nowSec()
  const intent = await resolveNowPaymentsIntent(env, ipn.paymentId, ipn.orderId, now)
  await commitDonationToWall(
    env,
    {
      provider: 'nowpayments',
      externalId: ipn.paymentId,
      displayName: intent?.displayName ?? null,
      amountMinor: money.amountMinor,
      currency: money.currency,
      minorDigits: money.minorDigits,
      message: intent?.message ?? null,
      isPublic: true,
      donatedAt: ipn.donatedAt,
    },
    now,
  )
  return jsonResponse({ ok: true }, 200, NO_STORE_HEADERS)
}
