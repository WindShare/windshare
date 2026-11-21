import { randomBytes } from 'node:crypto'
import type { DonationsEnv } from '../env'
import {
  ADMIN_COOKIE_NAME,
  DONATION_WALL_INSTANCE_NAME,
  MAX_ADMIN_BODY_BYTES,
  adminJsonResponse,
  getClientIp,
  hasMediaType,
  isSameOriginRequest,
  parseCookie,
  parseJsonObject,
  readBoundedText,
} from '../http'
import {
  logAdminAfdianSynced,
  logAdminLogin,
  logDonationModerated,
} from '../log'
import {
  ADMIN_SESSION_TTL_SECONDS,
  MAX_CRYPTO_USD_MINOR,
  MAX_HIDE_REASON_CODEPOINTS,
  type AdminConfigPatch,
  type AdminSessionRecord,
  type AdminVisibilityCommand,
  type DonationRecord,
  type WallVisibility,
} from '../model'
import { queryAfdianOrders } from '../providers'
import { sanitizePlainText } from '../sanitize'
import { constantTimeEqual, hmacSha256Hex } from '../verify'
import {
  commitVerifiedAfdianOrder,
  donationExistsInWall,
  type RuntimeDeps,
} from '../webhooks/handlers'

const VISIBILITY_PATH_PREFIX = '/v1/admin/donations/'
const VISIBILITY_PATH_SUFFIX = '/visibility'
const AFDIAN_PAID_STATUS = 2
const ADMIN_SESSION_HASH_DOMAIN = 'windshare-donations-admin-session:v1:'

interface AuthenticatedSession extends AdminSessionRecord {
  readonly sessionHash: string
}

function getWallStub(env: DonationsEnv): DurableObjectStub {
  return env.DONATION_WALL.get(env.DONATION_WALL.idFromName(DONATION_WALL_INSTANCE_NAME))
}

function buildSessionCookie(token: string, maxAgeSeconds: number): string {
  return `${ADMIN_COOKIE_NAME}=${token}; HttpOnly; Secure; SameSite=Strict; Path=/; Max-Age=${maxAgeSeconds}`
}

function deriveAdminSessionHash(adminPassword: string, token: string): string {
  // Keying stored session hashes with the active admin password invalidates all existing
  // sessions immediately upon secret rotation without persisting a password digest.
  return hmacSha256Hex(adminPassword, `${ADMIN_SESSION_HASH_DOMAIN}${token}`)
}

async function authenticateAdmin(
  request: Request,
  env: DonationsEnv,
  adminPassword: string,
  nowSec: number,
): Promise<AuthenticatedSession | null> {
  const token = parseCookie(request.headers.get('Cookie'), ADMIN_COOKIE_NAME)
  if (token === null) return null
  const sessionHash = deriveAdminSessionHash(adminPassword, token)
  const stub = getWallStub(env)
  const response = await stub.fetch('https://donation-wall.internal/internal/admin/sessions/verify', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sessionHash, nowSec }),
  })
  if (!response.ok) return null
  const body = await response.json() as { valid: boolean; csrfToken: string; expiresAt: number }
  if (!body.valid) return null
  return { sessionHash, csrfToken: body.csrfToken, expiresAt: body.expiresAt }
}

function verifyAdminMutationGuards(
  request: Request,
  expectedOrigin: string,
  expectedCsrf: string,
): Response | null {
  if (!isSameOriginRequest(request, expectedOrigin)) {
    return adminJsonResponse({ error: 'forbidden_origin' }, 403)
  }
  const csrfHeader = request.headers.get('X-CSRF-Token')
  if (csrfHeader === null || !constantTimeEqual(csrfHeader, expectedCsrf)) {
    return adminJsonResponse({ error: 'invalid_csrf_token' }, 403)
  }
  return null
}

