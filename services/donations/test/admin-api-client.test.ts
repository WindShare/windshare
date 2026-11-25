import { randomBytes } from 'node:crypto'
import { describe, expect, it, vi } from 'vitest'
import {
  AdminApiClient,
  formatApiError,
  type AdminFetchFn,
  type AdminFetchResponse,
  type ManualDonationPayload,
} from '../admin-ui/src/api'
import { getCopy } from '../admin-ui/src/locales'
import type { DonationsEnv } from '../src/env'
import {
  defaultDonationConfig,
  emptyVisibilityCounts,
  type DonationRecord,
} from '../src/model'
import { handleWorkerFetch } from '../src/router'
import { createInMemoryWallStore, createWallNamespaceFromStore } from './wall-harness'

const API_ORIGIN = 'https://donate.windshare.cc'

function makeSampleRecord(overrides: Partial<DonationRecord> = {}): DonationRecord {
  return {
    id: 'don_1',
    provider: 'direct',
    externalId: 'manual_ref_1',
    displayName: 'Alice',
    amountMinor: 2500,
    currency: 'USD',
    minorDigits: 2,
    message: 'Thanks!',
    visibility: { kind: 'published' },
    donatedAt: 1_700_000_000,
    ...overrides,
  }
}

function makeManualPayload(overrides: Partial<ManualDonationPayload> = {}): ManualDonationPayload {
  return {
    provider: 'direct',
    amount: '25',
    currency: 'USD',
    displayName: 'Alice',
    message: 'Thanks!',
    externalId: null,
    donatedAt: null,
    ...overrides,
  }
}

function makeMockResponse(options: {
  readonly ok: boolean
  readonly status: number
  readonly headers?: Record<string, string>
  readonly json: () => Promise<unknown>
}): AdminFetchResponse {
  const headers = new Headers(options.headers)
  return {
    ok: options.ok,
    status: options.status,
    headers,
    json: options.json,
  }
}

