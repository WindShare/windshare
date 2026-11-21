import { randomBytes } from 'node:crypto'
import { describe, expect, it } from 'vitest'
import type { DonationsEnv } from '../src/env'
import {
  MIN_PUBLIC_CNY_MINOR,
  MIN_PUBLIC_USD_MINOR,
  PUBLIC_WALL_LIMIT,
  applyVisibilityCommand,
  clampDonatedAt,
  defaultDonationConfig,
  type DonationRecord,
  type PublicWallResponse,
  type WallVisibility,
} from '../src/model'
import {
  createNowPaymentsInvoice,
  isValidNowPaymentsInvoiceUrl,
  lookupAfdianSponsorName,
  queryAfdianOrders,
  type FetchLike,
} from '../src/providers'
import { handleWorkerFetch } from '../src/router'
import {
  containsContactPii,
  containsExternalUrl,
  determineInitialVisibility,
  formatUsdMinorForInvoice,
  parseCryptoUsdAmount,
  parseFixedPointAmount,
  sanitizePlainText,
} from '../src/sanitize'
import {
  buildAfdianSignedBody,
  computeNowPaymentsSignature,
  isSupportedKofiType,
  md5Hex,
  parseAfdianOrderQueryResponse,
  parseAfdianSponsorName,
  parseAfdianSponsorPage,
  parseAfdianWebhookPayload,
  parseKofiFormDataObject,
  parseKofiWebhookPayload,
  parseNowPaymentsIpnPayload,
  parseUnixTimestampSeconds,
  sortJsonKeysRecursively,
  verifyKofiWebhookToken,
  verifyNowPaymentsSignature,
} from '../src/verify'
import { matchesIfNoneMatch } from '../src/wall-store'
import {
  createInMemoryWallStore,
  createInMemoryWallStoreWithDb,
  createWallNamespaceFromStore,
} from './wall-harness'

describe('sanitization, fixed-point money, and visibility classification', () => {
  it('parses decimal strings, JSON numbers, scientific notation, and unknown currencies safely', () => {
    expect(parseFixedPointAmount(1e-2, 'USD')).toEqual({
      amountMinor: 1,
      currency: 'USD',
      minorDigits: 2,
      knownCurrency: true,
    })
    expect(parseFixedPointAmount('15.5', 'eur')).toEqual({
      amountMinor: 1550,
      currency: 'EUR',
      minorDigits: 2,
      knownCurrency: true,
    })
    expect(parseFixedPointAmount('12.3456', 'AUD')).toEqual({
      amountMinor: 123456,
      currency: 'AUD',
      minorDigits: 4,
      knownCurrency: false,
    })
    expect(parseFixedPointAmount('500.00', 'JPY')).toEqual({
      amountMinor: 500,
      currency: 'JPY',
      minorDigits: 0,
      knownCurrency: true,
    })
    expect(parseFixedPointAmount('10.000', 'USD')).toEqual({
      amountMinor: 1000,
      currency: 'USD',
      minorDigits: 2,
      knownCurrency: true,
    })
    expect(parseFixedPointAmount('10.500', 'USD')).toEqual({
      amountMinor: 1050,
      currency: 'USD',
      minorDigits: 2,
      knownCurrency: true,
    })
    expect(parseFixedPointAmount('10.50', 'CAD')).toEqual({
      amountMinor: 1050,
      currency: 'CAD',
      minorDigits: 2,
      knownCurrency: false,
    })
    expect(parseFixedPointAmount('12.34500', 'CAD')).toEqual({
      amountMinor: 12345,
      currency: 'CAD',
      minorDigits: 3,
      knownCurrency: false,
    })
    expect(parseFixedPointAmount('10.00000', 'CAD')).toEqual({
      amountMinor: 10,
      currency: 'CAD',
      minorDigits: 0,
      knownCurrency: false,
    })
    expect(parseFixedPointAmount('500.50', 'JPY')).toBeNull()
    expect(parseFixedPointAmount('10.0010', 'USD')).toBeNull()
    expect(parseFixedPointAmount('12.34567', 'AUD')).toBeNull()
    expect(parseFixedPointAmount('12.345670', 'AUD')).toBeNull()
    expect(parseFixedPointAmount('500.', 'JPY')).toBeNull()
    expect(parseFixedPointAmount('10.', 'USD')).toBeNull()
    expect(parseFixedPointAmount(-5, 'USD')).toBeNull()
    expect(parseFixedPointAmount(Number.NaN, 'USD')).toBeNull()
    expect(parseCryptoUsdAmount('5')).toBe(500)
    expect(parseCryptoUsdAmount('4.99')).toBeNull()
    expect(parseCryptoUsdAmount('100000.00')).toBe(10_000_000)
    expect(parseCryptoUsdAmount('100000.01')).toBeNull()
    expect(formatUsdMinorForInvoice(1050)).toBe('10.50')
  })

  it('detects external URLs and contact PII without flagging normal blessings', () => {
    expect(containsExternalUrl('Check https://spam.example now')).toBe(true)
    expect(containsExternalUrl('Visit www.casino.example')).toBe(true)
    expect(containsExternalUrl('Join t.me/promo_channel')).toBe(true)
    expect(containsExternalUrl('Thanks for building WindShare v1.0!')).toBe(false)

    expect(containsContactPii('Contact me at alice@example.com')).toBe(true)
    expect(containsContactPii('我的手机 13812345678 请联系')).toBe(true)
    expect(containsContactPii('Phone 138-1234-5678')).toBe(true)
    expect(containsContactPii('Order 20261121138123456789')).toBe(false)
    expect(containsContactPii('Keep up the great work!')).toBe(false)
  })

  it('classifies initial visibility with strict precedence', () => {
    const config = defaultDonationConfig()

    expect(determineInitialVisibility({
      amountMinor: 5000,
      currency: 'USD',
      displayName: 'Alice',
      message: 'Secret note',
      isPublic: false,
      config,
    })).toEqual({ kind: 'private_donor' })

    expect(determineInitialVisibility({
      amountMinor: 2000,
      currency: 'CAD',
      displayName: 'Bob',
      message: 'Thanks',
      isPublic: true,
      config,
    })).toEqual({ kind: 'pending_review', reason: 'unsupported_currency' })

    expect(determineInitialVisibility({
      amountMinor: MIN_PUBLIC_CNY_MINOR - 1,
      currency: 'CNY',
      displayName: 'Spammer',
      message: 'https://spam.example',
      isPublic: true,
      config,
    })).toEqual({ kind: 'below_threshold' })

    expect(determineInitialVisibility({
      amountMinor: MIN_PUBLIC_USD_MINOR,
      currency: 'USD',
      displayName: 'Alice',
      message: 'See https://evil.example',
      isPublic: true,
      config,
    })).toEqual({ kind: 'pending_review', reason: 'url_detected' })

    expect(determineInitialVisibility({
      amountMinor: MIN_PUBLIC_CNY_MINOR,
      currency: 'CNY',
      displayName: '张三',
      message: '联系我 13900139000',
      isPublic: true,
      config,
    })).toEqual({ kind: 'pending_review', reason: 'pii_detected' })

    expect(determineInitialVisibility({
      amountMinor: MIN_PUBLIC_USD_MINOR,
      currency: 'USD',
      displayName: 'Carol',
      message: 'Love it!',
      isPublic: true,
      config: { ...config, moderationEnabled: true },
    })).toEqual({ kind: 'pending_review', reason: 'moderation_mode' })

    expect(determineInitialVisibility({
      amountMinor: MIN_PUBLIC_USD_MINOR,
      currency: 'USD',
      displayName: 'Carol',
      message: 'Love it!',
      isPublic: true,
      config,
    })).toEqual({ kind: 'published' })
  })

  it('enforces lossless visibility transitions, private_donor immutability, and timestamp clamping', () => {
    const pendingUrl: WallVisibility = { kind: 'pending_review', reason: 'url_detected' }
    const hidden = applyVisibilityCommand(pendingUrl, { action: 'hide', reason: 'spam link' }, 1_700_000_000)
    expect(hidden).toEqual({
      ok: true,
      next: {
        kind: 'hidden',
        moderatedAt: 1_700_000_000,
        reason: 'spam link',
        previous: pendingUrl,
      },
    })
    if (!hidden.ok) throw new Error('expected hide transition')

    const restored = applyVisibilityCommand(hidden.next, { action: 'restore' }, 1_700_000_010)
    expect(restored).toEqual({ ok: true, next: pendingUrl })

    const privateState: WallVisibility = { kind: 'private_donor' }
    expect(applyVisibilityCommand(privateState, { action: 'approve' }, 100)).toEqual({
      ok: false,
      code: 'private_donor_immutable',
    })
    expect(applyVisibilityCommand(privateState, { action: 'hide', reason: 'nope' }, 100)).toEqual({
      ok: false,
      code: 'private_donor_immutable',
    })
    expect(applyVisibilityCommand(privateState, { action: 'restore' }, 100)).toEqual({
      ok: false,
      code: 'private_donor_immutable',
    })

    expect(clampDonatedAt(null, 1000)).toBe(1000)
    expect(clampDonatedAt(1000 + 500, 1000)).toBe(1060)
    expect(clampDonatedAt(950, 1000)).toBe(950)
  })
})

