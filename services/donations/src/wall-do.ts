import { DurableObject } from 'cloudflare:workers'
import type { DonationsEnv } from './env'
import type {
  AdminConfigPatch,
  AdminVisibilityCommand,
  CommitDonationInput,
  DonationProvider,
} from './model'
import { WallStore, matchesIfNoneMatch } from './wall-store'

const INTERNAL_INTENT_PREFIX = '/internal/crypto-intents/'
const INTERNAL_DONATION_VISIBILITY_PREFIX = '/internal/admin/donations/'
const INTERNAL_DONATION_VISIBILITY_SUFFIX = '/visibility'

function nowSeconds(): number {
  return Math.floor(Date.now() / 1000)
}

// Callers receive values already validated by the admin route layer; this only coerces.
function parseOptionalIntParam(raw: string | null): number | undefined {
  if (raw === null) return undefined
  const parsed = Number(raw)
  return Number.isFinite(parsed) ? parsed : undefined
}

function json(body: unknown, status = 200, extraHeaders?: Record<string, string>): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      'Content-Type': 'application/json; charset=utf-8',
      ...extraHeaders,
    },
  })
}

export class DonationWall extends DurableObject<DonationsEnv> {
  private readonly store: WallStore

  constructor(ctx: DurableObjectState, env: DonationsEnv) {
    super(ctx, env)
    this.store = new WallStore(
      this.ctx.storage.sql,
      fn => this.ctx.storage.transactionSync(fn),
    )
  }