describe('AdminApiClient response validation and transport semantics', () => {
  it('rejects truncated, dropped, or malformed 201 manual-donation responses instead of fabricating {}', async () => {
    const expired = vi.fn()

    const droppedStreamClient = new AdminApiClient(expired, async () => makeMockResponse({
      ok: true,
      status: 201,
      json: () => Promise.reject(new TypeError('terminated')),
    }))
    const droppedResult = await droppedStreamClient.createManualDonation(makeManualPayload(), 'idem-drop-1')
    expect(droppedResult).toEqual({
      ok: false,
      status: 0,
      error: 'network_error',
      retryAfterSeconds: null,
    })

    const truncatedJsonClient = new AdminApiClient(expired, async () => makeMockResponse({
      ok: true,
      status: 201,
      json: () => Promise.reject(new SyntaxError('Unexpected end of JSON input')),
    }))
    const truncatedResult = await truncatedJsonClient.createManualDonation(makeManualPayload(), 'idem-trunc-1')
    expect(truncatedResult).toEqual({
      ok: false,
      status: 201,
      error: 'invalid_response',
      retryAfterSeconds: null,
    })

    const missingCountsClient = new AdminApiClient(expired, async () => makeMockResponse({
      ok: true,
      status: 201,
      json: async () => ({}),
    }))
    const missingCountsResult = await missingCountsClient.createManualDonation(makeManualPayload(), 'idem-empty-1')
    expect(missingCountsResult).toEqual({
      ok: false,
      status: 201,
      error: 'invalid_response',
      retryAfterSeconds: null,
    })

    expect(expired).not.toHaveBeenCalled()
  })

  it('allows idempotent retry against the real worker after a mid-body disconnect on HTTP 201', async () => {
    const store = createInMemoryWallStore()
    const adminPassword = randomBytes(16).toString('hex')
    let now = 1_700_000_000
    const env: DonationsEnv = {
      DONATION_WALL: createWallNamespaceFromStore(store),
      AFDIAN_USER_ID: 'creator',
      ALLOWED_ORIGINS: 'https://windshare.cc',
      DONATE_API_ORIGIN: API_ORIGIN,
      DONATE_ADMIN_PASSWORD: adminPassword,
    }

    let cookie = ''
    let dropFirstManualBody = true
    const observedIdempotencyKeys: Array<string | null> = []

    const workerFetch: AdminFetchFn = async (path, init) => {
      const headers = new Headers(init.headers)
      headers.set('Origin', API_ORIGIN)
      if (cookie.length > 0) headers.set('Cookie', cookie)
      if (path === '/v1/admin/donations' && init.method === 'POST') {
        observedIdempotencyKeys.push(headers.get('Idempotency-Key'))
      }

      const res = await handleWorkerFetch(
        new Request(`${API_ORIGIN}${path}`, {
          method: init.method,
          headers,
          ...(init.body === undefined ? {} : { body: init.body }),
        }),
        env,
        { nowSec: () => now },
      )
      const setCookie = res.headers.get('set-cookie')
      if (setCookie !== null) {
        cookie = setCookie.split(';', 1)[0] ?? ''
      }

      if (path === '/v1/admin/donations' && init.method === 'POST' && res.status === 201 && dropFirstManualBody) {
        dropFirstManualBody = false
        // Simulate connection closing after HTTP 201 headers arrive and part of the JSON body is lost.
        return makeMockResponse({
          ok: true,
          status: 201,
          json: () => Promise.reject(new SyntaxError('Unexpected end of JSON input')),
        })
      }

      return res
    }

    const client = new AdminApiClient(vi.fn(), workerFetch)
    const login = await client.login(adminPassword)
    expect(login.ok).toBe(true)
    if (!login.ok) throw new Error('expected login to succeed')
    client.setCsrf(login.data.csrfToken)

    const payload = makeManualPayload()
    const key = '550e8400-e29b-41d4-a716-446655440015'

    const firstAttempt = await client.createManualDonation(payload, key)
    expect(firstAttempt).toEqual({
      ok: false,
      status: 201,
      error: 'invalid_response',
      retryAfterSeconds: null,
    })
    expect(store.listAdminDonations(null, 10).items).toHaveLength(1)

    now += 8
    const retryAttempt = await client.createManualDonation(payload, key)
    expect(retryAttempt).not.toBeNull()
    expect(retryAttempt?.ok).toBe(true)
    if (retryAttempt === null || !retryAttempt.ok) {
      throw new Error('expected idempotent retry to succeed')
    }
    expect(retryAttempt.status).toBe(200)
    expect(retryAttempt.data.counts.published).toBe(1)
    expect(retryAttempt.data.counts.pending_review).toBe(0)
    expect(store.listAdminDonations(null, 10).items).toHaveLength(1)
    expect(observedIdempotencyKeys).toEqual([key, key])
  })

  it('propagates AbortError during both 2xx and error body reads as null instead of corrupting state', async () => {
    const okAbortClient = new AdminApiClient(vi.fn(), async () => makeMockResponse({
      ok: true,
      status: 200,
      json: () => Promise.reject(new DOMException('The operation was aborted.', 'AbortError')),
    }))
    expect(await okAbortClient.listDonations({})).toBeNull()

    const errAbortClient = new AdminApiClient(vi.fn(), async () => makeMockResponse({
      ok: false,
      status: 500,
      json: () => Promise.reject(new DOMException('The operation was aborted.', 'AbortError')),
    }))
    expect(await errAbortClient.listDonations({})).toBeNull()
  })

  it('preserves graceful degradation for non-JSON error pages and 401 session expiry', async () => {
    const expired = vi.fn()

    const badGatewayClient = new AdminApiClient(expired, async () => makeMockResponse({
      ok: false,
      status: 502,
      json: () => Promise.reject(new SyntaxError('Unexpected token < in JSON')),
    }))
    expect(await badGatewayClient.listDonations({})).toEqual({
      ok: false,
      status: 502,
      error: '502',
      retryAfterSeconds: null,
    })

    const rateLimitedClient = new AdminApiClient(expired, async () => makeMockResponse({
      ok: false,
      status: 429,
      headers: { 'Retry-After': '12.2' },
      json: async () => ({ error: 'rate_limited' }),
    }))
    expect(await rateLimitedClient.login('secret')).toEqual({
      ok: false,
      status: 429,
      error: 'rate_limited',
      retryAfterSeconds: 13,
    })

    const observedHeaders: Array<Readonly<Record<string, string>>> = []
    const unauthorizedClient = new AdminApiClient(expired, async (_path, init) => {
      observedHeaders.push(init.headers)
      return makeMockResponse({
        ok: false,
        status: 401,
        json: () => Promise.reject(new SyntaxError('empty body')),
      })
    })
    unauthorizedClient.setCsrf('csrf-before-expiry')
    expect(await unauthorizedClient.moderate('don_1', { action: 'approve' })).toBeNull()
    expect(expired).toHaveBeenCalledTimes(1)

    // Subsequent mutations must not reuse the cleared CSRF token.
    await unauthorizedClient.logout()
    expect(observedHeaders[0]?.['X-CSRF-Token']).toBe('csrf-before-expiry')
    expect(observedHeaders[1]?.['X-CSRF-Token']).toBeUndefined()
  })

  it('validates response shapes across all admin endpoints and rejects malformed 2xx payloads', async () => {
    const validConfig = defaultDonationConfig()
    const validCounts = { ...emptyVisibilityCounts(), total: 1, published: 1 }
    const validRecord = makeSampleRecord({
      visibility: {
        kind: 'hidden',
        moderatedAt: 1_700_000_010,
        reason: 'moderated',
        previous: { kind: 'pending_review', reason: 'url_detected' },
      },
    })

    const validResponses = new Map<string, unknown>([
      ['GET /v1/admin/session', {
        authenticated: true,
        csrfToken: 'csrf-1',
        expiresAt: 1_700_043_200,
        config: validConfig,
        afdianSyncConfigured: true,
      }],
      ['POST /v1/admin/login', {
        authenticated: true,
        csrfToken: 'csrf-2',
        expiresAt: 1_700_043_200,
        afdianSyncConfigured: false,
      }],
      ['POST /v1/admin/logout', { ok: true }],
      ['GET /v1/admin/donations', {
        items: [validRecord],
        nextCursor: null,
        config: validConfig,
        counts: validCounts,
      }],
      ['POST /v1/admin/donations/don_1/visibility', { record: validRecord, counts: validCounts }],
      ['POST /v1/admin/donations/sync-afdian', {
        fetched: 3,
        paid: 2,
        committed: 1,
        deduplicated: 1,
        deferred: 0,
      }],
      ['PATCH /v1/admin/donations/config', { config: validConfig }],
    ])

    const validClient = new AdminApiClient(vi.fn(), async (path, init) => makeMockResponse({
      ok: true,
      status: 200,
      json: async () => validResponses.get(`${init.method} ${path}`),
    }))

    expect((await validClient.checkSession()).ok).toBe(true)
    expect((await validClient.login('pw')).ok).toBe(true)
    expect((await validClient.logout()).ok).toBe(true)
    expect((await validClient.listDonations({}))?.ok).toBe(true)
    expect((await validClient.moderate('don_1', { action: 'restore' }))?.ok).toBe(true)
    expect((await validClient.syncAfdian())?.ok).toBe(true)
    expect((await validClient.patchConfig({ moderationEnabled: true }))?.ok).toBe(true)

    const malformedClient = new AdminApiClient(vi.fn(), async () => makeMockResponse({
      ok: true,
      status: 200,
      json: async () => ({}),
    }))

    for (const result of [
      await malformedClient.checkSession(),
      await malformedClient.login('pw'),
      await malformedClient.logout(),
      await malformedClient.listDonations({}),
      await malformedClient.moderate('don_1', { action: 'approve' }),
      await malformedClient.syncAfdian(),
      await malformedClient.patchConfig({ moderationEnabled: true }),
    ]) {
      expect(result).toEqual({
        ok: false,
        status: 200,
        error: 'invalid_response',
        retryAfterSeconds: null,
      })
    }
  })

  it('formats invalid_response errors with localized copy in both locales', () => {
    const failure = {
      ok: false as const,
      status: 201,
      error: 'invalid_response',
      retryAfterSeconds: null,
    }
    expect(formatApiError(failure, getCopy('en'))).toBe(getCopy('en').errors.invalidResponse)
    expect(formatApiError(failure, getCopy('zh-CN'))).toBe(getCopy('zh-CN').errors.invalidResponse)
  })
})
