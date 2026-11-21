import { randomBytes } from 'node:crypto'
import type { DonationsEnv } from '../env'
import {
  ADMIN_COOKIE_NAME,
  MAX_ADMIN_BODY_BYTES,
  adminJsonResponse,
  getClientIp,
  getDonationWallStub,
  hasMediaType,
  isSameOriginRequest,
  parseCookie,
  parseJsonObject,
  readBoundedText,
} from '../http'
import {
  logAdminAfdianSynced,
  logAdminConfigUpdated,
  logAdminLogin,
  logAdminStorageError,
  logAfdianSponsorLookupFallback,
  logDonationModerated,
  logWebhookRejected,
} from '../log'
import {
  ADMIN_SESSION_TTL_SECONDS,
  MAX_CRYPTO_USD_MINOR,
  MAX_HIDE_REASON_CODEPOINTS,
  type AdminConfigPatch,
  type AdminSessionRecord,
  type AdminVisibilityCommand,
  type CommitDonationInput,
} from '../model'
import {
  lookupAfdianSponsorNameDirect,
  queryAfdianOrders,
  queryAfdianSponsorPage,
} from '../providers'
import { parseFixedPointAmount, sanitizePlainText, type ParsedMoney } from '../sanitize'
import { constantTimeEqual, hmacSha256Hex, type AfdianOrderRecord } from '../verify'
import {
  commitDonationsBatchToWall,
  findExistingDonationsInWall,
  toAfdianCommitInput,
  type RuntimeDeps,
} from '../webhooks/handlers'

const VISIBILITY_PATH_PREFIX = '/v1/admin/donations/'
const VISIBILITY_PATH_SUFFIX = '/visibility'
const AFDIAN_PAID_STATUS = 2
const AFDIAN_SYNC_PER_PAGE = 50
// Cap per-sync direct sponsor lookups so 1 query-order + 1 sponsor page + direct lookups
// stay safely below the 50 external subrequests/request Workers Free limit.
const MAX_AFDIAN_SYNC_SPONSOR_LOOKUPS = 40
const AFDIAN_RATE_LIMIT_REASON = 'http_429'
const ADMIN_SESSION_HASH_DOMAIN = 'windshare-donations-admin-session:v1:'
const DECIMAL_INT_PATTERN = /^-?\d+$/

interface AuthenticatedSession extends AdminSessionRecord {
  readonly sessionHash: string
}

