import { randomBytes } from 'node:crypto'
import { describe, expect, it } from 'vitest'
import {
  parseAllowedOrigins,
  resolveCryptoCheckoutApiKey,
  type DonationWallRpc,
  type DonationsEnv,
} from '../src/env'
import {
  MIN_PUBLIC_CNY_MINOR,
  MIN_PUBLIC_USD_MINOR,
  defaultDonationConfig,
} from '../src/model'
import { handleWorkerFetch } from '../src/router'
import {
  containsContactPii,
  containsExternalUrl,
  determineInitialVisibility,
  parseFixedPointAmount,
  sanitizeAndTruncatePlainText,
  sanitizePlainText,
} from '../src/sanitize'
import {
  computeNowPaymentsSignature,
  constantTimeEqual,
  hmacSha256Hex,
  hmacSha512Hex,
  md5Hex,
} from '../src/verify'
import { computePublicWallEtag } from '../src/wall-store'

const EMPTY_WALL_JSON = JSON.stringify({ version: 0, entries: [], topSupporters: [] })
const EMPTY_WALL_ETAG = computePublicWallEtag(0, EMPTY_WALL_JSON)

function createStubEnv(): DonationsEnv {
  const namespace = {
    idFromName: (name: string) => ({ toString: () => name }),
    get: () => ({
      getPublicWall: async () => ({
        notModified: false as const,
        etag: EMPTY_WALL_ETAG,
        json: EMPTY_WALL_JSON,
      }),
    }),
  } as unknown as DurableObjectNamespace<DonationWallRpc>

  return {
    DONATION_WALL: namespace,
    AFDIAN_USER_ID: 'test-user',
    ALLOWED_ORIGINS: 'https://ws.ourworld.top, https://windshare.cc, invalid-origin',
    DONATE_API_ORIGIN: 'https://donate.windshare.cc',
  }
}