  async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url)
    const { method } = request
    const { pathname } = url

    if (method === 'GET' && pathname === '/internal/wall') {
      return this.handlePublicWall(request)
    }
    const donationRoute = await this.handleDonationInternalRoute(request, method, url)
    if (donationRoute !== null) return donationRoute

    const adminRoute = await this.handleAdminInternalRoute(request, method, url)
    if (adminRoute !== null) return adminRoute

    return json({ error: 'not_found' }, 404)
  }

  private handlePublicWall(request: Request): Response {
    const snapshot = this.store.getPublicWallSnapshot()
    if (matchesIfNoneMatch(request.headers.get('If-None-Match'), snapshot.etag)) {
      return new Response(null, {
        status: 304,
        headers: { ETag: snapshot.etag },
      })
    }
    return new Response(snapshot.json, {
      status: 200,
      headers: {
        'Content-Type': 'application/json; charset=utf-8',
        ETag: snapshot.etag,
      },
    })
  }

  private async handleCreateIntentInternal(request: Request): Promise<Response> {
    const body = await request.json() as {
      clientIp: string
      displayName: string | null
      message: string | null
      requestedUsdMinor: number
      returnOrigin: string
      nowSec?: number
    }
    const result = this.store.createCryptoIntent({
      clientIp: body.clientIp,
      displayName: body.displayName,
      message: body.message,
      requestedUsdMinor: body.requestedUsdMinor,
      returnOrigin: body.returnOrigin,
      nowSec: body.nowSec ?? nowSeconds(),
    })
    if (!result.ok) {
      if (result.code === 'crypto_checkout_paused') {
        return json({ error: result.code }, 503)
      }
      return json(
        { error: result.code, retryAfterSeconds: result.retryAfterSeconds },
        429,
        { 'Retry-After': String(result.retryAfterSeconds) },
      )
    }
    return json({ intent: result.intent }, 201)
  }

  private async handleDonationInternalRoute(
    request: Request,
    method: string,
    url: URL,
  ): Promise<Response | null> {
    const { pathname } = url
    if (method === 'POST' && pathname === '/internal/crypto-intents') {
      return this.handleCreateIntentInternal(request)
    }
    if (method === 'GET' && pathname.startsWith(INTERNAL_INTENT_PREFIX)) {
      const intentId = decodeURIComponent(pathname.slice(INTERNAL_INTENT_PREFIX.length))
      const nowSec = parseOptionalIntParam(url.searchParams.get('nowSec')) ?? nowSeconds()
      return json({ intent: this.store.getCryptoIntent(intentId, nowSec) }, 200)
    }
    if (method === 'GET' && pathname === '/internal/donations/exists') {
      const provider = url.searchParams.get('provider') as DonationProvider | null
      const externalId = url.searchParams.get('externalId')
      if (provider === null || externalId === null) {
        return json({ error: 'invalid_query' }, 400)
      }
      return json({ exists: this.store.hasDonation(provider, externalId) }, 200)
    }
    if (method === 'POST' && pathname === '/internal/donations/commit') {
      const body = await request.json() as { input: CommitDonationInput; nowSec?: number }
      return json(this.store.commitDonation(body.input, body.nowSec ?? nowSeconds()), 200)
    }
    return this.handleDonationBatchRoute(request, method, pathname)
  }

  private async handleDonationBatchRoute(
    request: Request,
    method: string,
    pathname: string,
  ): Promise<Response | null> {
    if (method === 'POST' && pathname === '/internal/donations/exists-batch') {
      const body = await request.json() as { provider?: DonationProvider; externalIds?: unknown }
      if (typeof body.provider !== 'string' || !Array.isArray(body.externalIds)) {
        return json({ error: 'invalid_payload' }, 400)
      }
      const externalIds = body.externalIds.filter((id): id is string => typeof id === 'string')
      return json({ existingIds: this.store.findExistingExternalIds(body.provider, externalIds) }, 200)
    }
    if (method === 'POST' && pathname === '/internal/donations/commit-batch') {
      const body = await request.json() as { inputs?: unknown; nowSec?: number }
      if (!Array.isArray(body.inputs)) {
        return json({ error: 'invalid_payload' }, 400)
      }
      const results = this.store.commitDonationsBatch(
        body.inputs as CommitDonationInput[],
        body.nowSec ?? nowSeconds(),
      )
      return json({ results }, 200)
    }
    return null
  }

  private async handleVisibilityInternal(request: Request, pathname: string): Promise<Response> {
    const donationId = decodeURIComponent(
      pathname.slice(
        INTERNAL_DONATION_VISIBILITY_PREFIX.length,
        pathname.length - INTERNAL_DONATION_VISIBILITY_SUFFIX.length,
      ),
    )
    const body = await request.json() as { command: AdminVisibilityCommand; nowSec?: number }
    const result = this.store.moderateDonationVisibility(
      donationId,
      body.command,
      body.nowSec ?? nowSeconds(),
    )
    if (!result.ok) {
      return json({ error: result.code }, result.code === 'not_found' ? 404 : 409)
    }
    return json(result, 200)
  }

  private async handleAdminConfigRoute(
    request: Request,
    method: string,
    pathname: string,
  ): Promise<Response | null> {
    if (method === 'GET' && pathname === '/internal/admin/config') {
      return json({ config: this.store.getConfig() }, 200)
    }
    if (method === 'PATCH' && pathname === '/internal/admin/config') {
      const patch = await request.json() as AdminConfigPatch
      return json({ config: this.store.patchConfig(patch) }, 200)
    }
    return null
  }

  private async handleAdminInternalRoute(
    request: Request,
    method: string,
    url: URL,
  ): Promise<Response | null> {
    const { pathname } = url
    if (method === 'POST' && pathname === '/internal/admin/rate-limit') {
      const body = await request.json() as { clientIp: string; nowSec?: number }
      const decision = this.store.consumeDualRateLimit(
        'admin_login',
        body.clientIp,
        body.nowSec ?? nowSeconds(),
      )
      return json(decision, decision.allowed ? 200 : 429)
    }

    const sessionRoute = await this.handleAdminSessionRoute(request, method, pathname)
    if (sessionRoute !== null) return sessionRoute

    const configRoute = await this.handleAdminConfigRoute(request, method, pathname)
    if (configRoute !== null) return configRoute

    if (method === 'GET' && pathname === '/internal/admin/donations') {
      const after = url.searchParams.get('after')
      const limit = parseOptionalIntParam(url.searchParams.get('limit'))
      return json(this.store.listAdminDonations(after, limit), 200)
    }

    if (
      method === 'POST'
      && pathname.startsWith(INTERNAL_DONATION_VISIBILITY_PREFIX)
      && pathname.endsWith(INTERNAL_DONATION_VISIBILITY_SUFFIX)
    ) {
      return this.handleVisibilityInternal(request, pathname)
    }

    return null
  }

  private async handleAdminSessionRoute(
    request: Request,
    method: string,
    pathname: string,
  ): Promise<Response | null> {
    if (method === 'POST' && pathname === '/internal/admin/sessions') {
      const body = await request.json() as {
        sessionHash: string
        csrfToken: string
        nowSec?: number
      }
      const session = this.store.createAdminSession({
        sessionHash: body.sessionHash,
        csrfToken: body.csrfToken,
        nowSec: body.nowSec ?? nowSeconds(),
      })
      return json(session, 201)
    }
    if (method === 'POST' && pathname === '/internal/admin/sessions/verify') {
      const body = await request.json() as { sessionHash: string; nowSec?: number }
      const session = this.store.verifyAdminSession(body.sessionHash, body.nowSec ?? nowSeconds())
      return session === null ? json({ valid: false }, 401) : json({ valid: true, ...session }, 200)
    }
    if (method === 'POST' && pathname === '/internal/admin/sessions/revoke') {
      const body = await request.json() as { sessionHash: string }
      this.store.revokeAdminSession(body.sessionHash)
      return json({ ok: true }, 200)
    }
    return null
  }
}