describe('provider protocol signing, parsing, and outbound adapters', () => {
  it('constructs Afdian MD5 signatures and parses order/sponsor payloads', () => {
    const signed = buildAfdianSignedBody('uid123', 'tok456', { out_trade_no: 'ORD-1' }, 1700000000)
    expect(signed.params).toBe('{"out_trade_no":"ORD-1"}')
    expect(signed.sign).toBe(md5Hex('tok456params{"out_trade_no":"ORD-1"}ts1700000000user_iduid123'))

    expect(parseAfdianWebhookPayload({ ec: 200, data: { order: { out_trade_no: 'ORD-1' } } })).toEqual({
      outTradeNo: 'ORD-1',
    })
    expect(parseAfdianWebhookPayload({ data: {} })).toBeNull()

    const orders = parseAfdianOrderQueryResponse({
      ec: 200,
      data: {
        list: [
          { out_trade_no: 'ORD-1', user_id: 'supporter-1', total_amount: '15.00', status: 2, remark: '加油！', create_time: 1_700_000_123 },
          { out_trade_no: 'ORD-2', user_id: 'supporter-2', total_amount: '10.00', status: 2, remark: '', create_time: '1700000456' },
          { out_trade_no: 'ORD-3', user_id: 'supporter-3', total_amount: '10.00', status: 2, remark: '' },
        ],
      },
    })
    expect(orders).toEqual({
      ok: true,
      orders: [
        { outTradeNo: 'ORD-1', userId: 'supporter-1', totalAmount: '15.00', status: 2, remark: '加油！', donatedAt: 1_700_000_123 },
        { outTradeNo: 'ORD-2', userId: 'supporter-2', totalAmount: '10.00', status: 2, remark: '', donatedAt: 1_700_000_456 },
        { outTradeNo: 'ORD-3', userId: 'supporter-3', totalAmount: '10.00', status: 2, remark: '', donatedAt: null },
      ],
    })
    expect(parseUnixTimestampSeconds(1_700_000_000)).toBe(1_700_000_000)
    expect(parseUnixTimestampSeconds(' 1700000000 ')).toBe(1_700_000_000)
    expect([0, -10, 1_700_000_000.5, 'not-a-timestamp', null].map(parseUnixTimestampSeconds)).toEqual([null, null, null, null, null])

    expect(parseAfdianSponsorName({
      ec: 200,
      data: { list: [{ user: { user_id: 'supporter-1', name: '小明' } }] },
    }, 'supporter-1')).toEqual({ ok: true, matched: true, displayName: '小明' })

    const parsedPage = parseAfdianSponsorPage({
      ec: 200,
      data: {
        list: [
          { user: { user_id: 'u-1', name: 'Alice' } },
          { user: { user_id: 'u-1', name: 'DuplicateIgnored' } },
          { user_id: 'u-2', user_name: 'Bob' },
          { user: { user_id: 'u-nameless', name: '   ' } },
        ],
      },
    })
    expect(parsedPage.ok).toBe(true)
    if (parsedPage.ok) {
      expect([...parsedPage.sponsors.entries()]).toEqual([
        ['u-1', 'Alice'],
        ['u-2', 'Bob'],
        ['u-nameless', null],
      ])
    }
  })

  it('authenticates Ko-fi webhooks before projecting payload fields and strips PII and credentials', () => {
    const form = new URLSearchParams({
      data: JSON.stringify({
        verification_token: 'kofi-secret',
        kofi_transaction_id: 'tx-100',
        type: 'Donation',
        amount: '5.00',
        currency: 'USD',
        is_public: false,
        from_name: 'Private Alice',
        message: 'Do not publish',
        email: 'alice@private.example',
        timestamp: '2026-11-20T12:00:00Z',
      }),
    }).toString()

    const rawObject = parseKofiFormDataObject(form)
    expect(rawObject).not.toBeNull()
    if (rawObject === null) throw new Error('expected valid Ko-fi form envelope')
    expect(verifyKofiWebhookToken(rawObject, 'kofi-secret')).toBe(true)
    expect(verifyKofiWebhookToken(rawObject, 'wrong')).toBe(false)
    expect(verifyKofiWebhookToken({}, 'kofi-secret')).toBe(false)
    expect(verifyKofiWebhookToken({ verification_token: '   ' }, 'kofi-secret')).toBe(false)
    expect(parseKofiFormDataObject('data=not-json')).toBeNull()
    expect(parseKofiFormDataObject('data=[]')).toBeNull()

    const parsed = parseKofiWebhookPayload(rawObject)
    expect(parsed.ok).toBe(true)
    if (!parsed.ok) throw new Error('expected valid Ko-fi payload')
    expect('email' in parsed.payload).toBe(false)
    expect('verificationToken' in parsed.payload).toBe(false)
    expect(parsed.payload.isPublic).toBe(false)
    expect(isSupportedKofiType('Donation')).toBe(true)
    expect(isSupportedKofiType('Shop Order')).toBe(false)
  })

  it('sorts NOWPayments IPN keys recursively and verifies HMAC-SHA512', () => {
    const ipnObject = {
      payment_status: 'finished',
      price_amount: 10.5,
      price_currency: 'usd',
      payment_id: 987654321,
      order_id: 'intent-abc',
      nested: { z: 1, a: 2 },
    }
    expect(Object.keys(sortJsonKeysRecursively(ipnObject) as Record<string, unknown>)).toEqual([
      'nested',
      'order_id',
      'payment_id',
      'payment_status',
      'price_amount',
      'price_currency',
    ])

    const sig = computeNowPaymentsSignature(ipnObject, 'ipn-secret')
    expect(verifyNowPaymentsSignature(ipnObject, sig, 'ipn-secret')).toBe(true)
    expect(verifyNowPaymentsSignature(ipnObject, sig.toUpperCase(), 'ipn-secret')).toBe(true)
    expect(verifyNowPaymentsSignature(ipnObject, '0'.repeat(128), 'ipn-secret')).toBe(false)

    const parsed = parseNowPaymentsIpnPayload(ipnObject)
    expect(parsed).toEqual({
      ok: true,
      ipn: {
        paymentId: '987654321',
        paymentStatus: 'finished',
        orderId: 'intent-abc',
        priceAmount: 10.5,
        priceCurrency: 'usd',
        donatedAt: null,
      },
    })
  })

  it('preserves __proto__ as an own data property during recursive key sorting and signature verification', () => {
    const rawWithProto = JSON.parse(
      '{"b":2,"__proto__":{"z":1,"a":2},"nested":[{"k":1,"__proto__":null}]}',
    ) as Record<string, unknown>
    const sorted = sortJsonKeysRecursively(rawWithProto) as Record<string, unknown>

    expect(Object.getPrototypeOf(sorted)).toBe(Object.prototype)
    expect(Object.hasOwn(sorted, '__proto__')).toBe(true)
    expect(Object.keys(sorted)).toEqual(['__proto__', 'b', 'nested'])
    expect('a' in sorted).toBe(false)
    expect(JSON.stringify(sorted)).toBe(
      '{"__proto__":{"a":2,"z":1},"b":2,"nested":[{"__proto__":null,"k":1}]}',
    )

    const basePayload = {
      payment_status: 'finished',
      price_amount: 10.5,
      price_currency: 'usd',
      payment_id: 987654321,
    }
    const baseSig = computeNowPaymentsSignature(basePayload, 'ipn-secret')
    const tamperedPayload = JSON.parse(
      `{"__proto__":{"injected":true},${JSON.stringify(basePayload).slice(1)}`,
    ) as Record<string, unknown>

    expect(verifyNowPaymentsSignature(tamperedPayload, baseSig, 'ipn-secret')).toBe(false)
    const tamperedSig = computeNowPaymentsSignature(tamperedPayload, 'ipn-secret')
    expect(verifyNowPaymentsSignature(tamperedPayload, tamperedSig, 'ipn-secret')).toBe(true)
  })

  it('handles Afdian sponsor page fallback, timeouts, and NOWPayments redirect rejection', async () => {
    let calls = 0
    const sponsorFallbackFetch: FetchLike = async (_url, init) => {
      calls += 1
      expect(init?.redirect).toBe('manual')
      const body = JSON.parse(String(init?.body)) as { params: string }
      if (body.params.includes('"user_id"')) {
        return Response.json({ ec: 200, data: { list: [] } })
      }
      return Response.json({
        ec: 200,
        data: { list: [{ user: { user_id: 'u-99', name: 'Fallback User' } }] },
      })
    }

    const found = await lookupAfdianSponsorName({
      userId: 'creator',
      token: 'tok',
      sponsorUserId: 'u-99',
      nowSec: 1700000000,
      fetchFn: sponsorFallbackFetch,
    })
    expect(found).toEqual({ ok: true, displayName: 'Fallback User' })
    expect(calls).toBe(2)

    const timeoutFetch: FetchLike = async () => {
      throw new Error('timeout')
    }
    const orderFail = await queryAfdianOrders({
      userId: 'creator',
      token: 'tok',
      outTradeNo: 'ORD-1',
      nowSec: 1700000000,
      fetchFn: timeoutFetch,
    })
    expect(orderFail).toEqual({ ok: false, reason: 'network_or_timeout' })

    const redirectFetch: FetchLike = async () => new Response(null, {
      status: 302,
      headers: { Location: 'https://attacker.example' },
    })
    const invoiceFail = await createNowPaymentsInvoice({
      apiKey: 'secret-key',
      priceAmountUsd: 10,
      orderId: 'intent-1',
      ipnCallbackUrl: 'https://donate.windshare.cc/v1/donations/webhooks/nowpayments',
      successUrl: 'https://windshare.cc/donate?status=submitted',
      cancelUrl: 'https://windshare.cc/donate',
      fetchFn: redirectFetch,
    })
    expect(invoiceFail).toEqual({ ok: false, reason: 'unexpected_redirect_302' })
  })

  it('restricts NOWPayments invoice_url to credential-free HTTPS nowpayments.io origins', async () => {
    expect(isValidNowPaymentsInvoiceUrl('https://nowpayments.io/payment/?iid=100')).toBe(true)
    expect(isValidNowPaymentsInvoiceUrl('https://pay.nowpayments.io/payment/?iid=100')).toBe(true)
    expect(isValidNowPaymentsInvoiceUrl('https://sandbox.nowpayments.io/payment/?iid=100')).toBe(true)
    expect(isValidNowPaymentsInvoiceUrl('https://NOWPAYMENTS.IO:443/payment/?iid=100')).toBe(true)

    const insecureHttpUrl = ['http', '//nowpayments.io/payment/?iid=100'].join(':')
    for (const invalidUrl of [
      insecureHttpUrl,
      'javascript:alert(1)',
      'https://phishing.example/payment/?iid=100',
      'https://evil-nowpayments.io/payment/?iid=100',
      'https://nowpayments.io.evil.example/payment/?iid=100',
      'https://nowpayments.io@evil.example/payment/?iid=100',
      'https://user:pass@nowpayments.io/payment/?iid=100',
      'https://nowpayments.io:8443/payment/?iid=100',
      '/payment/?iid=100',
      '',
      null,
      undefined,
    ]) {
      expect(isValidNowPaymentsInvoiceUrl(invalidUrl)).toBe(false)
    }

    const baseInput = {
      apiKey: 'secret-key',
      priceAmountUsd: 10,
      orderId: 'intent-2',
      ipnCallbackUrl: 'https://donate.windshare.cc/v1/donations/webhooks/nowpayments',
      successUrl: 'https://windshare.cc/donate?status=submitted',
      cancelUrl: 'https://windshare.cc/donate',
    }

    const foreignHostRes = await createNowPaymentsInvoice({
      ...baseInput,
      fetchFn: async () => Response.json({ invoice_url: 'https://evil.example/pay' }),
    })
    expect(foreignHostRes).toEqual({ ok: false, reason: 'invalid_invoice_url' })

    const validRes = await createNowPaymentsInvoice({
      ...baseInput,
      fetchFn: async () => Response.json({ invoice_url: 'https://nowpayments.io/payment/?iid=intent-2' }),
    })
    expect(validRes).toEqual({ ok: true, invoiceUrl: 'https://nowpayments.io/payment/?iid=intent-2' })
  })

  it('cancels unread outbound response bodies on non-2xx and redirect responses without masking the status reason', async () => {
    const createTrackedResponse = (status: number, cancelError?: Error) => {
      let cancelled = false
      const body = new ReadableStream<Uint8Array>({
        cancel() {
          cancelled = true
          if (cancelError !== undefined) {
            return Promise.reject(cancelError)
          }
        },
      })
      return {
        response: new Response(body, { status }),
        wasCancelled: () => cancelled,
      }
    }

    const redirectTracked = createTrackedResponse(302)
    const redirectResult = await queryAfdianOrders({
      userId: 'creator',
      token: 'tok',
      outTradeNo: 'ORD-302',
      nowSec: 1700000000,
      fetchFn: async () => redirectTracked.response,
    })
    expect(redirectResult).toEqual({ ok: false, reason: 'unexpected_redirect_302' })
    expect(redirectTracked.wasCancelled()).toBe(true)

    const errorTracked = createTrackedResponse(503)
    const errorResult = await queryAfdianOrders({
      userId: 'creator',
      token: 'tok',
      outTradeNo: 'ORD-503',
      nowSec: 1700000000,
      fetchFn: async () => errorTracked.response,
    })
    expect(errorResult).toEqual({ ok: false, reason: 'http_503' })
    expect(errorTracked.wasCancelled()).toBe(true)

    const brokenCancelTracked = createTrackedResponse(502, new Error('stream already reset'))
    const brokenCancelResult = await queryAfdianOrders({
      userId: 'creator',
      token: 'tok',
      outTradeNo: 'ORD-502',
      nowSec: 1700000000,
      fetchFn: async () => brokenCancelTracked.response,
    })
    expect(brokenCancelResult).toEqual({ ok: false, reason: 'http_502' })
    expect(brokenCancelTracked.wasCancelled()).toBe(true)
  })
})