function parseOptionalIntParam(raw: string | null): number | null {
  if (raw === null) return null
  const trimmed = raw.trim()
  // Require base-10 integer syntax so hex (0x10), scientific notation (1e0), and decimal
  // floats (1.0 / 1.5) all fall back consistently rather than relying on Number() coercions.
  if (!DECIMAL_INT_PATTERN.test(trimmed)) return null
  const parsed = Number(trimmed)
  return Number.isSafeInteger(parsed) ? parsed : null
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
  const session = await getDonationWallStub(env).verifyAdminSession(sessionHash, nowSec)
  if (session === null) return null
  return { sessionHash, csrfToken: session.csrfToken, expiresAt: session.expiresAt }
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
  const stub = getDonationWallStub(env)

  const limit = await stub.consumeAdminLoginRateLimit(getClientIp(request), nowSec)
  if (!limit.allowed) {
    logAdminLogin({ ok: false, reason: 'rate_limited' })
    return adminJsonResponse(
      { error: 'rate_limited', retryAfterSeconds: limit.retryAfterSeconds },
      429,
      { 'Retry-After': String(limit.retryAfterSeconds) },
    )
  }

  if (password.length === 0 || !constantTimeEqual(password, adminPassword)) {
    logAdminLogin({ ok: false, reason: 'invalid_password' })
    return adminJsonResponse({ error: 'invalid_credentials' }, 401)
  }

  const sessionToken = randomBytes(32).toString('base64url')
  const csrfToken = randomBytes(32).toString('base64url')
  const sessionHash = deriveAdminSessionHash(adminPassword, sessionToken)
  const created = await stub.createAdminSession({ sessionHash, csrfToken, nowSec })

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

interface ValidAfdianOrderDraft {
  readonly order: AfdianOrderRecord
  readonly money: ParsedMoney
}

type BatchSponsorResolution =
  | {
    readonly ok: true
    readonly names: ReadonlyMap<string, string | null>
    readonly deferredUserIds: ReadonlySet<string>
  }
  | { readonly ok: false; readonly reason: string }

function collectMissingPaidAfdianOrders(
  paidOrders: readonly AfdianOrderRecord[],
  existingIds: ReadonlySet<string>,
): { readonly deduplicated: number; readonly validOrders: ValidAfdianOrderDraft[] } {
  const seenIds = new Set(existingIds)
  let deduplicated = 0
  const validOrders: ValidAfdianOrderDraft[] = []

  for (const order of paidOrders) {
    if (seenIds.has(order.outTradeNo)) {
      deduplicated += 1
      continue
    }
    seenIds.add(order.outTradeNo)
    const money = parseFixedPointAmount(order.totalAmount, 'CNY')
    if (money === null) {
      logWebhookRejected({ provider: 'afdian', reason: 'invalid_payload' })
      continue
    }
    validOrders.push({ order, money })
  }

  return { deduplicated, validOrders }
}

async function resolveMissedSponsorUsers(
  env: DonationsEnv,
  afdianToken: string,
  firstOrderByUser: ReadonlyMap<string, string>,
  names: Map<string, string | null>,
  deps: RuntimeDeps,
): Promise<Set<string>> {
  const deferredUserIds = new Set<string>()
  let directLookups = 0
  let rateLimited = false

  for (const [sponsorUserId, outTradeNo] of firstOrderByUser) {
    if (names.has(sponsorUserId)) continue
    if (rateLimited || directLookups >= MAX_AFDIAN_SYNC_SPONSOR_LOOKUPS) {
      // Defer unresolved sponsors beyond our subrequest budget or after a 429 rather than
      // persisting irreversible anonymous records; re-running Sync Afdian picks them up cleanly.
      deferredUserIds.add(sponsorUserId)
      continue
    }
    directLookups += 1
    const lookup = await lookupAfdianSponsorNameDirect({
      userId: env.AFDIAN_USER_ID,
      token: afdianToken,
      sponsorUserId,
      nowSec: deps.nowSec(),
      fetchFn: deps.fetchFn,
    })
    if (lookup.ok) {
      names.set(sponsorUserId, lookup.displayName)
      continue
    }
    logAfdianSponsorLookupFallback({ outTradeNo, reason: lookup.reason })
    if (lookup.reason === AFDIAN_RATE_LIMIT_REASON) {
      rateLimited = true
      deferredUserIds.add(sponsorUserId)
    } else {
      names.set(sponsorUserId, null)
    }
  }

  return deferredUserIds
}

async function resolveBatchSponsorNames(
  env: DonationsEnv,
  afdianToken: string,
  validOrders: readonly ValidAfdianOrderDraft[],
  deps: RuntimeDeps,
): Promise<BatchSponsorResolution> {
  const firstOrderByUser = new Map<string, string>()
  for (const { order } of validOrders) {
    if (order.userId.length > 0 && !firstOrderByUser.has(order.userId)) {
      firstOrderByUser.set(order.userId, order.outTradeNo)
    }
  }
  if (firstOrderByUser.size === 0) {
    return { ok: true, names: new Map(), deferredUserIds: new Set() }
  }

  // Prime a batch-wide map from page 1 once so recent sponsors resolve in a single outbound call.
  const pageResult = await queryAfdianSponsorPage({
    userId: env.AFDIAN_USER_ID,
    token: afdianToken,
    nowSec: deps.nowSec(),
    fetchFn: deps.fetchFn,
  })
  if (!pageResult.ok) {
    const sampleTradeNo = firstOrderByUser.values().next().value ?? 'batch'
    logAfdianSponsorLookupFallback({ outTradeNo: sampleTradeNo, reason: pageResult.reason })
    if (pageResult.reason === AFDIAN_RATE_LIMIT_REASON) {
      return { ok: false, reason: pageResult.reason }
    }
    return { ok: true, names: new Map(), deferredUserIds: new Set() }
  }

  const names = new Map(pageResult.sponsors)
  const deferredUserIds = await resolveMissedSponsorUsers(
    env,
    afdianToken,
    firstOrderByUser,
    names,
    deps,
  )
  return { ok: true, names, deferredUserIds }
}

function partitionReadyAfdianInputs(
  validOrders: readonly ValidAfdianOrderDraft[],
  names: ReadonlyMap<string, string | null>,
  deferredUserIds: ReadonlySet<string>,
): { readonly readyInputs: CommitDonationInput[]; readonly deferred: number } {
  const readyInputs: CommitDonationInput[] = []
  let deferred = 0
  // Afdian returns query-order newest-first; build commit inputs oldest-first so
  // batch inserts and audit logs preserve chronological order.
  for (const { order, money } of validOrders.toReversed()) {
    if (order.userId.length > 0 && deferredUserIds.has(order.userId)) {
      deferred += 1
      continue
    }
    const rawDisplayName = order.userId.length > 0 ? (names.get(order.userId) ?? null) : null
    readyInputs.push(toAfdianCommitInput(order, money, rawDisplayName))
  }
  return { readyInputs, deferred }
}

function finalizeAfdianSyncResponse(summary: {
  readonly fetched: number
  readonly paid: number
  readonly committed: number
  readonly deduplicated: number
  readonly deferred: number
}): Response {
  logAdminAfdianSynced(summary)
  return adminJsonResponse(summary)
}

async function handleSyncAfdian(env: DonationsEnv, deps: RuntimeDeps): Promise<Response> {
  if (!env.AFDIAN_TOKEN) {
    return adminJsonResponse({ error: 'afdian_not_configured' }, 503)
  }
  const orderQuery = await queryAfdianOrders({
    userId: env.AFDIAN_USER_ID,
    token: env.AFDIAN_TOKEN,
    page: 1,
    perPage: AFDIAN_SYNC_PER_PAGE,
    nowSec: deps.nowSec(),
    fetchFn: deps.fetchFn,
  })
  if (!orderQuery.ok) {
    return adminJsonResponse({ error: 'afdian_query_failed', reason: orderQuery.reason }, 502)
  }

  const fetched = orderQuery.orders.length
  const paidOrders = orderQuery.orders.filter(order => order.status === AFDIAN_PAID_STATUS)
  if (paidOrders.length === 0) {
    return finalizeAfdianSyncResponse({ fetched, paid: 0, committed: 0, deduplicated: 0, deferred: 0 })
  }

  const existingIds = await findExistingDonationsInWall(
    env,
    'afdian',
    paidOrders.map(order => order.outTradeNo),
  )
  const missing = collectMissingPaidAfdianOrders(paidOrders, existingIds)
  if (missing.validOrders.length === 0) {
    return finalizeAfdianSyncResponse({
      fetched,
      paid: paidOrders.length,
      committed: 0,
      deduplicated: missing.deduplicated,
      deferred: 0,
    })
  }

  const sponsors = await resolveBatchSponsorNames(env, env.AFDIAN_TOKEN, missing.validOrders, deps)
  if (!sponsors.ok) {
    return adminJsonResponse({ error: 'afdian_rate_limited', reason: sponsors.reason }, 502)
  }

  const { readyInputs, deferred } = partitionReadyAfdianInputs(
    missing.validOrders,
    sponsors.names,
    sponsors.deferredUserIds,
  )
  const results = readyInputs.length > 0
    ? await commitDonationsBatchToWall(env, readyInputs, deps.nowSec())
    : []
  const committed = results.filter(result => !result.deduplicated).length
  const deduplicated = missing.deduplicated + (results.length - committed)

  return finalizeAfdianSyncResponse({
    fetched,
    paid: paidOrders.length,
    committed,
    deduplicated,
    deferred,
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

  const result = await getDonationWallStub(env).moderateDonationVisibility(
    donationId,
    command,
    deps.nowSec(),
  )
  if (!result.ok) {
    return adminJsonResponse({ error: result.code }, result.code === 'not_found' ? 404 : 409)
  }

  logDonationModerated({
    donationId: result.record.id,
    action: command.action,
    fromVisibility: result.fromVisibility.kind,
    toVisibility: result.toVisibility.kind,
  })
  return adminJsonResponse({ record: result.record }, 200)
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
  const config = await getDonationWallStub(env).patchAdminConfig(patch)
  logAdminConfigUpdated(patch)
  return adminJsonResponse({ config }, 200)
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
    await getDonationWallStub(env).revokeAdminSession(session.sessionHash)
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

function summarizeStorageError(error: unknown): {
  readonly message: string
  readonly retryable: boolean
  readonly overloaded: boolean
} {
  const message = error instanceof Error ? error.message : String(error)
  const retryable = typeof error === 'object'
    && error !== null
    && (error as { retryable?: unknown }).retryable === true
  const overloaded = typeof error === 'object'
    && error !== null
    && (error as { overloaded?: unknown }).overloaded === true
  return { message, retryable, overloaded }
}

async function dispatchAdminApi(
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
    const config = await getDonationWallStub(env).getAdminConfig()
    return adminJsonResponse({
      authenticated: true,
      csrfToken: session.csrfToken,
      expiresAt: session.expiresAt,
      config,
    })
  }

  if (method === 'GET' && pathname === '/v1/admin/donations') {
    const after = url.searchParams.get('after')
    const limit = parseOptionalIntParam(url.searchParams.get('limit'))
    const page = await getDonationWallStub(env).listAdminDonations(after, limit)
    return adminJsonResponse(page, 200)
  }

  const mutationResponse = await handleAuthenticatedAdminMutation(request, env, url, session, deps)
  if (mutationResponse !== null) return mutationResponse

  return adminJsonResponse({ error: 'not_found' }, 404)
}

export async function handleAdminApi(
  request: Request,
  env: DonationsEnv,
  url: URL,
  deps: RuntimeDeps,
): Promise<Response> {
  try {
    return await dispatchAdminApi(request, env, url, deps)
  } catch (error) {
    // Fail closed with structured admin headers instead of leaking an unhandled Worker error
    // whenever Durable Object RPC rejects (storage outage, overload, or reset).
    logAdminStorageError({
      method: request.method,
      pathname: url.pathname,
      ...summarizeStorageError(error),
    })
    return adminJsonResponse({ error: 'service_unavailable' }, 503)
  }
}
