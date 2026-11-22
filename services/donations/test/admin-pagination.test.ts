import { randomBytes } from 'node:crypto'
import { describe, expect, it } from 'vitest'
import type { DonationsEnv } from '../src/env'
import { decodeAdminCursor, encodeAdminCursor, type DonationRecord } from '../src/model'
import { handleWorkerFetch } from '../src/router'
import { createInMemoryWallStore, createWallNamespaceFromStore } from './wall-harness'

describe('admin donations cursor validation and pagination', () => {
  it('round-trips valid cursors and rejects malformed cursor strings', () => {
    const encoded = encodeAdminCursor({ donatedAt: 1_760_000_200, id: 'abc_123-XYZ' })
    expect(encoded).toBe('1760000200:abc_123-XYZ')
    expect(decodeAdminCursor(encoded)).toEqual({ donatedAt: 1_760_000_200, id: 'abc_123-XYZ' })
    expect(decodeAdminCursor(`  ${encoded}  `)).toEqual({ donatedAt: 1_760_000_200, id: 'abc_123-XYZ' })

    for (const invalid of [
      null,
      '',
      '   ',
      'not-a-cursor',
      '-1:abc',
      '1.5:abc',
      '9007199254740992:abc',
      '1700000000:',
      '1700000000:bad/id',
    ]) {
      expect(decodeAdminCursor(invalid)).toBeNull()
    }
  })

  it('rejects malformed after cursors with 400 before invoking DO RPC while paginating valid cursors', async () => {
    const store = createInMemoryWallStore()
    const now = 1_700_000_000
    for (let i = 1; i <= 3; i += 1) {
      store.commitDonation({
        provider: 'kofi',
        externalId: `cur-${i}`,
        displayName: `Donor ${i}`,
        amountMinor: 500,
        currency: 'USD',
        minorDigits: 2,
        message: null,
        isPublic: true,
        donatedAt: now + i,
      }, now + i)
    }

    let listRpcCalls = 0
    const origin = 'https://donate.windshare.cc'
    const adminPassword = randomBytes(16).toString('hex')
    const env: DonationsEnv = {
      DONATION_WALL: createWallNamespaceFromStore(store, {
        overrides: {
          listAdminDonations: async (afterCursor, rawLimit) => {
            listRpcCalls += 1
            return store.listAdminDonations(afterCursor, rawLimit)
          },
        },
      }),
      AFDIAN_USER_ID: 'creator',
      ALLOWED_ORIGINS: 'https://windshare.cc',
      DONATE_API_ORIGIN: origin,
      DONATE_ADMIN_PASSWORD: adminPassword,
    }
    const loginRes = await handleWorkerFetch(
      new Request(`${origin}/v1/admin/login`, {
        method: 'POST',
        headers: { Origin: origin, 'Content-Type': 'application/json', 'CF-Connecting-IP': '198.51.100.89' },
        body: JSON.stringify({ password: adminPassword }),
      }),
      env,
      { nowSec: () => now },
    )
    expect(loginRes.status).toBe(200)
    const cookie = (loginRes.headers.get('set-cookie') ?? '').split(';', 1)[0] ?? ''

    const fetchRaw = (query: string) => handleWorkerFetch(
      new Request(`${origin}/v1/admin/donations${query}`, { headers: { Cookie: cookie } }),
      env,
      { nowSec: () => now + 10 },
    )

    for (const badQuery of [
      '?after=',
      '?after=%20%20',
      '?after=not-a-cursor',
      '?after=-1:abc',
      '?after=1.5:abc',
      '?after=9007199254740992:abc',
      '?after=1700000000:bad/id',
    ]) {
      const res = await fetchRaw(badQuery)
      expect(res.status).toBe(400)
      expect(await res.json()).toEqual({ error: 'invalid_cursor' })
    }
    expect(listRpcCalls).toBe(0)

    const page1Res = await fetchRaw('?limit=2')
    expect(page1Res.status).toBe(200)
    const page1 = await page1Res.json() as { items: DonationRecord[]; nextCursor: string | null }
    expect(page1.items).toHaveLength(2)
    expect(page1.nextCursor).not.toBeNull()

    const page2Res = await fetchRaw(`?limit=2&after=${encodeURIComponent(page1.nextCursor ?? '')}`)
    expect(page2Res.status).toBe(200)
    const page2 = await page2Res.json() as { items: DonationRecord[]; nextCursor: string | null }
    expect(page2.items).toHaveLength(1)
    expect(page2.nextCursor).toBeNull()
    expect(new Set([...page1.items, ...page2.items].map(item => item.id)).size).toBe(3)
    expect(listRpcCalls).toBe(2)
  })
})