describe('WallStore SQLite persistence, caching, rate limiting, and pagination', () => {
  it('enforces webhook idempotency, versioned ETag invalidation, and 50-entry wall cap', () => {
    const store = createInMemoryWallStore()
    const initial = store.getPublicWallSnapshot()
    expect(initial.etag).toBe('"wall-v0"')
    expect(matchesIfNoneMatch('W/"wall-v0"', initial.etag)).toBe(true)

    const first = store.commitDonation({
      provider: 'afdian',
      externalId: 'ORD-1',
      displayName: sanitizePlainText('Alice', 32),
      amountMinor: 1000,
      currency: 'CNY',
      minorDigits: 2,
      message: 'Thanks!',
      isPublic: true,
      donatedAt: 1000,
    }, 1000)
    expect(first.deduplicated).toBe(false)

    const duplicate = store.commitDonation({
      provider: 'afdian',
      externalId: 'ORD-1',
      displayName: 'Mutated',
      amountMinor: 9999,
      currency: 'CNY',
      minorDigits: 2,
      message: 'Should be ignored',
      isPublic: true,
      donatedAt: 1005,
    }, 1005)
    expect(duplicate.deduplicated).toBe(true)
    expect(duplicate.record.id).toBe(first.record.id)
    expect(duplicate.record.amountMinor).toBe(1000)

    const afterFirst = store.getPublicWallSnapshot()
    expect(afterFirst.version).toBe(1)
    expect(afterFirst.etag).toBe('"wall-v1"')
    expect(JSON.parse(afterFirst.json)).not.toHaveProperty('moderationEnabled')

    store.patchConfig({ moderationEnabled: true })
    expect(store.getPublicWallSnapshot().etag).toBe('"wall-v1"')
    store.patchConfig({ moderationEnabled: false })

    for (let i = 2; i <= PUBLIC_WALL_LIMIT + 5; i += 1) {
      store.commitDonation({
        provider: 'kofi',
        externalId: `tx-${i}`,
        displayName: `Supporter ${i}`,
        amountMinor: 500,
        currency: 'USD',
        minorDigits: 2,
        message: null,
        isPublic: true,
        donatedAt: 1000 + i,
      }, 1000 + i)
    }

    const capped = JSON.parse(store.getPublicWallSnapshot().json) as PublicWallResponse
    expect(capped.entries).toHaveLength(PUBLIC_WALL_LIMIT)
    expect(capped.entries[0]?.displayName).toBe(`Supporter ${PUBLIC_WALL_LIMIT + 5}`)
  })

  it('preserves lossless hidden.previous, blocks private_donor edits, and paginates tied timestamps', () => {
    const store = createInMemoryWallStore()
    const privateCommit = store.commitDonation({
      provider: 'kofi',
      externalId: 'priv-1',
      displayName: 'ShouldBeStripped',
      amountMinor: 1000,
      currency: 'USD',
      minorDigits: 2,
      message: 'Secret',
      isPublic: false,
      donatedAt: 2000,
    }, 2000)
    expect(privateCommit.record.displayName).toBeNull()
    expect(privateCommit.record.message).toBeNull()
    expect(privateCommit.record.visibility).toEqual({ kind: 'private_donor' })
    expect(store.moderateDonationVisibility(privateCommit.record.id, { action: 'approve' }, 2001)).toEqual({
      ok: false,
      code: 'private_donor_immutable',
    })

    const flagged = store.commitDonation({
      provider: 'afdian',
      externalId: 'url-1',
      displayName: 'Bob',
      amountMinor: 2000,
      currency: 'CNY',
      minorDigits: 2,
      message: 'https://example.org',
      isPublic: true,
      donatedAt: 2000,
    }, 2000)
    expect(flagged.record.visibility).toEqual({ kind: 'pending_review', reason: 'url_detected' })

    const hidden = store.moderateDonationVisibility(flagged.record.id, { action: 'hide', reason: 'ad' }, 2010)
    expect(hidden.ok).toBe(true)
    const restored = store.moderateDonationVisibility(flagged.record.id, { action: 'restore' }, 2020)
    expect(restored.ok).toBe(true)
    if (restored.ok) {
      expect(restored.record.visibility).toEqual({ kind: 'pending_review', reason: 'url_detected' })
    }

    // Add a third record sharing the same donatedAt to verify deterministic tie-breaking pagination.
    store.commitDonation({
      provider: 'nowpayments',
      externalId: 'np-1',
      displayName: 'Carol',
      amountMinor: 1000,
      currency: 'USD',
      minorDigits: 2,
      message: 'Nice',
      isPublic: true,
      donatedAt: 2000,
    }, 2000)

    const page1 = store.listAdminDonations(null, 2)
    expect(page1.items).toHaveLength(2)
    expect(page1.nextCursor).not.toBeNull()

    const page2 = store.listAdminDonations(page1.nextCursor, 2)
    expect(page2.items).toHaveLength(1)
    expect(page2.nextCursor).toBeNull()

    const ids = [...page1.items, ...page2.items].map((item: DonationRecord) => item.id)
    expect(new Set(ids).size).toBe(3)
  })

  it('enforces dual-layer IP and global rate limits and lazily cleans expired intents/sessions', () => {
    const store = createInMemoryWallStore()
    const now = 1_700_000_000

    // Exhaust per-IP admin login bucket (5 attempts).
    for (let i = 0; i < 5; i += 1) {
      expect(store.consumeDualRateLimit('admin_login', 'client-a', now).allowed).toBe(true)
    }
    const blockedIp = store.consumeDualRateLimit('admin_login', 'client-a', now)
    expect(blockedIp.allowed).toBe(false)
    expect(blockedIp.retryAfterSeconds).toBeGreaterThan(0)

    // Rotating IPs still hit the global cap (20 total across all IPs).
    for (let i = 0; i < 15; i += 1) {
      expect(store.consumeDualRateLimit('admin_login', `client-rot-${i}`, now).allowed).toBe(true)
    }
    expect(store.consumeDualRateLimit('admin_login', 'client-rot-99', now).allowed).toBe(false)

    const createdIntent = store.createCryptoIntent({
      clientIp: 'client-crypto',
      displayName: 'Crypto Donor',
      message: 'Go!',
      requestedUsdMinor: 1000,
      returnOrigin: 'https://windshare.cc',
      nowSec: now,
    })
    expect(createdIntent.ok).toBe(true)
    if (!createdIntent.ok) throw new Error('expected intent')

    expect(store.getCryptoIntent(createdIntent.intent.intentId, now + 100)?.displayName).toBe('Crypto Donor')
    expect(store.getCryptoIntent(createdIntent.intent.intentId, createdIntent.intent.expiresAt)).toBeNull()

    store.createAdminSession({ sessionHash: 'hash-old', csrfToken: 'csrf-old', nowSec: now })
    expect(store.verifyAdminSession('hash-old', now + 10)?.csrfToken).toBe('csrf-old')
    expect(store.verifyAdminSession('hash-old', now + 12 * 3600)).toBeNull()
  })

  it('invalidates existing admin sessions immediately when DONATE_ADMIN_PASSWORD is rotated or deleted', async () => {
    const store = createInMemoryWallStore()
    const now = 1_700_000_000
    const origin = 'https://donate.windshare.cc'
    let doCalls = 0

    const wallNamespace = createWallNamespaceFromStore(store, {
      onCall: () => {
        doCalls += 1
      },
    })

    const createEnv = (adminSecret?: string): DonationsEnv => ({
      DONATION_WALL: wallNamespace,
      AFDIAN_USER_ID: 'creator',
      ALLOWED_ORIGINS: 'https://windshare.cc',
      DONATE_API_ORIGIN: origin,
      ...(adminSecret === undefined ? {} : { DONATE_ADMIN_PASSWORD: adminSecret }),
    })

    const loginWith = async (env: DonationsEnv, credential: string): Promise<Response> => handleWorkerFetch(
      new Request(`${origin}/v1/admin/login`, {
        method: 'POST',
        headers: {
          Origin: origin,
          'Content-Type': 'application/json',
          'CF-Connecting-IP': '198.51.100.42',
        },
        body: JSON.stringify({ password: credential }),
      }),
      env,
      { nowSec: () => now },
    )

    const initialSecret = randomBytes(16).toString('hex')
    const rotatedSecret = randomBytes(16).toString('hex')
    const initialEnv = createEnv(initialSecret)

    const loginV1 = await loginWith(initialEnv, initialSecret)
    expect(loginV1.status).toBe(200)
    const cookieV1 = (loginV1.headers.get('set-cookie') ?? '').split(';', 1)[0] ?? ''
    expect(cookieV1.startsWith('__Host-windshare_donate_admin=')).toBe(true)

    const sessionBeforeRotate = await handleWorkerFetch(
      new Request(`${origin}/v1/admin/session`, { headers: { Cookie: cookieV1 } }),
      initialEnv,
      { nowSec: () => now + 60 },
    )
    expect(sessionBeforeRotate.status).toBe(200)

    // Rotating the secret invalidates existing cookies immediately without DB migration.
    const rotatedEnv = createEnv(rotatedSecret)
    const staleSession = await handleWorkerFetch(
      new Request(`${origin}/v1/admin/session`, { headers: { Cookie: cookieV1 } }),
      rotatedEnv,
      { nowSec: () => now + 60 },
    )
    expect(staleSession.status).toBe(401)

    const staleDonations = await handleWorkerFetch(
      new Request(`${origin}/v1/admin/donations`, { headers: { Cookie: cookieV1 } }),
      rotatedEnv,
      { nowSec: () => now + 60 },
    )
    expect(staleDonations.status).toBe(401)

    const loginV2 = await loginWith(rotatedEnv, rotatedSecret)
    expect(loginV2.status).toBe(200)
    const cookieV2 = (loginV2.headers.get('set-cookie') ?? '').split(';', 1)[0] ?? ''

    const freshSession = await handleWorkerFetch(
      new Request(`${origin}/v1/admin/session`, { headers: { Cookie: cookieV2 } }),
      rotatedEnv,
      { nowSec: () => now + 60 },
    )
    expect(freshSession.status).toBe(200)

    // Deleting or blanking the secret fails closed across all admin routes before any DO call.
    for (const disabledEnv of [createEnv(undefined), createEnv('')]) {
      const callsBefore = doCalls
      const disabledSession = await handleWorkerFetch(
        new Request(`${origin}/v1/admin/session`, { headers: { Cookie: cookieV2 } }),
        disabledEnv,
        { nowSec: () => now + 60 },
      )
      expect(disabledSession.status).toBe(503)
      expect(await disabledSession.json()).toEqual({ error: 'admin_not_configured' })

      const disabledDonations = await handleWorkerFetch(
        new Request(`${origin}/v1/admin/donations`, { headers: { Cookie: cookieV2 } }),
        disabledEnv,
        { nowSec: () => now + 60 },
      )
      expect(disabledDonations.status).toBe(503)

      const disabledLogin = await loginWith(disabledEnv, rotatedSecret)
      expect(disabledLogin.status).toBe(503)
      expect(doCalls).toBe(callsBefore)
    }
  })

  it('batch-checks existing external IDs and atomically commits batches with a single wall version bump', () => {
    const store = createInMemoryWallStore()
    store.commitDonation({
      provider: 'afdian',
      externalId: 'AF-PRE',
      displayName: 'Existing',
      amountMinor: 1500,
      currency: 'CNY',
      minorDigits: 2,
      message: null,
      isPublic: true,
      donatedAt: 1000,
    }, 1000)
    store.commitDonation({
      provider: 'kofi',
      externalId: 'AF-OTHER-PROVIDER',
      displayName: 'KofiUser',
      amountMinor: 1000,
      currency: 'USD',
      minorDigits: 2,
      message: null,
      isPublic: true,
      donatedAt: 1001,
    }, 1001)

    const versionBefore = store.getPublicWallSnapshot().version
    expect(store.findExistingExternalIds('afdian', ['AF-PRE', 'AF-PRE', 'AF-NEW', 'AF-OTHER-PROVIDER', ''])).toEqual([
      'AF-PRE',
    ])

    const batchResults = store.commitDonationsBatch([
      {
        provider: 'afdian',
        externalId: 'AF-PRE',
        displayName: 'Ignored',
        amountMinor: 9999,
        currency: 'CNY',
        minorDigits: 2,
        message: null,
        isPublic: true,
        donatedAt: 1002,
      },
      {
        provider: 'afdian',
        externalId: 'AF-B1',
        displayName: 'First',
        amountMinor: 1000,
        currency: 'CNY',
        minorDigits: 2,
        message: 'hi',
        isPublic: true,
        donatedAt: 1003,
      },
      {
        provider: 'afdian',
        externalId: 'AF-B2',
        displayName: 'Second',
        amountMinor: 2000,
        currency: 'CNY',
        minorDigits: 2,
        message: 'hello',
        isPublic: true,
        donatedAt: 1004,
      },
      {
        provider: 'afdian',
        externalId: 'AF-B1',
        displayName: 'DuplicateInSameBatch',
        amountMinor: 1000,
        currency: 'CNY',
        minorDigits: 2,
        message: null,
        isPublic: true,
        donatedAt: 1005,
      },
    ], 1005)

    expect(batchResults.map(r => r.deduplicated)).toEqual([true, false, false, true])
    expect(store.getPublicWallSnapshot().version).toBe(versionBefore + 1)

    // Re-submitting an all-duplicate batch does not bump wall_version.
    const dupOnly = store.commitDonationsBatch([{
      provider: 'afdian',
      externalId: 'AF-B1',
      displayName: 'First',
      amountMinor: 1000,
      currency: 'CNY',
      minorDigits: 2,
      message: 'hi',
      isPublic: true,
      donatedAt: 1006,
    }], 1006)
    expect(dupOnly[0]?.deduplicated).toBe(true)
    expect(store.getPublicWallSnapshot().version).toBe(versionBefore + 1)
  })
})

