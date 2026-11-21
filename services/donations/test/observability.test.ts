import { randomBytes } from 'node:crypto'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { DonationsEnv } from '../src/env'
import { defaultDonationConfig, type CommitDonationInput } from '../src/model'
import { handleWorkerFetch } from '../src/router'
import { computeNowPaymentsSignature } from '../src/verify'

const API_ORIGIN = 'https://donate.windshare.cc'
const NOW_SEC = 1_700_000_000
const AFDIAN_SECRET = 'test-afdian-webhook-secret'
const NOWPAYMENTS_IPN_SECRET = 'test-nowpayments-ipn-secret'

interface LogRecord {
  readonly level: 'info' | 'warn'
  readonly payload: Record<string, unknown>
}

interface HarnessOptions {
  readonly intentLookupResponse?: (intentId: string) => Response
  readonly configPatchResponse?: () => Response
}

function captureStructuredLogs(): LogRecord[] {
  const logs: LogRecord[] = []
  const push = (level: 'info' | 'warn') => (line: unknown) => {
    if (typeof line !== 'string') return
    logs.push({ level, payload: JSON.parse(line) as Record<string, unknown> })
  }
  vi.spyOn(console, 'log').mockImplementation(push('info'))
  vi.spyOn(console, 'warn').mockImplementation(push('warn'))
  return logs
}