describe('donations worker scaffold', () => {
  it('parses valid allowed origins and ignores malformed entries', () => {
    const origins = parseAllowedOrigins('https://ws.ourworld.top/, https://windshare.cc, not-a-url')
    expect([...origins]).toEqual(['https://ws.ourworld.top', 'https://windshare.cc'])
  })

  it('sanitizes plain text without truncating and truncates explicitly when bounded', () => {
    expect(sanitizePlainText('  hello\u0000\u200B\nworld  ')).toBe('hello world')
    expect(sanitizePlainText('😀😀😀😀')).toBe('😀😀😀😀')
    expect(sanitizePlainText('   \n\t  ')).toBeNull()

    expect(sanitizeAndTruncatePlainText('  hello\u0000\u200B\nworld  ', 32)).toBe('hello world')
    expect(sanitizeAndTruncatePlainText('😀😀😀😀', 3)).toBe('😀😀😀')
    expect(sanitizeAndTruncatePlainText('abc   def', 5)).toBe('abc')
    expect(sanitizeAndTruncatePlainText('   \n\t  ', 32)).toBeNull()
  })

  it('strips Unicode BiDi controls and soft hyphens and collapses LS/PS before moderation checks', () => {
    const allBidiControls = '\u061C\u200E\u200F\u202A\u202B\u202C\u202D\u202E\u2066\u2067\u2068\u2069'
    expect(sanitizePlainText(`${allBidiControls}Alice\u00AD`)).toBe('Alice')
    expect(sanitizePlainText(`${allBidiControls}\u2028\u2029`)).toBeNull()
    expect(sanitizePlainText('line1\u2028line2\u2029line3')).toBe('line1 line2 line3')
    expect(sanitizePlainText('a\r\n\u2028\u2029b')).toBe('a b')

    const bidiUrl = sanitizeAndTruncatePlainText('https:\u200E//spam.example', 140)
    const softHyphenUrl = sanitizeAndTruncatePlainText('https:\u00AD//spam.example', 140)
    const bidiEmail = sanitizeAndTruncatePlainText('a\u200E@b.com', 140)
    const bidiPhone = sanitizeAndTruncatePlainText('138\u200E1234\u00AD5678', 140)
    const separatorPhone = sanitizeAndTruncatePlainText('138\u20281234\u20295678', 140)

    expect(bidiUrl).toBe('https://spam.example')
    expect(softHyphenUrl).toBe('https://spam.example')
    expect(bidiEmail).toBe('a@b.com')
    expect(bidiPhone).toBe('13812345678')
    expect(separatorPhone).toBe('138 1234 5678')

    expect(containsExternalUrl(bidiUrl)).toBe(true)
    expect(containsExternalUrl(softHyphenUrl)).toBe(true)
    expect(containsContactPii(bidiEmail)).toBe(true)
    expect(containsContactPii(bidiPhone)).toBe(true)
    expect(containsContactPii(separatorPhone)).toBe(true)

    const config = defaultDonationConfig()
    expect(determineInitialVisibility({
      amountMinor: MIN_PUBLIC_USD_MINOR,
      currency: 'USD',
      displayName: sanitizeAndTruncatePlainText('\u202EAlice', 32),
      message: bidiUrl,
      isPublic: true,
      config,
    })).toEqual({ kind: 'pending_review', reason: 'url_detected' })
    expect(determineInitialVisibility({
      amountMinor: MIN_PUBLIC_CNY_MINOR,
      currency: 'CNY',
      displayName: 'Bob',
      message: bidiPhone,
      isPublic: true,
      config,
    })).toEqual({ kind: 'pending_review', reason: 'pii_detected' })
  })

  it('parses fixed-point amounts accurately for known and unknown currencies', () => {
    expect(parseFixedPointAmount('10.00', 'cny')).toEqual({
      amountMinor: 1000,
      currency: 'CNY',
      minorDigits: 2,
      knownCurrency: true,
    })
    expect(parseFixedPointAmount(5.5, 'USD')).toEqual({
      amountMinor: 550,
      currency: 'USD',
      minorDigits: 2,
      knownCurrency: true,
    })
    expect(parseFixedPointAmount('1000', 'jpy')).toEqual({
      amountMinor: 1000,
      currency: 'JPY',
      minorDigits: 0,
      knownCurrency: true,
    })
    expect(parseFixedPointAmount('10000', 'krw')).toEqual({
      amountMinor: 10000,
      currency: 'KRW',
      minorDigits: 0,
      knownCurrency: true,
    })
    expect(parseFixedPointAmount('12.345', 'SEK')).toEqual({
      amountMinor: 12345,
      currency: 'SEK',
      minorDigits: 3,
      knownCurrency: false,
    })
    expect(parseFixedPointAmount('0', 'USD')).toBeNull()
    expect(parseFixedPointAmount('10.001', 'USD')).toBeNull()
  })

  it('provides constant-time comparison and protocol hash helpers', () => {
    expect(constantTimeEqual('secret-token', 'secret-token')).toBe(true)
    expect(constantTimeEqual('secret-token', 'other')).toBe(false)
    // RFC 1321 canonical MD5 test vectors.
    expect(md5Hex('')).toBe('d41d8cd98f00b204e9800998ecf8427e')
    expect(md5Hex('abc')).toBe('900150983cd24fb0d6963f7d28e17f72')
    expect(hmacSha256Hex('key', 'payload')).toMatch(/^[0-9a-f]{64}$/)
    expect(hmacSha512Hex('key', 'payload')).toMatch(/^[0-9a-f]{128}$/)
  })

  it('serves healthz, public wall with ACAO *, and strict CSP admin assets', async () => {
    const env = createStubEnv()

    const health = await handleWorkerFetch(new Request('https://donate.windshare.cc/healthz'), env)
    expect(health.status).toBe(200)
    expect(await health.json()).toEqual({ ok: true, service: 'windshare-donations' })

    const wall = await handleWorkerFetch(new Request('https://donate.windshare.cc/v1/donations/wall'), env)
    expect(wall.status).toBe(200)
    expect(wall.headers.get('access-control-allow-origin')).toBe('*')
    expect(wall.headers.get('etag')).toBe(EMPTY_WALL_ETAG)
    expect(await wall.json()).toEqual({ version: 0, entries: [], topSupporters: [] })

    const admin = await handleWorkerFetch(new Request('https://donate.windshare.cc/admin'), env)
    expect(admin.status).toBe(200)
    expect(admin.headers.get('cache-control')).toBe('no-store')
    expect(admin.headers.get('content-security-policy')).toContain("script-src 'self'")

    const missing = await handleWorkerFetch(new Request('https://donate.windshare.cc/unknown'), env)
    expect(missing.status).toBe(404)
  })

  it('requires both NOWPayments secrets for crypto checkout while allowing IPN settlement without API key', async () => {
    const allowedOrigin = 'https://windshare.cc'
    const apiOrigin = 'https://donate.windshare.cc'
    const ipnSecret = 'test-nowpayments-ipn-secret'
    const apiKey = 'test-nowpayments-api-key'

    let doCalls = 0
    let outboundCalls = 0
    let committedExternalId: string | null = null

    const createEnv = (overrides: Partial<DonationsEnv>): DonationsEnv => {
      const namespace = {
        idFromName: (name: string) => ({ toString: () => name }),
        get: () => ({
          createCryptoIntent: async () => {
            doCalls += 1
            return {
              ok: true as const,
              intent: {
                intentId: 'intent-ready-1',
                displayName: 'Crypto Fan',
                message: 'Thanks!',
                requestedUsdMinor: 1000,
                returnOrigin: allowedOrigin,
                createdAt: 1_700_000_000,
                expiresAt: 1_700_604_800,
              },
            }
          },
          getCryptoIntent: async () => {
            doCalls += 1
            return null
          },
          commitDonation: async (input: { externalId: string }) => {
            doCalls += 1
            committedExternalId = input.externalId
            return {
              record: {
                id: 'don-1',
                provider: 'nowpayments' as const,
                externalId: input.externalId,
                displayName: null,
                amountMinor: 1000,
                currency: 'USD',
                minorDigits: 2,
                message: null,
                visibility: { kind: 'published' as const },
                donatedAt: 1_700_000_000,
              },
              outcome: 'inserted' as const,
            }
          },
        }),
      } as unknown as DurableObjectNamespace<DonationWallRpc>

      return {
        ...createStubEnv(),
        DONATION_WALL: namespace,
        ...overrides,
      }
    }

    const postIntent = (env: DonationsEnv) => handleWorkerFetch(
      new Request(`${apiOrigin}/v1/donations/crypto-intents`, {
        method: 'POST',
        headers: {
          Origin: allowedOrigin,
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({ amountUsd: '10', displayName: 'Crypto Fan', message: 'Thanks!' }),
      }),
      env,
      {
        nowSec: () => 1_700_000_000,
        fetchFn: async () => {
          outboundCalls += 1
          return Response.json({ invoice_url: 'https://nowpayments.io/payment/?iid=intent-ready-1' })
        },
      },
    )

    const partialConfigs: Array<Partial<DonationsEnv>> = [
      {},
      { NOWPAYMENTS_API_KEY: apiKey },
      { NOWPAYMENTS_API_KEY: apiKey, NOWPAYMENTS_IPN_SECRET: '' },
      { NOWPAYMENTS_IPN_SECRET: ipnSecret },
      { NOWPAYMENTS_API_KEY: '', NOWPAYMENTS_IPN_SECRET: ipnSecret },
    ]
    for (const partial of partialConfigs) {
      const env = createEnv(partial)
      expect(resolveCryptoCheckoutApiKey(env)).toBeNull()
      const res = await postIntent(env)
      expect(res.status).toBe(503)
      expect(res.headers.get('access-control-allow-origin')).toBe(allowedOrigin)
      expect(res.headers.get('cache-control')).toBe('no-store')
      expect(await res.json()).toEqual({ error: 'crypto_not_configured' })
    }
    expect(doCalls).toBe(0)
    expect(outboundCalls).toBe(0)

    // In-flight IPN callbacks still settle when only NOWPAYMENTS_IPN_SECRET is configured.
    const settleOnlyEnv = createEnv({ NOWPAYMENTS_IPN_SECRET: ipnSecret })
    const ipnPayload = {
      payment_id: 900001,
      payment_status: 'finished',
      order_id: 'intent-inflight-1',
      price_amount: 10,
      price_currency: 'usd',
    }
    const ipnRes = await handleWorkerFetch(
      new Request(`${apiOrigin}/v1/donations/webhooks/nowpayments`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'x-nowpayments-sig': computeNowPaymentsSignature(ipnPayload, ipnSecret),
        },
        body: JSON.stringify(ipnPayload),
      }),
      settleOnlyEnv,
      { nowSec: () => 1_700_000_000 },
    )
    expect(ipnRes.status).toBe(200)
    expect(await ipnRes.json()).toEqual({ ok: true })
    expect(committedExternalId).toBe('900001')

    // Complete configuration succeeds end-to-end.
    const completeEnv = createEnv({ NOWPAYMENTS_API_KEY: apiKey, NOWPAYMENTS_IPN_SECRET: ipnSecret })
    expect(resolveCryptoCheckoutApiKey(completeEnv)).toBe(apiKey)
    const okRes = await postIntent(completeEnv)
    expect(okRes.status).toBe(201)
    expect(await okRes.json()).toEqual({
      intentId: 'intent-ready-1',
      invoiceUrl: 'https://nowpayments.io/payment/?iid=intent-ready-1',
    })
    expect(outboundCalls).toBe(1)
  })

  it('exposes Retry-After via CORS on rate-limited crypto intents without duplicating it in JSON error bodies', async () => {
    const allowedOrigin = 'https://windshare.cc'
    const apiOrigin = 'https://donate.windshare.cc'
    const namespace = {
      idFromName: (name: string) => ({ toString: () => name }),
      get: () => ({
        createCryptoIntent: async () => ({
          ok: false as const,
          code: 'rate_limited' as const,
          retryAfterSeconds: 42,
        }),
        consumeAdminLoginRateLimit: async () => ({
          allowed: false,
          retryAfterSeconds: 17,
        }),
      }),
    } as unknown as DurableObjectNamespace<DonationWallRpc>

    const adminSecret = randomBytes(16).toString('hex')
    const env: DonationsEnv = {
      ...createStubEnv(),
      DONATION_WALL: namespace,
      NOWPAYMENTS_API_KEY: 'test-nowpayments-api-key',
      NOWPAYMENTS_IPN_SECRET: 'test-nowpayments-ipn-secret',
      DONATE_ADMIN_PASSWORD: adminSecret,
    }

    const cryptoRes = await handleWorkerFetch(
      new Request(`${apiOrigin}/v1/donations/crypto-intents`, {
        method: 'POST',
        headers: { Origin: allowedOrigin, 'Content-Type': 'application/json' },
        body: JSON.stringify({ amountUsd: '10' }),
      }),
      env,
      { nowSec: () => 1_700_000_000 },
    )
    expect(cryptoRes.status).toBe(429)
    expect(cryptoRes.headers.get('access-control-allow-origin')).toBe(allowedOrigin)
    expect(cryptoRes.headers.get('retry-after')).toBe('42')
    expect(cryptoRes.headers.get('access-control-expose-headers')).toBe('Retry-After')
    expect(await cryptoRes.json()).toEqual({ error: 'rate_limited' })

    const adminRes = await handleWorkerFetch(
      new Request(`${apiOrigin}/v1/admin/login`, {
        method: 'POST',
        headers: { Origin: apiOrigin, 'Content-Type': 'application/json' },
        body: JSON.stringify({ password: adminSecret }),
      }),
      env,
      { nowSec: () => 1_700_000_000 },
    )
    expect(adminRes.status).toBe(429)
    expect(adminRes.headers.get('retry-after')).toBe('17')
    expect(await adminRes.json()).toEqual({ error: 'rate_limited' })
  })
})
