import { randomBytes } from 'node:crypto'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { DonationsEnv } from '../src/env'
import type { CommitDonationInput } from '../src/model'
import { handleWorkerFetch } from '../src/router'
import { computeNowPaymentsSignature } from '../src/verify'
import {
  createInMemoryWallStore,
  createWallNamespaceFromStore,
  type AsyncWallStub,
} from './wall-harness'

const API_ORIGIN = 'https://donate.windshare.cc'
const NOW_SEC = 1_700_000_000
const AFDIAN_SECRET = 'test-afdian-webhook-secret'
const KOFI_TOKEN = 'test-kofi-verification-token'
const NOWPAYMENTS_IPN_SECRET = 'test-nowpayments-ipn-secret'

interface LogRecord {
  readonly level: 'info' | 'warn' | 'error'
  readonly payload: Record<string, unknown>
}

function captureStructuredLogs(): LogRecord[] {
  const logs: LogRecord[] = []
  const push = (level: 'info' | 'warn' | 'error') => (line: unknown) => {
    if (typeof line !== 'string') return
    logs.push({ level, payload: JSON.parse(line) as Record<string, unknown> })
  }
  vi.spyOn(console, 'log').mockImplementation(push('info'))
  vi.spyOn(console, 'warn').mockImplementation(push('warn'))
  vi.spyOn(console, 'error').mockImplementation(push('error'))
  return logs
}

function createObservabilityHarness(overrides: Partial<AsyncWallStub> = {}) {
  const store = createInMemoryWallStore()
  const adminPassword = randomBytes(16).toString('hex')
  const commits: CommitDonationInput[] = []

  const env: DonationsEnv = {
    DONATION_WALL: createWallNamespaceFromStore(store, {
      overrides: {
        commitDonation: async (input, nowSec) => {
          commits.push(input)
          return store.commitDonation(input, nowSec)
        },
        ...overrides,
      },
    }),
    AFDIAN_USER_ID: 'creator',
    AFDIAN_TOKEN: 'afdian-token',
    AFDIAN_WEBHOOK_SECRET: AFDIAN_SECRET,
    KOFI_VERIFICATION_TOKEN: KOFI_TOKEN,
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

  return { store, env, commits, loginAdmin }
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

  it('logs NOWPayments intent fallback when expired and propagates DO RPC outages for provider retry', async () => {
    const logs = captureStructuredLogs()
    const { env, commits } = createObservabilityHarness({
      getCryptoIntent: async (intentId) => {
        if (intentId === 'intent-do-err') {
          throw new Error('storage unavailable')
        }
        return null
      },
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

    await expect(sendIpn({
      payment_id: 7002,
      payment_status: 'finished',
      order_id: 'intent-do-err',
      price_amount: 12,
      price_currency: 'usd',
    })).rejects.toThrow('storage unavailable')

    expect((await sendIpn({
      payment_id: 7003,
      payment_status: 'finished',
      price_amount: 15,
      price_currency: 'usd',
    })).status).toBe(200)

    expect(commits).toHaveLength(2)
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
    ])
  })

  it('logs applied fields on successful admin config patches and logs storage errors on DO RPC failure', async () => {
    const logs = captureStructuredLogs()
    let failUpstream = false
    const harness = createObservabilityHarness({
      patchAdminConfig: async (patch) => {
        if (failUpstream) {
          throw new Error('storage_error')
        }
        return harness.store.patchConfig(patch)
      },
    })
    const auth = await harness.loginAdmin()

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
      harness.env,
      { nowSec: () => NOW_SEC },
    )

    expect((await patchConfig({ moderationEnabled: true, minPublicUsdMinor: 1000 })).status).toBe(200)
    expect((await patchConfig({ moderationEnabled: 'invalid' })).status).toBe(400)
    failUpstream = true
    expect((await patchConfig({ cryptoCheckoutPaused: true })).status).toBe(503)

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
    expect(logs.some(entry => entry.level === 'error' && entry.payload.event === 'donation.admin_storage_error')).toBe(true)
  })

  it('verifies Ko-fi verification_token before validating business fields and logs distinct rejection reasons', async () => {
    const logs = captureStructuredLogs()
    const { env, commits } = createObservabilityHarness()

    const sendKofiRaw = (rawData: string) => handleWorkerFetch(
      new Request(`${API_ORIGIN}/v1/donations/webhooks/kofi`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ data: rawData }).toString(),
      }),
      env,
      { nowSec: () => NOW_SEC },
    )

    // Unauthenticated JSON objects without business fields must return 403 and log invalid_signature.
    for (const unauthBody of [
      JSON.stringify({}),
      JSON.stringify({ verification_token: 'wrong-token' }),
      JSON.stringify({ verification_token: 12345 }),
    ]) {
      const res = await sendKofiRaw(unauthBody)
      expect(res.status).toBe(403)
      expect(await res.json()).toEqual({ error: 'forbidden' })
    }

    // Malformed envelope JSON cannot be authenticated and remains 400 invalid_payload.
    const badJsonRes = await sendKofiRaw('{not-json')
    expect(badJsonRes.status).toBe(400)
    expect(await badJsonRes.json()).toEqual({ error: 'invalid_payload' })

    // Authenticated payloads missing required business fields return 400 invalid_payload.
    const missingFieldsRes = await sendKofiRaw(JSON.stringify({ verification_token: KOFI_TOKEN }))
    expect(missingFieldsRes.status).toBe(400)
    expect(await missingFieldsRes.json()).toEqual({ error: 'invalid_payload' })

    expect(commits).toHaveLength(0)
    const rejectedReasons = logs
      .filter(entry => entry.payload.event === 'donation.webhook_rejected' && entry.payload.provider === 'kofi')
      .map(entry => entry.payload.reason)
    expect(rejectedReasons).toEqual([
      'invalid_signature',
      'invalid_signature',
      'invalid_signature',
      'invalid_payload',
      'invalid_payload',
    ])
  })

  it('logs admin_secret_rejected when DONATE_ADMIN_PASSWORD is below the minimum length', async () => {
    const logs = captureStructuredLogs()
    const { env } = createObservabilityHarness()

    const shortSecret = randomBytes(8).toString('hex')
    const shortEnv: DonationsEnv = { ...env, DONATE_ADMIN_PASSWORD: shortSecret }
    const shortRes = await handleWorkerFetch(
      new Request(`${API_ORIGIN}/v1/admin/session`),
      shortEnv,
      { nowSec: () => NOW_SEC },
    )
    expect(shortRes.status).toBe(503)
    expect(await shortRes.json()).toEqual({ error: 'admin_not_configured' })

    // Missing secret is intentional disablement and should not emit secret-rejection warnings.
    const missingEnv: DonationsEnv = { ...env, DONATE_ADMIN_PASSWORD: '' }
    const missingRes = await handleWorkerFetch(
      new Request(`${API_ORIGIN}/v1/admin/session`),
      missingEnv,
      { nowSec: () => NOW_SEC },
    )
    expect(missingRes.status).toBe(503)

    const secretWarnings = logs.filter(entry => entry.payload.event === 'donation.admin_secret_rejected')
    expect(secretWarnings).toEqual([
      {
        level: 'warn',
        payload: {
          event: 'donation.admin_secret_rejected',
          reason: 'too_short',
          min_length: 24,
        },
      },
    ])
  })
})