describe('WallStore rate-limit eviction indexing', () => {
  it('indexes rate_limits(updated_at) for stale bucket eviction without full table scans', () => {
    const { store, db } = createInMemoryWallStoreWithDb()
    const now = 1_700_000_000

    const plan = db.prepare(
      `EXPLAIN QUERY PLAN
       DELETE FROM rate_limits WHERE bucket_key IN (
         SELECT bucket_key FROM rate_limits WHERE updated_at <= ? LIMIT ?
       )`,
    ).all(now - 3600, 32) as Array<{ detail: string }>
    const planDetails = plan.map(row => row.detail)
    expect(planDetails.some(detail => detail.includes('idx_rate_limits_updated_at'))).toBe(true)
    expect(planDetails.some(detail => detail.includes('SCAN rate_limits'))).toBe(false)

    expect(store.consumeDualRateLimit('crypto', 'stale-ip', now).allowed).toBe(true)
    expect(store.consumeDualRateLimit('crypto', 'fresh-ip', now + 1800).allowed).toBe(true)
    expect(store.consumeDualRateLimit('crypto', 'trigger-ip', now + 3600).allowed).toBe(true)

    const remainingKeys = (db.prepare(
      'SELECT bucket_key FROM rate_limits ORDER BY bucket_key',
    ).all() as Array<{ bucket_key: string }>).map(row => row.bucket_key)
    expect(remainingKeys).toEqual(['crypto:global', 'crypto:ip:fresh-ip', 'crypto:ip:trigger-ip'])
  })
})

