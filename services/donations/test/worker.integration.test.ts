import { execFileSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  Log,
  LogLevel,
  Miniflare,
  Response as MiniflareResponse,
  convertV4MiniflareOptions,
} from 'miniflare'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import type {
  AdminDonationsPage,
  DonationRecord,
  PublicWallResponse,
} from '../src/model'
import { computeNowPaymentsSignature } from '../src/verify'

const SERVICE_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const DONATE_ORIGIN = 'https://donate.windshare.cc'
const ALLOWED_ORIGIN = 'https://windshare.cc'

const SECRETS = Object.freeze({
  AFDIAN_USER_ID: '735de47ce57c11f1b2c95254001e7c00',
  AFDIAN_TOKEN: 'test-afdian-token',
  AFDIAN_WEBHOOK_SECRET: 'test-afdian-webhook-secret-entropy',
  KOFI_HANDLE: 'windshare',
  KOFI_VERIFICATION_TOKEN: 'test-kofi-verification-token',
  NOWPAYMENTS_API_KEY: 'test-nowpayments-api-key',
  NOWPAYMENTS_IPN_SECRET: 'test-nowpayments-ipn-secret',
  DONATE_ADMIN_PASSWORD: randomBytes(16).toString('hex'),
})

interface OutboundControl {
  afdianOrderStatus: 'ok' | 'transient_error' | 'unpaid'
  afdianSponsorMode: 'ok' | 'fail'
  afdianOrderCalls: number
  afdianSponsorCalls: number
  afdianSyncOrders: Array<{
    out_trade_no: string
    user_id: string
    total_amount: string
    status: number
    remark: string
    create_time?: number
  }>
  nowPaymentsInvoiceCalls: number
}

const outbound: OutboundControl = {
  afdianOrderStatus: 'ok',
  afdianSponsorMode: 'ok',
  afdianOrderCalls: 0,
  afdianSponsorCalls: 0,
  afdianSyncOrders: [],
  nowPaymentsInvoiceCalls: 0,
}

let bundleDir = ''
let mf: Miniflare

function buildWorkerBundle(): string {
  const outDir = mkdtempSync(path.join(tmpdir(), 'windshare-donations-mf-'))
  const wranglerBin = path.join(SERVICE_ROOT, 'node_modules', 'wrangler', 'bin', 'wrangler.js')
  execFileSync(
    process.execPath,
    [wranglerBin, 'deploy', '--dry-run', '--outdir', outDir],
    { cwd: SERVICE_ROOT, stdio: 'pipe' },
  )
  return outDir
}