async function readAdminJsonBody(
  request: Request,
): Promise<{ readonly ok: true; readonly data: Record<string, unknown> } | { readonly ok: false; readonly response: Response }> {
  if (!hasMediaType(request, 'application/json')) {
    return { ok: false, response: adminJsonResponse({ error: 'unsupported_media_type' }, 400) }
  }
  const read = await readBoundedText(request, MAX_ADMIN_BODY_BYTES)
  if (!read.ok) {
    return { ok: false, response: adminJsonResponse({ error: 'body_too_large' }, 413) }
  }
  const data = parseJsonObject(read.text)
  if (data === null) {
    return { ok: false, response: adminJsonResponse({ error: 'invalid_json' }, 400) }
  }
  return { ok: true, data }
}

async function handleAdminLogin(
  request: Request,
  env: DonationsEnv,
  url: URL,
  adminPassword: string,
  deps: RuntimeDeps,
): Promise<Response> {
  if (!isSameOriginRequest(request, url.origin)) {
    return adminJsonResponse({ error: 'forbidden_origin' }, 403)
  }

  const parsedBody = await readAdminJsonBody(request)
  if (!parsedBody.ok) return parsedBody.response

  const password = typeof parsedBody.data.password === 'string' ? parsedBody.data.password : ''
  const nowSec = deps.nowSec()
  const stub = getWallStub(env)

  const limitRes = await stub.fetch('https://donation-wall.internal/internal/admin/rate-limit', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientIp: getClientIp(request), nowSec }),
  })
  if (limitRes.status === 429) {
    const limitBody = await limitRes.json() as { retryAfterSeconds: number }
    logAdminLogin({ ok: false, reason: 'rate_limited' })
    return adminJsonResponse(
      { error: 'rate_limited', retryAfterSeconds: limitBody.retryAfterSeconds },
      429,
      { 'Retry-After': String(limitBody.retryAfterSeconds) },
    )
  }

  if (password.length === 0 || !constantTimeEqual(password, adminPassword)) {
    logAdminLogin({ ok: false, reason: 'invalid_password' })
    return adminJsonResponse({ error: 'invalid_credentials' }, 401)
  }

  const sessionToken = randomBytes(32).toString('base64url')
  const csrfToken = randomBytes(32).toString('base64url')
  const sessionHash = deriveAdminSessionHash(adminPassword, sessionToken)
  const createRes = await stub.fetch('https://donation-wall.internal/internal/admin/sessions', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sessionHash, csrfToken, nowSec }),
  })
  const created = await createRes.json() as AdminSessionRecord

  logAdminLogin({ ok: true, reason: 'authenticated' })
  return adminJsonResponse(
    { authenticated: true, csrfToken: created.csrfToken, expiresAt: created.expiresAt },
    200,
    { 'Set-Cookie': buildSessionCookie(sessionToken, ADMIN_SESSION_TTL_SECONDS) },
  )
}

function parseVisibilityCommand(data: Record<string, unknown>): AdminVisibilityCommand | null {
  if (data.action === 'approve') return { action: 'approve' }
  if (data.action === 'restore') return { action: 'restore' }
  if (data.action === 'hide') {
    const reason = sanitizePlainText(
      typeof data.reason === 'string' ? data.reason : null,
      MAX_HIDE_REASON_CODEPOINTS,
    )
    if (reason === null) return null
    return { action: 'hide', reason }
  }
  return null
}

function parseNonNegativeMinorLimit(value: unknown): number | null {
  if (typeof value !== 'number' || !Number.isSafeInteger(value)) return null
  if (value < 0 || value > MAX_CRYPTO_USD_MINOR) return null
  return value
}