describe('admin donations page limit normalization', () => {
  it('guards SQLite LIMIT against fractional/non-safe numbers and non-decimal query params', async () => {
    const store = createInMemoryWallStore()
    const now = 1_700_000_000
    for (let i = 1; i <= 3; i += 1) {
      store.commitDonation({
        provider: 'kofi',
        externalId: `lim-${i}`,
        displayName: `Donor ${i}`,
        amountMinor: 500,
        currency: 'USD',
        minorDigits: 2,
        message: null,
        isPublic: true,
        donatedAt: now + i,
      }, now + i)
    }

    for (const invalidLimit of [1.5, Number.NaN, Number.POSITIVE_INFINITY, Number.MAX_SAFE_INTEGER + 1]) {
      expect(store.listAdminDonations(null, invalidLimit).items).toHaveLength(3)
    }
    expect(store.listAdminDonations(null, 0).items).toHaveLength(1)
    expect(store.listAdminDonations(null, -5).items).toHaveLength(1)
    expect(store.listAdminDonations(null, 2).items).toHaveLength(2)

    const origin = 'https://donate.windshare.cc'
    const adminPassword = randomBytes(16).toString('hex')
    const env: DonationsEnv = {
      DONATION_WALL: createWallNamespaceFromStore(store),
      AFDIAN_USER_ID: 'creator',
      ALLOWED_ORIGINS: 'https://windshare.cc',
      DONATE_API_ORIGIN: origin,
      DONATE_ADMIN_PASSWORD: adminPassword,
    }
    const loginRes = await handleWorkerFetch(
      new Request(`${origin}/v1/admin/login`, {
        method: 'POST',
        headers: { Origin: origin, 'Content-Type': 'application/json', 'CF-Connecting-IP': '198.51.100.88' },
        body: JSON.stringify({ password: adminPassword }),
      }),
      env,
      { nowSec: () => now },
    )
    expect(loginRes.status).toBe(200)
    const cookie = (loginRes.headers.get('set-cookie') ?? '').split(';', 1)[0] ?? ''

    const fetchPage = async (query: string) => {
      const res = await handleWorkerFetch(
        new Request(`${origin}/v1/admin/donations?${query}`, { headers: { Cookie: cookie } }),
        env,
        { nowSec: () => now + 10 },
      )
      expect(res.status).toBe(200)
      return res.json() as Promise<{ items: DonationRecord[]; nextCursor: string | null }>
    }

    for (const query of ['limit=1.5', 'limit=1.0', 'limit=', 'limit=%20', 'limit=0x2', 'limit=1e0', 'limit=abc', 'limit=9007199254740992']) {
      const page = await fetchPage(query)
      expect(page.items).toHaveLength(3)
      expect(page.nextCursor).toBeNull()
    }

    const bounded = await fetchPage('limit=2')
    expect(bounded.items).toHaveLength(2)
    expect(bounded.nextCursor).not.toBeNull()

    const clamped = await fetchPage('limit=-5')
    expect(clamped.items).toHaveLength(1)
    expect(clamped.nextCursor).not.toBeNull()
  })
})

