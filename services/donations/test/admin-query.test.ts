import { randomBytes } from 'node:crypto'
import { describe, expect, it } from 'vitest'
import { parseMajorTwoDecimalToMinor, formatDonationMoney } from '../admin-ui/src/format'
import type { DonationsEnv } from '../src/env'
import type { AdminDonationsPage } from '../src/model'
import { handleWorkerFetch } from '../src/router'
import { createInMemoryWallStore, createWallNamespaceFromStore } from './wall-harness'

describe('admin donations server-side filtering, counts, and fixed-point currency helpers', () => {
  it('filters by visibility, provider, and escaped LIKE query while preserving global counts', async () => {
    const store = createInMemoryWallStore()
    const now = 1_760_000_000

    store.commitDonation({
      provider: 'afdian',
      externalId: 'AF-100_1',
      displayName: 'Alice',
      amountMinor: 2000,
      currency: 'CNY',
      minorDigits: 2,
      message: 'Keep shipping!',
      isPublic: true,
      donatedAt: now + 1,
    }, now + 1)

    const flagged = store.commitDonation({
      provider: 'kofi',
      externalId: 'KOFI-URL-1',
      displayName: 'Bob',
      amountMinor: 1000,
      currency: 'USD',
      minorDigits: 2,
      message: 'Check https://promo.example for 100% off',
      isPublic: true,
      donatedAt: now + 2,
    }, now + 2)

    store.commitDonation({
      provider: 'nowpayments',
      externalId: 'NP-SMALL',
      displayName: 'Carol',
      amountMinor: 100,
      currency: 'USD',
      minorDigits: 2,
      message: 'Coffee',
      isPublic: true,
      donatedAt: now + 3,
    }, now + 3)

    store.commitDonation({
      provider: 'kofi',
      externalId: 'KOFI-PRIV',
      displayName: 'Secret',
      amountMinor: 2500,
      currency: 'USD',
      minorDigits: 2,
      message: 'Hidden',
      isPublic: false,
      donatedAt: now + 4,
    }, now + 4)

    const toHide = store.commitDonation({
      provider: 'afdian',
      externalId: 'AF-HIDE',
      displayName: 'Dave',
      amountMinor: 1500,
      currency: 'CNY',
      minorDigits: 2,
      message: 'Spam text',
      isPublic: true,
      donatedAt: now + 5,
    }, now + 5)
    store.moderateDonationVisibility(toHide.record.id, { action: 'hide', reason: 'spam' }, now + 6)

    const origin = 'https://donate.windshare.cc'
    const adminPassword = randomBytes(16).toString('hex')
    const env: DonationsEnv = {
      DONATION_WALL: createWallNamespaceFromStore(store),
      AFDIAN_USER_ID: 'creator',
      ALLOWED_ORIGINS: 'https://windshare.cc',
      DONATE_API_ORIGIN: origin,
      DONATE_ADMIN_PASSWORD: adminPassword,
      AFDIAN_TOKEN: 'configured-token',
    }

    const loginRes = await handleWorkerFetch(
      new Request(`${origin}/v1/admin/login`, {
        method: 'POST',
        headers: {
          Origin: origin,
          'Content-Type': 'application/json',
          'CF-Connecting-IP': '198.51.100.90',
        },
        body: JSON.stringify({ password: adminPassword }),
      }),
      env,
      { nowSec: () => now + 10 },
    )
    expect(loginRes.status).toBe(200)
    const cookie = (loginRes.headers.get('set-cookie') ?? '').split(';', 1)[0] ?? ''
    const loginPayload = await loginRes.json() as { csrfToken: string; afdianSyncConfigured: boolean }
    expect(loginPayload.afdianSyncConfigured).toBe(true)

    const expectedCounts = {
      total: 5,
      published: 1,
      pending_review: 1,
      below_threshold: 1,
      hidden: 1,
      private_donor: 1,
    }

    const fetchAdmin = async (query: string): Promise<AdminDonationsPage> => {
      const res = await handleWorkerFetch(
        new Request(`${origin}/v1/admin/donations${query}`, { headers: { Cookie: cookie } }),
        env,
        { nowSec: () => now + 20 },
      )
      expect(res.status).toBe(200)
      return res.json() as Promise<AdminDonationsPage>
    }

    const pendingPage = await fetchAdmin('?visibility=pending_review')
    expect(pendingPage.items.map(item => item.externalId)).toEqual(['KOFI-URL-1'])
    expect(pendingPage.counts).toEqual(expectedCounts)

    const afdianPage = await fetchAdmin('?provider=afdian')
    expect(afdianPage.items.map(item => item.externalId)).toEqual(['AF-HIDE', 'AF-100_1'])
    expect(afdianPage.counts).toEqual(expectedCounts)

    // Literal '%' and '_' characters must not behave as SQL LIKE wildcards.
    const percentSearch = await fetchAdmin(`?q=${encodeURIComponent('100%')}`)
    expect(percentSearch.items.map(item => item.externalId)).toEqual(['KOFI-URL-1'])

    const underscoreSearch = await fetchAdmin(`?q=${encodeURIComponent('100_1')}`)
    expect(underscoreSearch.items.map(item => item.externalId)).toEqual(['AF-100_1'])

    // Approving updates counts atomically in the response.
    const approveRes = await handleWorkerFetch(
      new Request(`${origin}/v1/admin/donations/${flagged.record.id}/visibility`, {
        method: 'POST',
        headers: {
          Origin: origin,
          Cookie: cookie,
          'X-CSRF-Token': loginPayload.csrfToken,
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({ action: 'approve' }),
      }),
      env,
      { nowSec: () => now + 30 },
    )
    expect(approveRes.status).toBe(200)
    const approved = await approveRes.json() as { counts: typeof expectedCounts }
    expect(approved.counts).toEqual({
      ...expectedCounts,
      pending_review: 0,
      published: 2,
    })
  })

  it('converts major-unit currency inputs to exact minor integers without float drift', () => {
    expect(parseMajorTwoDecimalToMinor('0')).toBe(0)
    expect(parseMajorTwoDecimalToMinor('0.00')).toBe(0)
    expect(parseMajorTwoDecimalToMinor('10')).toBe(1000)
    expect(parseMajorTwoDecimalToMinor('19.99')).toBe(1999)
    expect(parseMajorTwoDecimalToMinor('5.5')).toBe(550)
    expect(parseMajorTwoDecimalToMinor('100000.00')).toBe(10_000_000)
    expect(parseMajorTwoDecimalToMinor('100000.01')).toBeNull()
    expect(parseMajorTwoDecimalToMinor('-5')).toBeNull()
    expect(parseMajorTwoDecimalToMinor('1.234')).toBeNull()
    expect(parseMajorTwoDecimalToMinor('abc')).toBeNull()

    expect(formatDonationMoney({ amountMinor: 1000, currency: 'CNY', minorDigits: 2 })).toBe('¥10.00')
    expect(formatDonationMoney({ amountMinor: 500, currency: 'USD', minorDigits: 2 })).toBe('$5.00')
    expect(formatDonationMoney({ amountMinor: 500, currency: 'JPY', minorDigits: 0 })).toBe('¥500 JPY')
    expect(formatDonationMoney({ amountMinor: 1234, currency: 'CAD', minorDigits: 2 })).toBe('CAD 12.34')
  })
})