function parseConfigPatch(data: Record<string, unknown>): AdminConfigPatch | null {
  const patch: {
    minPublicCnyMinor?: number
    minPublicUsdMinor?: number
    moderationEnabled?: boolean
    cryptoCheckoutPaused?: boolean
  } = {}
  let changed = false

  if ('minPublicCnyMinor' in data) {
    const parsed = parseNonNegativeMinorLimit(data.minPublicCnyMinor)
    if (parsed === null) return null
    patch.minPublicCnyMinor = parsed
    changed = true
  }
  if ('minPublicUsdMinor' in data) {
    const parsed = parseNonNegativeMinorLimit(data.minPublicUsdMinor)
    if (parsed === null) return null
    patch.minPublicUsdMinor = parsed
    changed = true
  }
  if ('moderationEnabled' in data) {
    if (typeof data.moderationEnabled !== 'boolean') return null
    patch.moderationEnabled = data.moderationEnabled
    changed = true
  }
  if ('cryptoCheckoutPaused' in data) {
    if (typeof data.cryptoCheckoutPaused !== 'boolean') return null
    patch.cryptoCheckoutPaused = data.cryptoCheckoutPaused
    changed = true
  }

  return changed ? patch : null
}

async function handleSyncAfdian(env: DonationsEnv, deps: RuntimeDeps): Promise<Response> {
  if (!env.AFDIAN_TOKEN) {
    return adminJsonResponse({ error: 'afdian_not_configured' }, 503)
  }
  const orderQuery = await queryAfdianOrders({
    userId: env.AFDIAN_USER_ID,
    token: env.AFDIAN_TOKEN,
    page: 1,
    perPage: 50,
    nowSec: deps.nowSec(),
    fetchFn: deps.fetchFn,
  })
  if (!orderQuery.ok) {
    return adminJsonResponse({ error: 'afdian_query_failed', reason: orderQuery.reason }, 502)
  }

  let paid = 0
  let committed = 0
  let deduplicated = 0

  for (const order of orderQuery.orders) {
    if (order.status !== AFDIAN_PAID_STATUS) continue
    paid += 1
    if (await donationExistsInWall(env, 'afdian', order.outTradeNo)) {
      deduplicated += 1
      continue
    }
    const result = await commitVerifiedAfdianOrder(env, order, env.AFDIAN_TOKEN, deps)
    if (result === null) continue
    if (result.deduplicated) {
      deduplicated += 1
    } else {
      committed += 1
    }
  }

  logAdminAfdianSynced({
    fetched: orderQuery.orders.length,
    paid,
    committed,
    deduplicated,
  })
  return adminJsonResponse({
    fetched: orderQuery.orders.length,
    paid,
    committed,
    deduplicated,
  })
}

async function handleVisibilityMutation(
  request: Request,
  env: DonationsEnv,
  donationId: string,
  deps: RuntimeDeps,
): Promise<Response> {
  const parsedBody = await readAdminJsonBody(request)
  if (!parsedBody.ok) return parsedBody.response

  const command = parseVisibilityCommand(parsedBody.data)
  if (command === null) {
    return adminJsonResponse({ error: 'invalid_visibility_command' }, 400)
  }

  const stub = getWallStub(env)
  const upstream = await stub.fetch(
    `https://donation-wall.internal/internal/admin/donations/${encodeURIComponent(donationId)}/visibility`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ command, nowSec: deps.nowSec() }),
    },
  )
  const payload = await upstream.json() as
    | { error: string }
    | { record: DonationRecord; fromVisibility: WallVisibility; toVisibility: WallVisibility }

  if (!upstream.ok || 'error' in payload) {
    return adminJsonResponse(payload, upstream.status)
  }

  logDonationModerated({
    donationId: payload.record.id,
    action: command.action,
    fromVisibility: payload.fromVisibility.kind,
    toVisibility: payload.toVisibility.kind,
  })
  return adminJsonResponse({ record: payload.record }, 200)
}

function extractVisibilityDonationId(pathname: string): { readonly matched: false } | { readonly matched: true; readonly donationId: string | null } {
  if (!pathname.startsWith(VISIBILITY_PATH_PREFIX) || !pathname.endsWith(VISIBILITY_PATH_SUFFIX)) {
    return { matched: false }
  }
  const rawId = pathname.slice(
    VISIBILITY_PATH_PREFIX.length,
    pathname.length - VISIBILITY_PATH_SUFFIX.length,
  )
  if (rawId.length === 0 || rawId.includes('/')) {
    return { matched: true, donationId: null }
  }
  try {
    const decoded = decodeURIComponent(rawId)
    return { matched: true, donationId: decoded.length > 0 ? decoded : null }
  } catch {
    return { matched: true, donationId: null }
  }
}