function createSyncHarness() {
  const store = createInMemoryWallStore()
  const origin = 'https://donate.windshare.cc'
  const adminPassword = randomBytes(16).toString('hex')
  const now = 1_700_000_000
  const counts = { doCalls: 0, orderCalls: 0, sponsorPageCalls: 0, sponsorDirectCalls: 0 }
  const committedBatches: string[][] = []

  const wallNamespace = createWallNamespaceFromStore(store, {
    onCall: () => {
      counts.doCalls += 1
    },
    overrides: {
      commitDonationsBatch: async (inputs, nowSec) => {
        counts.doCalls += 1
        committedBatches.push(inputs.map(item => item.externalId))
        return store.commitDonationsBatch(inputs, nowSec)
      },
    },
  })

  const env: DonationsEnv = {
    DONATION_WALL: wallNamespace,
    AFDIAN_USER_ID: 'creator',
    AFDIAN_TOKEN: 'afdian-token',
    AFDIAN_WEBHOOK_SECRET: 'afdian-webhook-secret',
    ALLOWED_ORIGINS: 'https://windshare.cc',
    DONATE_API_ORIGIN: origin,
    DONATE_ADMIN_PASSWORD: adminPassword,
  }

  const authenticate = async (): Promise<{ cookie: string; csrf: string }> => {
    const res = await handleWorkerFetch(
      new Request(`${origin}/v1/admin/login`, {
        method: 'POST',
        headers: { Origin: origin, 'Content-Type': 'application/json', 'CF-Connecting-IP': '198.51.100.77' },
        body: JSON.stringify({ password: adminPassword }),
      }),
      env,
      { nowSec: () => now },
    )
    const cookie = (res.headers.get('set-cookie') ?? '').split(';', 1)[0] ?? ''
    const data = await res.json() as { csrfToken: string }
    return { cookie, csrf: data.csrfToken }
  }

  const runSync = async (
    auth: { cookie: string; csrf: string },
    fetchFn: FetchLike,
  ): Promise<Response> => handleWorkerFetch(
    new Request(`${origin}/v1/admin/donations/sync-afdian`, {
      method: 'POST',
      headers: { Origin: origin, Cookie: auth.cookie, 'X-CSRF-Token': auth.csrf },
    }),
    env,
    { nowSec: () => now, fetchFn },
  )

  const postAfdianWebhook = async (
    outTradeNo: string,
    fetchFn: FetchLike,
  ): Promise<Response> => handleWorkerFetch(
    new Request(`${origin}/v1/donations/webhooks/afdian/afdian-webhook-secret`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ec: 200, data: { order: { out_trade_no: outTradeNo } } }),
    }),
    env,
    { nowSec: () => now, fetchFn },
  )

  return { store, now, counts, committedBatches, authenticate, runSync, postAfdianWebhook }
}