function createObservabilityHarness(options: HarnessOptions = {}) {
  const adminPassword = randomBytes(16).toString('hex')
  let activeSessionHash: string | null = null
  let activeCsrfToken: string | null = null
  const commits: CommitDonationInput[] = []

  const dispatchInternal = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const req = input instanceof Request ? input : new Request(input, init)
    const { pathname } = new URL(req.url)
    if (pathname === '/internal/donations/exists') {
      return Response.json({ exists: false }, { status: 200 })
    }
    if (pathname.startsWith('/internal/crypto-intents/')) {
      const intentId = decodeURIComponent(pathname.slice('/internal/crypto-intents/'.length))
      return options.intentLookupResponse?.(intentId) ?? Response.json({ intent: null }, { status: 200 })
    }
    if (pathname === '/internal/donations/commit') {
      const body = await req.json() as { input: CommitDonationInput }
      commits.push(body.input)
      return Response.json({
        record: {
          id: `don-${commits.length}`,
          provider: body.input.provider,
          externalId: body.input.externalId,
          displayName: body.input.displayName,
          amountMinor: body.input.amountMinor,
          currency: body.input.currency,
          minorDigits: body.input.minorDigits,
          message: body.input.message,
          visibility: { kind: 'published' },
          donatedAt: NOW_SEC,
        },
        deduplicated: false,
      })
    }
    if (pathname === '/internal/admin/rate-limit') {
      return Response.json({ allowed: true, retryAfterSeconds: 0 }, { status: 200 })
    }
    if (pathname === '/internal/admin/sessions') {
      const body = await req.json() as { sessionHash: string; csrfToken: string }
      activeSessionHash = body.sessionHash
      activeCsrfToken = body.csrfToken
      return Response.json({ csrfToken: body.csrfToken, expiresAt: NOW_SEC + 3600 }, { status: 201 })
    }
    if (pathname === '/internal/admin/sessions/verify') {
      const body = await req.json() as { sessionHash: string }
      if (body.sessionHash === activeSessionHash && activeCsrfToken !== null) {
        return Response.json({ valid: true, csrfToken: activeCsrfToken, expiresAt: NOW_SEC + 3600 }, { status: 200 })
      }
      return Response.json({ valid: false }, { status: 401 })
    }
    if (pathname === '/internal/admin/config') {
      if (options.configPatchResponse !== undefined) return options.configPatchResponse()
      return Response.json({ config: defaultDonationConfig() }, { status: 200 })
    }
    return Response.json({ error: 'not_found' }, { status: 404 })
  }

  const env: DonationsEnv = {
    DONATION_WALL: {
      idFromName: (name: string) => ({ toString: () => name }),
      get: () => ({ fetch: dispatchInternal }),
    } as unknown as DurableObjectNamespace,
    AFDIAN_USER_ID: 'creator',
    AFDIAN_TOKEN: 'afdian-token',
    AFDIAN_WEBHOOK_SECRET: AFDIAN_SECRET,
    KOFI_HANDLE: 'windshare',
    ALLOWED_ORIGINS: 'https://windshare.cc',
    DONATE_API_ORIGIN: API_ORIGIN,
    NOWPAYMENTS_IPN_SECRET,
    DONATE_ADMIN_PASSWORD: adminPassword,
  }

  const loginAdmin = async (): Promise<{ cookie: string; csrf: string }> => {
    const res = await handleWorkerFetch(
      new Request(`${API_ORIGIN}/v1/admin/login`, {
        method: 'POST',
        headers: { Origin: API_ORIGIN, 'Content-Type': 'application/json' },
        body: JSON.stringify({ password: adminPassword }),
      }),
      env,
      { nowSec: () => NOW_SEC },
    )
    const cookie = (res.headers.get('set-cookie') ?? '').split(';', 1)[0] ?? ''
    const data = await res.json() as { csrfToken: string }
    return { cookie, csrf: data.csrfToken }
  }

  return { env, commits, loginAdmin }
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('structured observability logs on failure, fallback, and admin config paths', () => {
  it('logs distinct reasons when Afdian webhooks are skipped or fail upstream verification', async () => {
    const logs = captureStructuredLogs()
    const { env, commits } = createObservabilityHarness()

    const sendAfdian = (outTradeNo: string, fetchFn: () => Promise<Response>) => handleWorkerFetch(
      new Request(`${API_ORIGIN}/v1/donations/webhooks/afdian/${AFDIAN_SECRET}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ec: 200, data: { order: { out_trade_no: outTradeNo } } }),
      }),
      env,
      { nowSec: () => NOW_SEC, fetchFn },
    )

    const missingRes = await sendAfdian('AF-MISSING', async () => Response.json({ ec: 200, data: { list: [] } }))
    expect(missingRes.status).toBe(200)
    expect(await missingRes.json()).toEqual({ ec: 200, em: '' })

    const unpaidRes = await sendAfdian('AF-UNPAID', async () => Response.json({
      ec: 200,
      data: {
        list: [{ out_trade_no: 'AF-UNPAID', user_id: 'u-1', total_amount: '10.00', status: 1, remark: '' }],
      },
    }))
    expect(unpaidRes.status).toBe(200)
    expect(await unpaidRes.json()).toEqual({ ec: 200, em: '' })

    const errRes = await sendAfdian('AF-ERR', async () => new Response('overloaded', { status: 503 }))
    expect(errRes.status).toBe(502)
    expect(commits).toHaveLength(0)

    expect(logs).toContainEqual({
      level: 'warn',
      payload: {
        event: 'donation.afdian_order_skipped',
        out_trade_no: 'AF-MISSING',
        reason: 'order_not_found',
      },
    })
    expect(logs).toContainEqual({
      level: 'info',
      payload: {
        event: 'donation.afdian_order_skipped',
        out_trade_no: 'AF-UNPAID',
        reason: 'order_not_paid',
        order_status: 1,
      },
    })
    expect(logs).toContainEqual({
      level: 'warn',
      payload: {
        event: 'donation.webhook_rejected',
        provider: 'afdian',
        reason: 'afdian_verify_failed',
        external_id: 'AF-ERR',
        upstream_reason: 'http_503',
      },
    })
  })

  it('logs NOWPayments intent fallback only when order_id is present and distinguishes DO failures', async () => {
    const logs = captureStructuredLogs()
    const { env, commits } = createObservabilityHarness({
      intentLookupResponse: intentId => (
        intentId === 'intent-do-err'
          ? Response.json({ error: 'internal' }, { status: 503 })
          : Response.json({ intent: null }, { status: 200 })
      ),
    })

    const sendIpn = (payload: Record<string, unknown>) => handleWorkerFetch(
      new Request(`${API_ORIGIN}/v1/donations/webhooks/nowpayments`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'x-nowpayments-sig': computeNowPaymentsSignature(payload, NOWPAYMENTS_IPN_SECRET),
        },
        body: JSON.stringify(payload),
      }),
      env,
      { nowSec: () => NOW_SEC },
    )

    expect((await sendIpn({
      payment_id: 7001,
      payment_status: 'finished',
      order_id: 'intent-expired',
      price_amount: 10,
      price_currency: 'usd',
    })).status).toBe(200)

    expect((await sendIpn({
      payment_id: 7002,
      payment_status: 'finished',
      order_id: 'intent-do-err',
      price_amount: 12,
      price_currency: 'usd',
    })).status).toBe(200)

    expect((await sendIpn({
      payment_id: 7003,
      payment_status: 'finished',
      price_amount: 15,
      price_currency: 'usd',
    })).status).toBe(200)

    expect(commits).toHaveLength(3)
    expect(commits.every(item => item.displayName === null && item.message === null)).toBe(true)

    const fallbackLogs = logs.filter(entry => entry.payload.event === 'donation.nowpayments_intent_fallback')
    expect(fallbackLogs).toEqual([
      {
        level: 'warn',
        payload: {
          event: 'donation.nowpayments_intent_fallback',
          payment_id: '7001',
          order_id: 'intent-expired',
          reason: 'not_found_or_expired',
        },
      },
      {
        level: 'warn',
        payload: {
          event: 'donation.nowpayments_intent_fallback',
          payment_id: '7002',
          order_id: 'intent-do-err',
          reason: 'lookup_failed',
          upstream_status: 503,
        },
      },
    ])
  })

  it('logs applied fields on successful admin config patches and stays silent on failed updates', async () => {
    const logs = captureStructuredLogs()
    let failUpstream = false
    const { env, loginAdmin } = createObservabilityHarness({
      configPatchResponse: () => (
        failUpstream
          ? Response.json({ error: 'storage_error' }, { status: 500 })
          : Response.json({ config: { ...defaultDonationConfig(), moderationEnabled: true, minPublicUsdMinor: 1000 } })
      ),
    })
    const auth = await loginAdmin()

    const patchConfig = (body: Record<string, unknown>) => handleWorkerFetch(
      new Request(`${API_ORIGIN}/v1/admin/donations/config`, {
        method: 'PATCH',
        headers: {
          Origin: API_ORIGIN,
          Cookie: auth.cookie,
          'X-CSRF-Token': auth.csrf,
          'Content-Type': 'application/json',
        },
        body: JSON.stringify(body),
      }),
      env,
      { nowSec: () => NOW_SEC },
    )

    expect((await patchConfig({ moderationEnabled: true, minPublicUsdMinor: 1000 })).status).toBe(200)
    expect((await patchConfig({ moderationEnabled: 'invalid' })).status).toBe(400)
    failUpstream = true
    expect((await patchConfig({ cryptoCheckoutPaused: true })).status).toBe(500)

    const configLogs = logs.filter(entry => entry.payload.event === 'donation.admin_config_updated')
    expect(configLogs).toEqual([
      {
        level: 'info',
        payload: {
          event: 'donation.admin_config_updated',
          min_public_usd_minor: 1000,
          moderation_enabled: true,
        },
      },
    ])
  })
})