function handleOutboundMock(request: { url: string; headers: { get(name: string): string | null }; text(): Promise<string> }): Promise<MiniflareResponse> {
  return (async () => {
    if (request.url === 'https://api.nowpayments.io/v1/invoice') {
      outbound.nowPaymentsInvoiceCalls += 1
      if (request.headers.get('x-api-key') !== SECRETS.NOWPAYMENTS_API_KEY) {
        return new MiniflareResponse(JSON.stringify({ message: 'unauthorized' }), { status: 401 })
      }
      const payload = JSON.parse(await request.text()) as { order_id: string }
      return new MiniflareResponse(
        JSON.stringify({
          id: 'inv-100',
          invoice_url: `https://nowpayments.io/payment/?iid=${payload.order_id}`,
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      )
    }

    if (request.url === 'https://afdian.com/api/open/query-order') {
      outbound.afdianOrderCalls += 1
      if (outbound.afdianOrderStatus === 'transient_error') {
        return new MiniflareResponse('upstream overloaded', { status: 503 })
      }
      const body = JSON.parse(await request.text()) as { params: string }
      const params = JSON.parse(body.params) as { out_trade_no?: string }
      if (params.out_trade_no !== undefined) {
        const list = outbound.afdianOrderStatus === 'unpaid'
          ? [{ out_trade_no: params.out_trade_no, user_id: 'u-1', total_amount: '10.00', status: 1, remark: 'unpaid', create_time: 1_760_000_200 }]
          : [{ out_trade_no: params.out_trade_no, user_id: 'u-1', total_amount: '15.00', status: 2, remark: '支持开源', create_time: 1_760_000_200 }]
        return new MiniflareResponse(JSON.stringify({ ec: 200, data: { list } }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      }
      return new MiniflareResponse(
        JSON.stringify({ ec: 200, data: { list: outbound.afdianSyncOrders } }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      )
    }

    if (request.url === 'https://afdian.com/api/open/query-sponsor') {
      outbound.afdianSponsorCalls += 1
      if (outbound.afdianSponsorMode === 'fail') {
        return new MiniflareResponse('timeout', { status: 504 })
      }
      return new MiniflareResponse(
        JSON.stringify({
          ec: 200,
          data: {
            list: [
              { user: { user_id: 'u-1', name: '爱发电用户' } },
              { user: { user_id: 'u-sync', name: '同步赞助者' } },
            ],
          },
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      )
    }

    return new MiniflareResponse('unmocked outbound URL', { status: 500 })
  })()
}

beforeAll(async () => {
  bundleDir = buildWorkerBundle()
  const workerPath = path.join(bundleDir, 'worker.js')
  const options = convertV4MiniflareOptions({
    log: new Log(LogLevel.ERROR),
    workers: [
      {
        name: 'donations',
        compatibilityDate: '2026-09-28',
        compatibilityFlags: ['nodejs_compat'],
        modulesRoot: bundleDir,
        modules: [{ type: 'ESModule', path: workerPath }],
        bindings: {
          ...SECRETS,
          ALLOWED_ORIGINS: `https://ws.ourworld.top,${ALLOWED_ORIGIN}`,
          DONATE_API_ORIGIN: DONATE_ORIGIN,
        },
        durableObjects: {
          DONATION_WALL: { className: 'DonationWall', useSQLite: true },
        },
        outboundService: request => handleOutboundMock(request),
      },
    ],
  })
  mf = new Miniflare(options)
}, 30_000)

afterAll(async () => {
  await mf?.dispose()
  if (bundleDir) {
    rmSync(bundleDir, { recursive: true, force: true })
  }
})

function postKofiWebhook(fields: Record<string, unknown>): Promise<Awaited<ReturnType<Miniflare['dispatchFetch']>>> {
  const body = new URLSearchParams({
    data: JSON.stringify({
      verification_token: SECRETS.KOFI_VERIFICATION_TOKEN,
      type: 'Donation',
      ...fields,
    }),
  }).toString()
  return mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/webhooks/kofi`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body,
  })
}

describe('windshare-donations Worker + SQLite DO HTTP integration', () => {
  it('handles crypto intent creation, NOWPayments IPN lifecycle, concurrent idempotency, and wall ETag caching', async () => {
    const initialWall = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/wall`)
    expect(initialWall.status).toBe(200)
    expect(initialWall.headers.get('access-control-allow-origin')).toBe('*')
    expect(initialWall.headers.get('cache-control')).toBe('public, max-age=15, stale-while-revalidate=60')
    const initialEtag = initialWall.headers.get('etag')
    expect(initialEtag).toBe('"wall-v0"')

    const cachedWall = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/wall`, {
      headers: { 'If-None-Match': initialEtag ?? '' },
    })
    expect(cachedWall.status).toBe(304)

    // Disallowed origin rejected on preflight and POST.
    const badPreflight = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/crypto-intents`, {
      method: 'OPTIONS',
      headers: { Origin: 'https://evil.example' },
    })
    expect(badPreflight.status).toBe(403)

    const goodPreflight = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/crypto-intents`, {
      method: 'OPTIONS',
      headers: { Origin: ALLOWED_ORIGIN },
    })
    expect(goodPreflight.status).toBe(204)
    expect(goodPreflight.headers.get('access-control-allow-origin')).toBe(ALLOWED_ORIGIN)

    // Oversized body (> 8 KiB) rejected with 413.
    const oversized = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/crypto-intents`, {
      method: 'POST',
      headers: { Origin: ALLOWED_ORIGIN, 'Content-Type': 'application/json' },
      body: JSON.stringify({ amountUsd: '10', message: 'a'.repeat(9000) }),
    })
    expect(oversized.status).toBe(413)

    // Valid crypto checkout intent calls NOWPayments /v1/invoice.
    const intentRes = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/crypto-intents`, {
      method: 'POST',
      headers: {
        Origin: ALLOWED_ORIGIN,
        'Content-Type': 'application/json; charset=utf-8',
        'CF-Connecting-IP': '203.0.113.10',
      },
      body: JSON.stringify({
        displayName: 'Crypto Fan',
        message: 'Keep shipping!',
        amountUsd: '10',
      }),
    })
    expect(intentRes.status).toBe(201)
    const intentData = await intentRes.json() as { intentId: string; invoiceUrl: string }
    expect(intentData.invoiceUrl).toContain(intentData.intentId)
    expect(outbound.nowPaymentsInvoiceCalls).toBe(1)

    // Intermediate partially_paid IPN is ignored (HTTP 200, not published).
    const partialPayload = {
      payment_id: 555001,
      payment_status: 'partially_paid',
      order_id: intentData.intentId,
      price_amount: 10,
      price_currency: 'usd',
    }
    const partialRes = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/webhooks/nowpayments`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'x-nowpayments-sig': computeNowPaymentsSignature(partialPayload, SECRETS.NOWPAYMENTS_IPN_SECRET),
      },
      body: JSON.stringify(partialPayload),
    })
    expect(partialRes.status).toBe(200)

    // Concurrent duplicate finished IPNs commit exactly once.
    const finishedPayload = {
      payment_id: 555002,
      payment_status: 'finished',
      order_id: intentData.intentId,
      price_amount: 10,
      price_currency: 'usd',
    }
    const finishedSig = computeNowPaymentsSignature(finishedPayload, SECRETS.NOWPAYMENTS_IPN_SECRET)
    const concurrentResults = await Promise.all([
      mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/webhooks/nowpayments`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'x-nowpayments-sig': finishedSig },
        body: JSON.stringify(finishedPayload),
      }),
      mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/webhooks/nowpayments`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'x-nowpayments-sig': finishedSig },
        body: JSON.stringify(finishedPayload),
      }),
    ])
    expect(concurrentResults.map(r => r.status)).toEqual([200, 200])

    // Missing/expired order_id still commits as anonymous instead of dropping real funds.
    const orphanPayload = {
      payment_id: 555003,
      payment_status: 'finished',
      order_id: 'expired-or-missing-intent',
      price_amount: 12.5,
      price_currency: 'usd',
    }
    const orphanRes = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/webhooks/nowpayments`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'x-nowpayments-sig': computeNowPaymentsSignature(orphanPayload, SECRETS.NOWPAYMENTS_IPN_SECRET),
      },
      body: JSON.stringify(orphanPayload),
    })
    expect(orphanRes.status).toBe(200)

    const updatedWallRes = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/wall`, {
      headers: { 'If-None-Match': initialEtag ?? '' },
    })
    expect(updatedWallRes.status).toBe(200)
    expect(updatedWallRes.headers.get('etag')).not.toBe(initialEtag)
    const updatedWall = await updatedWallRes.json() as PublicWallResponse
    expect(updatedWall.entries).toHaveLength(2)
    expect(updatedWall.entries.some(e => e.displayName === 'Crypto Fan' && e.message === 'Keep shipping!')).toBe(true)
    expect(updatedWall.entries.some(e => e.displayName === null && e.amountMinor === 1250)).toBe(true)
  })

  it('handles Afdian retries, sponsor fallback, dedup short-circuit, and Ko-fi privacy rules', async () => {
    // Wrong Afdian secret -> 403.
    const badSecret = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/webhooks/afdian/wrong-secret`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ec: 200, data: { order: { out_trade_no: 'AF-1' } } }),
    })
    expect(badSecret.status).toBe(403)

    // Transient query-order error -> 502 so Afdian retries (not ec: 200).
    outbound.afdianOrderStatus = 'transient_error'
    const transient = await mf.dispatchFetch(
      `${DONATE_ORIGIN}/v1/donations/webhooks/afdian/${SECRETS.AFDIAN_WEBHOOK_SECRET}`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ec: 200, data: { order: { out_trade_no: 'AF-1' } } }),
      },
    )
    expect(transient.status).toBe(502)

    // Verified paid order succeeds even when query-sponsor fails (degrades to anonymous).
    outbound.afdianOrderStatus = 'ok'
    outbound.afdianSponsorMode = 'fail'
    const callsBeforeCommit = outbound.afdianOrderCalls
    const afdianOk = await mf.dispatchFetch(
      `${DONATE_ORIGIN}/v1/donations/webhooks/afdian/${SECRETS.AFDIAN_WEBHOOK_SECRET}`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ec: 200, data: { order: { out_trade_no: 'AF-1' } } }),
      },
    )
    expect(afdianOk.status).toBe(200)
    expect(await afdianOk.json()).toEqual({ ec: 200, em: '' })
    expect(outbound.afdianOrderCalls).toBe(callsBeforeCommit + 1)

    // Replay of already-committed order short-circuits before calling query-order again.
    const afdianReplay = await mf.dispatchFetch(
      `${DONATE_ORIGIN}/v1/donations/webhooks/afdian/${SECRETS.AFDIAN_WEBHOOK_SECRET}`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ec: 200, data: { order: { out_trade_no: 'AF-1' } } }),
      },
    )
    expect(afdianReplay.status).toBe(200)
    expect(await afdianReplay.json()).toEqual({ ec: 200, em: '' })
    expect(outbound.afdianOrderCalls).toBe(callsBeforeCommit + 1)

    // Ko-fi private donation stays off public wall and strips PII.
    const kofiRes = await postKofiWebhook({
      kofi_transaction_id: 'KOFI-PRIV-1',
      amount: '25.00',
      currency: 'USD',
      is_public: false,
      from_name: 'Hidden Person',
      message: 'Top secret message',
      email: 'hidden@example.com',
    })
    expect(kofiRes.status).toBe(200)

    // Ko-fi zero-decimal currency with ".00" string amount commits with exact minor units.
    const kofiJpyRes = await postKofiWebhook({
      kofi_transaction_id: 'KOFI-JPY-1',
      amount: '500.00',
      currency: 'JPY',
      is_public: true,
      from_name: 'Taro',
      message: '応援しています',
    })
    expect(kofiJpyRes.status).toBe(200)

    const wall = await (await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/wall`)).json() as PublicWallResponse
    expect(wall.entries.some(e => e.provider === 'afdian' && e.message === '支持开源' && e.displayName === null)).toBe(true)
    expect(wall.entries.some(e => e.provider === 'kofi')).toBe(false)
  })

  it('enforces admin auth, CSRF, sync-afdian, moderation transitions, config updates, and logout revocation', async () => {
    const adminHtml = await mf.dispatchFetch(`${DONATE_ORIGIN}/admin`)
    expect(adminHtml.status).toBe(200)
    expect(adminHtml.headers.get('cache-control')).toBe('no-store')
    expect(adminHtml.headers.get('content-security-policy')).toContain("script-src 'self'")

    // Unauthenticated API access rejected.
    expect((await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/admin/donations`)).status).toBe(401)

    // Cross-origin login rejected.
    const crossOriginLogin = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/admin/login`, {
      method: 'POST',
      headers: { Origin: 'https://evil.example', 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: SECRETS.DONATE_ADMIN_PASSWORD }),
    })
    expect(crossOriginLogin.status).toBe(403)

    // Wrong password rejected.
    const badLogin = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/admin/login`, {
      method: 'POST',
      headers: {
        Origin: DONATE_ORIGIN,
        'Content-Type': 'application/json',
        'CF-Connecting-IP': '198.51.100.5',
      },
      body: JSON.stringify({ password: `${SECRETS.DONATE_ADMIN_PASSWORD}-mismatch` }),
    })
    expect(badLogin.status).toBe(401)

    // Successful login issues a __Host- HttpOnly Secure SameSite=Strict cookie + CSRF token.
    const loginRes = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/admin/login`, {
      method: 'POST',
      headers: {
        Origin: DONATE_ORIGIN,
        'Content-Type': 'application/json',
        'CF-Connecting-IP': '198.51.100.5',
      },
      body: JSON.stringify({ password: SECRETS.DONATE_ADMIN_PASSWORD }),
    })
    expect(loginRes.status).toBe(200)
    const setCookie = loginRes.headers.get('set-cookie') ?? ''
    expect(setCookie).toMatch(/^__Host-windshare_donate_admin=[^;]+; HttpOnly; Secure; SameSite=Strict; Path=\/; Max-Age=\d+$/)
    const cookiePair = setCookie.split(';', 1)[0] ?? ''
    const loginData = await loginRes.json() as { authenticated: boolean; csrfToken: string }
    expect(loginData.authenticated).toBe(true)

    // Session endpoint restores CSRF after page reload.
    const sessionRes = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/admin/session`, {
      headers: { Cookie: cookiePair },
    })
    expect(sessionRes.status).toBe(200)
    const sessionData = await sessionRes.json() as { csrfToken: string }
    expect(sessionData.csrfToken).toBe(loginData.csrfToken)

    // Mutating request without CSRF token rejected with 403.
    const noCsrf = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/admin/donations/sync-afdian`, {
      method: 'POST',
      headers: { Origin: DONATE_ORIGIN, Cookie: cookiePair },
    })
    expect(noCsrf.status).toBe(403)

    // One-click Afdian sync imports new paid orders and skips already-existing ones.
    outbound.afdianSponsorMode = 'ok'
    outbound.afdianSyncOrders = [
      { out_trade_no: 'AF-1', user_id: 'u-1', total_amount: '15.00', status: 2, remark: 'duplicate', create_time: 1_760_000_200 },
      { out_trade_no: 'AF-SYNC-2', user_id: 'u-sync', total_amount: '20.00', status: 2, remark: '补录感谢', create_time: 1_760_000_100 },
      { out_trade_no: 'AF-UNPAID', user_id: 'u-sync', total_amount: '20.00', status: 1, remark: 'pending', create_time: 1_760_000_050 },
    ]
    const sponsorCallsBeforeSync = outbound.afdianSponsorCalls
    const syncRes = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/admin/donations/sync-afdian`, {
      method: 'POST',
      headers: {
        Origin: DONATE_ORIGIN,
        Cookie: cookiePair,
        'X-CSRF-Token': loginData.csrfToken,
      },
    })
    expect(syncRes.status).toBe(200)
    expect(await syncRes.json()).toEqual({ fetched: 3, paid: 2, committed: 1, deduplicated: 1, deferred: 0 })
    expect(outbound.afdianSponsorCalls).toBe(sponsorCallsBeforeSync + 1)

    // Re-running sync with zero new orders short-circuits via DO batch dedup with zero sponsor lookups.
    const replaySyncRes = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/admin/donations/sync-afdian`, {
      method: 'POST',
      headers: {
        Origin: DONATE_ORIGIN,
        Cookie: cookiePair,
        'X-CSRF-Token': loginData.csrfToken,
      },
    })
    expect(replaySyncRes.status).toBe(200)
    expect(await replaySyncRes.json()).toEqual({ fetched: 3, paid: 2, committed: 0, deduplicated: 2, deferred: 0 })
    expect(outbound.afdianSponsorCalls).toBe(sponsorCallsBeforeSync + 1)

    // Fetch admin list and verify private_donor cannot be approved (409), while published can hide/restore.
    const listRes = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/admin/donations`, {
      headers: { Cookie: cookiePair },
    })
    expect(listRes.status).toBe(200)
    const listData = await listRes.json() as AdminDonationsPage
    const privateItem = listData.items.find((item: DonationRecord) => item.externalId === 'KOFI-PRIV-1')
    const kofiJpyItem = listData.items.find((item: DonationRecord) => item.externalId === 'KOFI-JPY-1')
    const syncedItem = listData.items.find((item: DonationRecord) => item.externalId === 'AF-SYNC-2')
    expect(privateItem).toBeDefined()
    expect(kofiJpyItem).toMatchObject({
      amountMinor: 500,
      currency: 'JPY',
      minorDigits: 0,
      displayName: 'Taro',
      visibility: { kind: 'pending_review', reason: 'unsupported_currency' },
    })
    const webhookAfdianItem = listData.items.find((item: DonationRecord) => item.externalId === 'AF-1')
    expect(syncedItem).toMatchObject({ displayName: '同步赞助者', donatedAt: 1_760_000_100 })
    expect(webhookAfdianItem?.donatedAt).toBe(1_760_000_200)

    const privateApprove = await mf.dispatchFetch(
      `${DONATE_ORIGIN}/v1/admin/donations/${privateItem?.id}/visibility`,
      {
        method: 'POST',
        headers: {
          Origin: DONATE_ORIGIN,
          Cookie: cookiePair,
          'X-CSRF-Token': loginData.csrfToken,
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({ action: 'approve' }),
      },
    )
    expect(privateApprove.status).toBe(409)

    const hideRes = await mf.dispatchFetch(
      `${DONATE_ORIGIN}/v1/admin/donations/${syncedItem?.id}/visibility`,
      {
        method: 'POST',
        headers: {
          Origin: DONATE_ORIGIN,
          Cookie: cookiePair,
          'X-CSRF-Token': loginData.csrfToken,
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({ action: 'hide', reason: 'temporary test' }),
      },
    )
    expect(hideRes.status).toBe(200)
    const wallAfterHide = await (await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/wall`)).json() as PublicWallResponse
    expect(wallAfterHide.entries.some(e => e.id === syncedItem?.id)).toBe(false)

    const restoreRes = await mf.dispatchFetch(
      `${DONATE_ORIGIN}/v1/admin/donations/${syncedItem?.id}/visibility`,
      {
        method: 'POST',
        headers: {
          Origin: DONATE_ORIGIN,
          Cookie: cookiePair,
          'X-CSRF-Token': loginData.csrfToken,
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({ action: 'restore' }),
      },
    )
    expect(restoreRes.status).toBe(200)
    const wallAfterRestore = await (await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/wall`)).json() as PublicWallResponse
    expect(wallAfterRestore.entries.map(e => e.id).slice(2)).toEqual([webhookAfdianItem?.id, syncedItem?.id])

    // Pause Crypto checkout via config patch and verify 503 enforcement.
    const patchRes = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/admin/donations/config`, {
      method: 'PATCH',
      headers: {
        Origin: DONATE_ORIGIN,
        Cookie: cookiePair,
        'X-CSRF-Token': loginData.csrfToken,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ cryptoCheckoutPaused: true, moderationEnabled: true }),
    })
    expect(patchRes.status).toBe(200)

    const pausedIntent = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/donations/crypto-intents`, {
      method: 'POST',
      headers: { Origin: ALLOWED_ORIGIN, 'Content-Type': 'application/json' },
      body: JSON.stringify({ amountUsd: '10' }),
    })
    expect(pausedIntent.status).toBe(503)

    // Logout immediately revokes session in SQLite.
    const logoutRes = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/admin/logout`, {
      method: 'POST',
      headers: {
        Origin: DONATE_ORIGIN,
        Cookie: cookiePair,
        'X-CSRF-Token': loginData.csrfToken,
      },
    })
    expect(logoutRes.status).toBe(200)
    expect(logoutRes.headers.get('set-cookie')).toBe('__Host-windshare_donate_admin=; HttpOnly; Secure; SameSite=Strict; Path=/; Max-Age=0')

    const afterLogout = await mf.dispatchFetch(`${DONATE_ORIGIN}/v1/admin/session`, {
      headers: { Cookie: cookiePair },
    })
    expect(afterLogout.status).toBe(401)
  })
})