describe('handleSyncAfdian batching, sponsor cache, and subrequest budgeting', () => {
  it('batches 50 existing orders into a single DO check with zero sponsor lookups', async () => {
    const { store, counts, authenticate, runSync } = createSyncHarness()
    const auth = await authenticate()
    const existingOrders = Array.from({ length: 50 }, (_, index) => ({
      out_trade_no: `AF-OLD-${index}`,
      user_id: `u-${index}`,
      total_amount: '10.00',
      status: 2,
      remark: '',
    }))
    store.commitDonationsBatch(
      existingOrders.map(order => ({
        provider: 'afdian',
        externalId: order.out_trade_no,
        displayName: 'Old',
        amountMinor: 1000,
        currency: 'CNY',
        minorDigits: 2,
        message: null,
        isPublic: true,
        donatedAt: 1_700_000_000,
      })),
      1_700_000_000,
    )

    counts.doCalls = 0
    const fetchFn: FetchLike = async (url) => {
      if (String(url).endsWith('/query-order')) {
        counts.orderCalls += 1
        return Response.json({ ec: 200, data: { list: existingOrders } })
      }
      counts.sponsorPageCalls += 1
      return Response.json({ ec: 200, data: { list: [] } })
    }

    const res = await runSync(auth, fetchFn)
    expect(res.status).toBe(200)
    expect(await res.json()).toEqual({ fetched: 50, paid: 50, committed: 0, deduplicated: 50, deferred: 0 })
    // 1 auth verify + 1 exists-batch; zero sponsor calls and zero commit-batch calls.
    expect(counts.doCalls).toBe(2)
    expect(counts.orderCalls).toBe(1)
    expect(counts.sponsorPageCalls).toBe(0)
  })

  it('primes page-1 sponsor cache, deduplicates missed user lookups, and defers beyond budget without anonymizing', async () => {
    const { store, counts, authenticate, runSync } = createSyncHarness()
    const auth = await authenticate()
    // 2 orders share page-1 user u-page1; 2 orders share missed user u-miss-0; 44 more unique missed users => 45 unique misses total.
    const orders = [
      { out_trade_no: 'AF-P1-A', user_id: 'u-page1', total_amount: '15.00', status: 2, remark: '' },
      { out_trade_no: 'AF-P1-B', user_id: 'u-page1', total_amount: '15.00', status: 2, remark: '' },
      { out_trade_no: 'AF-M0-A', user_id: 'u-miss-0', total_amount: '15.00', status: 2, remark: '' },
      { out_trade_no: 'AF-M0-B', user_id: 'u-miss-0', total_amount: '15.00', status: 2, remark: '' },
      ...Array.from({ length: 44 }, (_, index) => ({
        out_trade_no: `AF-M-${index + 1}`,
        user_id: `u-miss-${index + 1}`,
        total_amount: '15.00',
        status: 2,
        remark: '',
      })),
    ]

    const fetchFn: FetchLike = async (url, init) => {
      if (String(url).endsWith('/query-order')) {
        counts.orderCalls += 1
        return Response.json({ ec: 200, data: { list: orders } })
      }
      const body = JSON.parse(String(init?.body)) as { params: string }
      const params = JSON.parse(body.params) as { user_id?: string }
      if (params.user_id !== undefined) {
        counts.sponsorDirectCalls += 1
        return Response.json({
          ec: 200,
          data: { list: [{ user: { user_id: params.user_id, name: `Name-${params.user_id}` } }] },
        })
      }
      counts.sponsorPageCalls += 1
      return Response.json({
        ec: 200,
        data: { list: [{ user: { user_id: 'u-page1', name: 'PageOneUser' } }] },
      })
    }

    counts.doCalls = 0
    const versionBefore = store.getPublicWallSnapshot().version
    const firstRes = await runSync(auth, fetchFn)
    expect(firstRes.status).toBe(200)
    // 2 page-1 orders + 2 u-miss-0 orders + 39 unique missed orders (u-miss-1..39) = 43 committed; 5 deferred (u-miss-40..44).
    expect(await firstRes.json()).toEqual({ fetched: 48, paid: 48, committed: 43, deduplicated: 0, deferred: 5 })
    expect(counts.doCalls).toBe(3)
    expect(counts.orderCalls).toBe(1)
    expect(counts.sponsorPageCalls).toBe(1)
    expect(counts.sponsorDirectCalls).toBe(40)
    expect(store.getPublicWallSnapshot().version).toBe(versionBefore + 1)

    // Re-running sync picks up only the 5 deferred orders with real display names (never anonymized).
    counts.doCalls = 0
    counts.orderCalls = 0
    counts.sponsorPageCalls = 0
    counts.sponsorDirectCalls = 0
    const secondRes = await runSync(auth, fetchFn)
    expect(secondRes.status).toBe(200)
    expect(await secondRes.json()).toEqual({ fetched: 48, paid: 48, committed: 5, deduplicated: 43, deferred: 0 })
    expect(counts.doCalls).toBe(3)
    expect(counts.sponsorPageCalls).toBe(1)
    expect(counts.sponsorDirectCalls).toBe(5)

    const allDonations = store.listAdminDonations(null, 100).items
    expect(allDonations).toHaveLength(48)
    expect(allDonations.every(item => item.displayName !== null)).toBe(true)
    expect(allDonations.find(item => item.externalId === 'AF-P1-B')?.displayName).toBe('PageOneUser')
    expect(allDonations.find(item => item.externalId === 'AF-M0-B')?.displayName).toBe('Name-u-miss-0')
    expect(allDonations.find(item => item.externalId === 'AF-M-44')?.displayName).toBe('Name-u-miss-44')
  })

  it('stops without anonymizing on 429 rate limits and degrades gracefully on non-429 sponsor outage', async () => {
    const { store, counts, authenticate, runSync } = createSyncHarness()
    const auth = await authenticate()
    const orders = [
      { out_trade_no: 'AF-RL-1', user_id: 'u-a', total_amount: '15.00', status: 2, remark: '' },
      { out_trade_no: 'AF-RL-2', user_id: 'u-b', total_amount: '15.00', status: 2, remark: '' },
      { out_trade_no: 'AF-RL-3', user_id: 'u-c', total_amount: '15.00', status: 2, remark: '' },
    ]

    // Page-1 429 aborts before committing anonymous records.
    const page429Res = await runSync(auth, async (url) => {
      if (String(url).endsWith('/query-order')) {
        return Response.json({ ec: 200, data: { list: orders } })
      }
      return new Response('rate limited', { status: 429 })
    })
    expect(page429Res.status).toBe(502)
    expect(await page429Res.json()).toEqual({ error: 'afdian_rate_limited', reason: 'http_429' })
    expect(store.listAdminDonations(null, 10).items).toHaveLength(0)

    // Mid-batch direct lookup 429 commits already-resolved orders and defers remaining without hammering upstream.
    const mid429Res = await runSync(auth, async (url, init) => {
      if (String(url).endsWith('/query-order')) {
        return Response.json({ ec: 200, data: { list: orders } })
      }
      const body = JSON.parse(String(init?.body)) as { params: string }
      const params = JSON.parse(body.params) as { user_id?: string }
      if (params.user_id === undefined) {
        return Response.json({ ec: 200, data: { list: [{ user: { user_id: 'u-a', name: 'Alice' } }] } })
      }
      counts.sponsorDirectCalls += 1
      return new Response('rate limited', { status: 429 })
    })
    expect(mid429Res.status).toBe(200)
    expect(await mid429Res.json()).toEqual({ fetched: 3, paid: 3, committed: 1, deduplicated: 0, deferred: 2 })
    expect(counts.sponsorDirectCalls).toBe(1)
    expect(store.listAdminDonations(null, 10).items.map(item => [item.externalId, item.displayName])).toEqual([
      ['AF-RL-1', 'Alice'],
    ])

    // Non-429 page-1 outage (e.g. 504) fails fast in 1 sponsor call and commits verified orders as anonymous.
    let sponsorAttempts = 0
    const outageRes = await runSync(auth, async (url) => {
      if (String(url).endsWith('/query-order')) {
        return Response.json({ ec: 200, data: { list: orders } })
      }
      sponsorAttempts += 1
      return new Response('timeout', { status: 504 })
    })
    expect(outageRes.status).toBe(200)
    expect(await outageRes.json()).toEqual({ fetched: 3, paid: 3, committed: 2, deduplicated: 1, deferred: 0 })
    expect(sponsorAttempts).toBe(1)
  })

  it('preserves Afdian create_time on sync and webhook, and submits sync batches oldest-first', async () => {
    const { store, now, committedBatches, authenticate, runSync, postAfdianWebhook } = createSyncHarness()
    const auth = await authenticate()

    // Seed a newer Ko-fi donation so backfilled historical Afdian orders stay below it on the wall.
    store.commitDonation({
      provider: 'kofi',
      externalId: 'KOFI-RECENT',
      displayName: 'Recent Kofi',
      amountMinor: 500,
      currency: 'USD',
      minorDigits: 2,
      message: 'recent',
      isPublic: true,
      donatedAt: now - 120,
    }, now - 120)

    const syncRes = await runSync(auth, async (url) => {
      if (String(url).endsWith('/query-order')) {
        return Response.json({
          ec: 200,
          data: {
            list: [
              { out_trade_no: 'AF-HIST-NEW', user_id: '', total_amount: '15.00', status: 2, remark: 'hist-new', create_time: now - 3600 },
              { out_trade_no: 'AF-HIST-OLD', user_id: '', total_amount: '15.00', status: 2, remark: 'hist-old', create_time: now - 7200 },
            ],
          },
        })
      }
      return Response.json({ ec: 200, data: { list: [] } })
    })
    expect(syncRes.status).toBe(200)
    expect(committedBatches).toEqual([['AF-HIST-OLD', 'AF-HIST-NEW']])

    const wall = JSON.parse(store.getPublicWallSnapshot().json) as PublicWallResponse
    expect(wall.entries.map(entry => [entry.message, entry.donatedAt])).toEqual([
      ['recent', now - 120],
      ['hist-new', now - 3600],
      ['hist-old', now - 7200],
    ])

    // Webhook preserves create_time and clamps future skew to now + 60s.
    const webhookRes = await postAfdianWebhook('AF-WH-FUTURE', async () => Response.json({
      ec: 200,
      data: {
        list: [{
          out_trade_no: 'AF-WH-FUTURE',
          user_id: '',
          total_amount: '20.00',
          status: 2,
          remark: 'wh-future',
          create_time: now + 500,
        }],
      },
    }))
    expect(webhookRes.status).toBe(200)
    const wallAfterWebhook = JSON.parse(store.getPublicWallSnapshot().json) as PublicWallResponse
    expect(wallAfterWebhook.entries[0]?.message).toBe('wh-future')
    expect(wallAfterWebhook.entries[0]?.donatedAt).toBe(now + 60)
  })
})