async function handleAdminConfigPatch(request: Request, env: DonationsEnv): Promise<Response> {
  const parsedBody = await readAdminJsonBody(request)
  if (!parsedBody.ok) return parsedBody.response
  const patch = parseConfigPatch(parsedBody.data)
  if (patch === null) {
    return adminJsonResponse({ error: 'invalid_config_patch' }, 400)
  }
  const stub = getWallStub(env)
  const upstream = await stub.fetch('https://donation-wall.internal/internal/admin/config', {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  })
  return adminJsonResponse(await upstream.json(), upstream.status)
}

async function handleAuthenticatedAdminMutation(
  request: Request,
  env: DonationsEnv,
  url: URL,
  session: AuthenticatedSession,
  deps: RuntimeDeps,
): Promise<Response | null> {
  const guardError = verifyAdminMutationGuards(request, url.origin, session.csrfToken)
  if (guardError !== null) return guardError

  const { method } = request
  const { pathname } = url

  if (method === 'POST' && pathname === '/v1/admin/logout') {
    const stub = getWallStub(env)
    await stub.fetch('https://donation-wall.internal/internal/admin/sessions/revoke', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessionHash: session.sessionHash }),
    })
    return adminJsonResponse({ ok: true }, 200, {
      'Set-Cookie': buildSessionCookie('', 0),
    })
  }

  if (method === 'POST' && pathname === '/v1/admin/donations/sync-afdian') {
    return handleSyncAfdian(env, deps)
  }

  if (method === 'POST') {
    const visibilityMatch = extractVisibilityDonationId(pathname)
    if (visibilityMatch.matched) {
      if (visibilityMatch.donationId === null) {
        return adminJsonResponse({ error: 'invalid_donation_id' }, 400)
      }
      return handleVisibilityMutation(request, env, visibilityMatch.donationId, deps)
    }
  }

  if (method === 'PATCH' && pathname === '/v1/admin/donations/config') {
    return handleAdminConfigPatch(request, env)
  }

  return null
}

export async function handleAdminApi(
  request: Request,
  env: DonationsEnv,
  url: URL,
  deps: RuntimeDeps,
): Promise<Response> {
  // Fail closed across every admin route when the shared secret is absent or deleted so
  // existing cookies cannot survive disabling admin access.
  const adminPassword = env.DONATE_ADMIN_PASSWORD
  if (!adminPassword) {
    return adminJsonResponse({ error: 'admin_not_configured' }, 503)
  }

  const { method } = request
  const { pathname } = url

  if (method === 'POST' && pathname === '/v1/admin/login') {
    return handleAdminLogin(request, env, url, adminPassword, deps)
  }

  const session = await authenticateAdmin(request, env, adminPassword, deps.nowSec())
  if (session === null) {
    return adminJsonResponse({ error: 'unauthorized' }, 401)
  }

  if (method === 'GET' && pathname === '/v1/admin/session') {
    const stub = getWallStub(env)
    const configRes = await stub.fetch('https://donation-wall.internal/internal/admin/config')
    const { config } = await configRes.json() as { config: unknown }
    return adminJsonResponse({
      authenticated: true,
      csrfToken: session.csrfToken,
      expiresAt: session.expiresAt,
      config,
    })
  }

  if (method === 'GET' && pathname === '/v1/admin/donations') {
    const stub = getWallStub(env)
    const query = url.searchParams.toString()
    const suffix = query.length > 0 ? `?${query}` : ''
    const upstream = await stub.fetch(
      `https://donation-wall.internal/internal/admin/donations${suffix}`,
    )
    return adminJsonResponse(await upstream.json(), upstream.status)
  }

  const mutationResponse = await handleAuthenticatedAdminMutation(request, env, url, session, deps)
  if (mutationResponse !== null) return mutationResponse

  return adminJsonResponse({ error: 'not_found' }, 404)
}
