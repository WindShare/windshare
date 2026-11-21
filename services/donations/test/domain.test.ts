import { DatabaseSync, type SQLInputValue } from 'node:sqlite'
import { describe, expect, it } from 'vitest'
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
  lookupAfdianSponsorName,
  queryAfdianOrders,
  type FetchLike,
} from '../src/providers'
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
  parseAfdianWebhookPayload,
  parseKofiWebhookBody,
  parseNowPaymentsIpnPayload,
  sortJsonKeysRecursively,
  verifyKofiToken,
  verifyNowPaymentsSignature,
} from '../src/verify'
import { WallStore, matchesIfNoneMatch, type SqlStoragePort } from '../src/wall-store'

function createInMemoryWallStore(): WallStore {
  const db = new DatabaseSync(':memory:')
  const sqlPort: SqlStoragePort = {
    exec(query: string, ...bindings: unknown[]): Iterable<Record<string, unknown>> {
      const stmt = db.prepare(query)
      const params = bindings as SQLInputValue[]
      const trimmed = query.trimStart().toUpperCase()
      if (trimmed.startsWith('SELECT')) {
        return stmt.all(...params) as Record<string, unknown>[]
      }
      stmt.run(...params)
      return []
    },
  }
  const runTx = <T,>(closure: () => T): T => {
    // node:sqlite supports nested SAVEPOINT transactions cleanly.
    db.exec('SAVEPOINT wall_tx')
    try {
      const result = closure()
      db.exec('RELEASE SAVEPOINT wall_tx')
      return result
    } catch (error) {
      db.exec('ROLLBACK TO SAVEPOINT wall_tx')
      db.exec('RELEASE SAVEPOINT wall_tx')
      throw error
    }
  }
  return new WallStore(sqlPort, runTx)
}

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
    expect(parseFixedPointAmount('12.34567', 'AUD')).toBeNull()
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
        list: [{
          out_trade_no: 'ORD-1',
          user_id: 'supporter-1',
          total_amount: '15.00',
          status: 2,
          remark: '加油！',
        }],
      },
    })
    expect(orders).toEqual({
      ok: true,
      orders: [{
        outTradeNo: 'ORD-1',
        userId: 'supporter-1',
        totalAmount: '15.00',
        status: 2,
        remark: '加油！',
      }],
    })

    expect(parseAfdianSponsorName({
      ec: 200,
      data: { list: [{ user: { user_id: 'supporter-1', name: '小明' } }] },
    }, 'supporter-1')).toEqual({ ok: true, matched: true, displayName: '小明' })
  })

  it('parses Ko-fi webhooks while physically stripping email and failing closed on privacy', () => {
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

    const parsed = parseKofiWebhookBody(form)
    expect(parsed.ok).toBe(true)
    if (!parsed.ok) throw new Error('expected valid Ko-fi payload')
    expect('email' in parsed.payload).toBe(false)
    expect(parsed.payload.isPublic).toBe(false)
    expect(verifyKofiToken(parsed.payload.verificationToken, 'kofi-secret')).toBe(true)
    expect(verifyKofiToken(parsed.payload.verificationToken, 'wrong')).toBe(false)
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
})
