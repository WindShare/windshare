import { ADMIN_APP_CSS, ADMIN_APP_JS, ADMIN_FAVICON_SVG, ADMIN_HTML } from './admin/assets'
import { handleAdminApi } from './admin/routes'
import { parseAllowedOrigins, resolveCryptoCheckoutApiKey, type DonationsEnv } from './env'
import {
  MAX_CRYPTO_INTENT_BODY_BYTES,
  adminSecurityHeaders,
  getClientIp,
  getDonationWallStub,
  hasMediaType,
  jsonResponse,
  parseJsonObject,
  readBoundedText,
} from './http'
import { logCryptoIntentCreated, logCryptoInvoiceFailed } from './log'
import {
  MAX_DISPLAY_NAME_CODEPOINTS,
  MAX_MESSAGE_CODEPOINTS,
} from './model'
import { createNowPaymentsInvoice, type FetchLike } from './providers'
import {
  formatUsdMinorForInvoice,
  parseCryptoUsdAmount,
  sanitizeAndTruncatePlainText,
} from './sanitize'
import {
  handleAfdianWebhook,
  handleKofiWebhook,
  handleNowPaymentsWebhook,
  type RuntimeDeps,
} from './webhooks/handlers'

const PUBLIC_WALL_CACHE_CONTROL = 'public, max-age=15, stale-while-revalidate=60'
const AFDIAN_WEBHOOK_PREFIX = '/v1/donations/webhooks/afdian/'

export interface WorkerDeps {
  readonly fetchFn?: FetchLike
  readonly nowSec?: () => number
}

function resolveRuntimeDeps(deps?: WorkerDeps): RuntimeDeps {
  return {
    fetchFn: deps?.fetchFn ?? ((input, init) => fetch(input, init)),
    nowSec: deps?.nowSec ?? (() => Math.floor(Date.now() / 1000)),
  }
}

function handleAdminAssetRoute(method: string, pathname: string): Response | null {
  if (method !== 'GET') return null
  if (pathname === '/admin' || pathname === '/admin/') {
    return new Response(ADMIN_HTML, {
      status: 200,
      headers: adminSecurityHeaders('text/html; charset=utf-8'),
    })
  }
  if (pathname === '/admin/app.js') {
    return new Response(ADMIN_APP_JS, {
      status: 200,
      headers: adminSecurityHeaders('application/javascript; charset=utf-8'),
    })
  }
  if (pathname === '/admin/app.css') {
    return new Response(ADMIN_APP_CSS, {
      status: 200,
      headers: adminSecurityHeaders('text/css; charset=utf-8'),
    })
  }
  if (pathname === '/admin/favicon.svg') {
    return new Response(ADMIN_FAVICON_SVG, {
      status: 200,
      headers: adminSecurityHeaders('image/svg+xml; charset=utf-8'),
    })
  }
  return null
}

async function handlePublicWall(request: Request, env: DonationsEnv): Promise<Response> {
  const result = await getDonationWallStub(env).getPublicWall(request.headers.get('If-None-Match'))
  const headers = new Headers({
    'Access-Control-Allow-Origin': '*',
    'Cache-Control': PUBLIC_WALL_CACHE_CONTROL,
    ETag: result.etag,
    'X-Content-Type-Options': 'nosniff',
  })
  if (result.notModified) {
    return new Response(null, { status: 304, headers })
  }
  headers.set('Content-Type', 'application/json; charset=utf-8')
  return new Response(result.json, { status: 200, headers })
}

function cryptoCorsHeaders(origin: string): Record<string, string> {
  return {
    'Access-Control-Allow-Origin': origin,
    'Cache-Control': 'no-store',
    Vary: 'Origin',
  }
}

function handleCryptoPreflight(request: Request, env: DonationsEnv): Response {
  const origin = request.headers.get('Origin')
  const allowed = parseAllowedOrigins(env.ALLOWED_ORIGINS)
  if (origin === null || !allowed.has(origin)) {
    return new Response(null, { status: 403 })
  }
  return new Response(null, {
    status: 204,
    headers: {
      'Access-Control-Allow-Origin': origin,
      'Access-Control-Allow-Methods': 'POST, OPTIONS',
      'Access-Control-Allow-Headers': 'Content-Type',
      'Access-Control-Max-Age': '600',
      Vary: 'Origin',
    },
  })
}

function normalizeApiOrigin(raw: string): string {
  return new URL(raw).origin
}

