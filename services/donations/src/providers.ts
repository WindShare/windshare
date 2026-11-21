import {
  buildAfdianSignedBody,
  parseAfdianOrderQueryResponse,
  parseAfdianSponsorPage,
  type AfdianOrderRecord,
} from './verify'

const OUTBOUND_TIMEOUT_MS = 10_000
const AFDIAN_QUERY_ORDER_URL = 'https://afdian.com/api/open/query-order'
const AFDIAN_QUERY_SPONSOR_URL = 'https://afdian.com/api/open/query-sponsor'
const NOWPAYMENTS_INVOICE_URL = 'https://api.nowpayments.io/v1/invoice'
const NOWPAYMENTS_HOST = 'nowpayments.io'
// Afdian query-sponsor supports up to 100 entries per page, maximizing single-request cache hits for recent orders.
const AFDIAN_SPONSOR_PAGE_SIZE = 100

export type FetchLike = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>

export type AfdianOrderQueryResult =
  | { readonly ok: true; readonly orders: readonly AfdianOrderRecord[] }
  | { readonly ok: false; readonly reason: string }

export type AfdianSponsorPageResult =
  | { readonly ok: true; readonly sponsors: ReadonlyMap<string, string | null> }
  | { readonly ok: false; readonly reason: string }

export type AfdianSponsorLookupResult =
  | { readonly ok: true; readonly displayName: string | null }
  | { readonly ok: false; readonly reason: string }

export type NowPaymentsInvoiceResult =
  | { readonly ok: true; readonly invoiceUrl: string }
  | { readonly ok: false; readonly reason: string }

async function postJsonOutbound(
  url: string,
  body: unknown,
  headers: Record<string, string>,
  fetchFn: FetchLike,
): Promise<{ readonly ok: true; readonly json: unknown } | { readonly ok: false; readonly reason: string }> {
  let response: Response
  try {
    response = await fetchFn(url, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...headers,
      },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(OUTBOUND_TIMEOUT_MS),
      // Never follow redirects automatically so upstream API keys cannot leak to redirect targets.
      redirect: 'manual',
    })
  } catch {
    return { ok: false, reason: 'network_or_timeout' }
  }

  if (!response.ok) {
    // Explicitly cancel unconsumed error/redirect streams so workerd releases subrequest resources immediately.
    await response.body?.cancel().catch(() => undefined)
    const reason = response.status >= 300 && response.status < 400
      ? `unexpected_redirect_${response.status}`
      : `http_${response.status}`
    return { ok: false, reason }
  }

  try {
    const json: unknown = await response.json()
    return { ok: true, json }
  } catch {
    return { ok: false, reason: 'invalid_json' }
  }
}

export async function queryAfdianOrders(input: {
  readonly userId: string
  readonly token: string
  readonly outTradeNo?: string
  readonly page?: number
  readonly perPage?: number
  readonly nowSec: number
  readonly fetchFn: FetchLike
}): Promise<AfdianOrderQueryResult> {
  const params: Record<string, unknown> = input.outTradeNo !== undefined
    ? { out_trade_no: input.outTradeNo }
    : { page: input.page ?? 1, per_page: input.perPage ?? 50 }
  const signed = buildAfdianSignedBody(input.userId, input.token, params, input.nowSec)
  const result = await postJsonOutbound(AFDIAN_QUERY_ORDER_URL, signed, {}, input.fetchFn)
  if (!result.ok) return result
  return parseAfdianOrderQueryResponse(result.json)
}

async function fetchAfdianSponsorMap(input: {
  readonly userId: string
  readonly token: string
  readonly params: Record<string, unknown>
  readonly nowSec: number
  readonly fetchFn: FetchLike
}): Promise<AfdianSponsorPageResult> {
  const signed = buildAfdianSignedBody(input.userId, input.token, input.params, input.nowSec)
  const result = await postJsonOutbound(AFDIAN_QUERY_SPONSOR_URL, signed, {}, input.fetchFn)
  if (!result.ok) return result
  return parseAfdianSponsorPage(result.json)
}

export async function queryAfdianSponsorPage(input: {
  readonly userId: string
  readonly token: string
  readonly page?: number
  readonly perPage?: number
  readonly nowSec: number
  readonly fetchFn: FetchLike
}): Promise<AfdianSponsorPageResult> {
  return fetchAfdianSponsorMap({
    userId: input.userId,
    token: input.token,
    params: {
      page: input.page ?? 1,
      per_page: input.perPage ?? AFDIAN_SPONSOR_PAGE_SIZE,
    },
    nowSec: input.nowSec,
    fetchFn: input.fetchFn,
  })
}