async function handleCreateCryptoIntent(
  request: Request,
  env: DonationsEnv,
  deps: RuntimeDeps,
): Promise<Response> {
  const origin = request.headers.get('Origin')
  const allowed = parseAllowedOrigins(env.ALLOWED_ORIGINS)
  if (origin === null || !allowed.has(origin)) {
    return jsonResponse({ error: 'forbidden_origin' }, 403, { 'Cache-Control': 'no-store' })
  }
  const cors = cryptoCorsHeaders(origin)

  const nowPaymentsApiKey = resolveCryptoCheckoutApiKey(env)
  if (nowPaymentsApiKey === null) {
    return jsonResponse({ error: 'crypto_not_configured' }, 503, cors)
  }
  if (!hasMediaType(request, 'application/json')) {
    return jsonResponse({ error: 'unsupported_media_type' }, 400, cors)
  }

  const bodyRead = await readBoundedText(request, MAX_CRYPTO_INTENT_BODY_BYTES)
  if (!bodyRead.ok) {
    return jsonResponse({ error: 'body_too_large' }, 413, cors)
  }
  const parsed = parseJsonObject(bodyRead.text)
  if (parsed === null) {
    return jsonResponse({ error: 'invalid_json' }, 400, cors)
  }

  const rawAmount = parsed.amountUsd
  if (typeof rawAmount !== 'string' && typeof rawAmount !== 'number') {
    return jsonResponse({ error: 'invalid_amount' }, 400, cors)
  }
  const requestedUsdMinor = parseCryptoUsdAmount(rawAmount)
  if (requestedUsdMinor === null) {
    return jsonResponse({ error: 'invalid_amount' }, 400, cors)
  }

  const displayName = sanitizeAndTruncatePlainText(
    typeof parsed.displayName === 'string' ? parsed.displayName : null,
    MAX_DISPLAY_NAME_CODEPOINTS,
  )
  const message = sanitizeAndTruncatePlainText(
    typeof parsed.message === 'string' ? parsed.message : null,
    MAX_MESSAGE_CODEPOINTS,
  )

  const created = await getDonationWallStub(env).createCryptoIntent({
    clientIp: getClientIp(request),
    displayName,
    message,
    requestedUsdMinor,
    returnOrigin: origin,
    nowSec: deps.nowSec(),
  })
  if (!created.ok) {
    if (created.code === 'crypto_checkout_paused') {
      return jsonResponse({ error: created.code }, 503, cors)
    }
    return jsonResponse(
      { error: created.code },
      429,
      {
        ...cors,
        'Retry-After': String(created.retryAfterSeconds),
        // Retry-After is not CORS-safelisted; expose it explicitly so cross-origin clients can read it.
        'Access-Control-Expose-Headers': 'Retry-After',
      },
    )
  }

  const { intent } = created
  logCryptoIntentCreated({
    intentId: intent.intentId,
    requestedUsdMinor: intent.requestedUsdMinor,
    hasMessage: intent.message !== null,
  })

  const apiOrigin = normalizeApiOrigin(env.DONATE_API_ORIGIN)
  const invoice = await createNowPaymentsInvoice({
    apiKey: nowPaymentsApiKey,
    priceAmountUsd: Number(formatUsdMinorForInvoice(intent.requestedUsdMinor)),
    orderId: intent.intentId,
    ipnCallbackUrl: `${apiOrigin}/v1/donations/webhooks/nowpayments`,
    successUrl: `${origin}/donate?status=submitted&intent=${encodeURIComponent(intent.intentId)}`,
    cancelUrl: `${origin}/donate`,
    fetchFn: deps.fetchFn,
  })
  if (!invoice.ok) {
    logCryptoInvoiceFailed({
      intentId: intent.intentId,
      reason: invoice.reason,
    })
    return jsonResponse({ error: 'upstream_checkout_failed' }, 502, cors)
  }

  return jsonResponse({ intentId: intent.intentId, invoiceUrl: invoice.invoiceUrl }, 201, cors)
}

async function handleWebhookRoute(
  request: Request,
  env: DonationsEnv,
  pathname: string,
  deps: RuntimeDeps,
): Promise<Response | null> {
  if (request.method !== 'POST') return null
  if (pathname === '/v1/donations/webhooks/kofi') {
    return handleKofiWebhook(request, env, deps)
  }
  if (pathname === '/v1/donations/webhooks/nowpayments') {
    return handleNowPaymentsWebhook(request, env, deps)
  }
  if (pathname.startsWith(AFDIAN_WEBHOOK_PREFIX)) {
    const secretSegment = pathname.slice(AFDIAN_WEBHOOK_PREFIX.length)
    if (secretSegment.length === 0 || secretSegment.includes('/')) {
      return jsonResponse({ error: 'not_found' }, 404, { 'Cache-Control': 'no-store' })
    }
    return handleAfdianWebhook(request, env, secretSegment, deps)
  }
  return null
}

export async function handleWorkerFetch(
  request: Request,
  env: DonationsEnv,
  deps?: WorkerDeps,
): Promise<Response> {
  const url = new URL(request.url)
  const { method } = request
  const { pathname } = url
  const runtime = resolveRuntimeDeps(deps)

  if (method === 'GET' && pathname === '/healthz') {
    return jsonResponse({ ok: true, service: 'windshare-donations' }, 200, {
      'Cache-Control': 'no-store',
    })
  }
  if (method === 'GET' && pathname === '/v1/donations/wall') {
    return handlePublicWall(request, env)
  }
  if (method === 'OPTIONS' && pathname === '/v1/donations/crypto-intents') {
    return handleCryptoPreflight(request, env)
  }
  if (method === 'POST' && pathname === '/v1/donations/crypto-intents') {
    return handleCreateCryptoIntent(request, env, runtime)
  }

  const webhookResponse = await handleWebhookRoute(request, env, pathname, runtime)
  if (webhookResponse !== null) return webhookResponse

  const adminAsset = handleAdminAssetRoute(method, pathname)
  if (adminAsset !== null) return adminAsset

  if (pathname.startsWith('/v1/admin/')) {
    return handleAdminApi(request, env, url, runtime)
  }

  return jsonResponse({ error: 'not_found' }, 404, { 'Cache-Control': 'no-store' })
}