export async function lookupAfdianSponsorNameDirect(input: {
  readonly userId: string
  readonly token: string
  readonly sponsorUserId: string
  readonly nowSec: number
  readonly fetchFn: FetchLike
}): Promise<AfdianSponsorLookupResult> {
  const filtered = await fetchAfdianSponsorMap({
    userId: input.userId,
    token: input.token,
    params: { user_id: input.sponsorUserId, page: 1 },
    nowSec: input.nowSec,
    fetchFn: input.fetchFn,
  })
  if (!filtered.ok) return filtered
  if (!filtered.sponsors.has(input.sponsorUserId)) {
    return { ok: false, reason: 'sponsor_not_found' }
  }
  return { ok: true, displayName: filtered.sponsors.get(input.sponsorUserId) ?? null }
}

export async function lookupAfdianSponsorName(input: {
  readonly userId: string
  readonly token: string
  readonly sponsorUserId: string
  readonly nowSec: number
  readonly fetchFn: FetchLike
}): Promise<AfdianSponsorLookupResult> {
  const filtered = await fetchAfdianSponsorMap({
    userId: input.userId,
    token: input.token,
    params: { user_id: input.sponsorUserId, page: 1 },
    nowSec: input.nowSec,
    fetchFn: input.fetchFn,
  })
  if (!filtered.ok) return filtered
  if (filtered.sponsors.has(input.sponsorUserId)) {
    return { ok: true, displayName: filtered.sponsors.get(input.sponsorUserId) ?? null }
  }

  // Fallback to page 1 in case the upstream account/endpoint ignores user_id filtering.
  const fallback = await queryAfdianSponsorPage({
    userId: input.userId,
    token: input.token,
    page: 1,
    perPage: AFDIAN_SPONSOR_PAGE_SIZE,
    nowSec: input.nowSec,
    fetchFn: input.fetchFn,
  })
  if (!fallback.ok) return fallback
  if (!fallback.sponsors.has(input.sponsorUserId)) {
    return { ok: false, reason: 'sponsor_not_found' }
  }
  return { ok: true, displayName: fallback.sponsors.get(input.sponsorUserId) ?? null }
}

function isNowPaymentsHostname(hostname: string): boolean {
  return hostname === NOWPAYMENTS_HOST || hostname.endsWith(`.${NOWPAYMENTS_HOST}`)
}

export function isValidNowPaymentsInvoiceUrl(raw: unknown): raw is string {
  if (typeof raw !== 'string') return false
  try {
    const parsed = new URL(raw)
    // Constrain upstream invoice links to NOWPayments HTTPS origins without embedded
    // credentials or non-default ports so upstream anomalies cannot redirect donors elsewhere.
    return (
      parsed.protocol === 'https:'
      && parsed.username === ''
      && parsed.password === ''
      && parsed.port === ''
      && isNowPaymentsHostname(parsed.hostname)
    )
  } catch {
    return false
  }
}

export async function createNowPaymentsInvoice(input: {
  readonly apiKey: string
  readonly priceAmountUsd: number
  readonly orderId: string
  readonly ipnCallbackUrl: string
  readonly successUrl: string
  readonly cancelUrl: string
  readonly fetchFn: FetchLike
}): Promise<NowPaymentsInvoiceResult> {
  const result = await postJsonOutbound(
    NOWPAYMENTS_INVOICE_URL,
    {
      price_amount: input.priceAmountUsd,
      price_currency: 'usd',
      order_id: input.orderId,
      ipn_callback_url: input.ipnCallbackUrl,
      success_url: input.successUrl,
      cancel_url: input.cancelUrl,
    },
    { 'x-api-key': input.apiKey },
    input.fetchFn,
  )
  if (!result.ok) return result
  if (typeof result.json !== 'object' || result.json === null || Array.isArray(result.json)) {
    return { ok: false, reason: 'invalid_invoice_response' }
  }
  const invoiceUrl = (result.json as Record<string, unknown>).invoice_url
  if (!isValidNowPaymentsInvoiceUrl(invoiceUrl)) {
    return { ok: false, reason: 'invalid_invoice_url' }
  }
  return { ok: true, invoiceUrl }
}
