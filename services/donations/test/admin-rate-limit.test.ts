import { randomBytes } from 'node:crypto'
import { describe, expect, it } from 'vitest'
import {
  MIN_ADMIN_PASSWORD_LENGTH,
  resolveAdminPassword,
  type DonationsEnv,
} from '../src/env'
import { getClientIp, normalizeClientIp } from '../src/http'
import { handleWorkerFetch } from '../src/router'
import {
  createInMemoryWallStoreWithDb,
  createWallNamespaceFromStore,
} from './wall-harness'

const API_ORIGIN = 'https://donate.windshare.cc'
const NOW_SEC = 1_700_000_000

function createAdminEnv(
  store: ReturnType<typeof createInMemoryWallStoreWithDb>['store'],
  adminPassword: string | undefined,
  onCall?: () => void,
): DonationsEnv {
  return {
    DONATION_WALL: createWallNamespaceFromStore(store, onCall === undefined ? undefined : { onCall }),
    AFDIAN_USER_ID: 'creator',
    ALLOWED_ORIGINS: 'https://windshare.cc',
    DONATE_API_ORIGIN: API_ORIGIN,
    ...(adminPassword === undefined ? {} : { DONATE_ADMIN_PASSWORD: adminPassword }),
  }
}

function loginRequest(password: string, clientIp: string): Request {
  return new Request(`${API_ORIGIN}/v1/admin/login`, {
    method: 'POST',
    headers: {
      Origin: API_ORIGIN,
      'Content-Type': 'application/json',
      'CF-Connecting-IP': clientIp,
    },
    body: JSON.stringify({ password }),
  })
}

// RFC 3849 (2001:db8::/32) and RFC 5737 (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24)
// documentation addresses are used as deterministic IP-normalization test vectors.
/* eslint-disable sonarjs/no-hardcoded-ip */
describe('client IP normalization for rate-limit bucketing', () => {
  it('preserves IPv4, buckets IPv6 by /64 prefix, and unwraps IPv4-mapped IPv6 addresses', () => {
    expect(normalizeClientIp('203.0.113.42')).toBe('203.0.113.42')
    expect(normalizeClientIp('   ')).toBe('unknown')

    // Distinct host IDs and formatting variants inside the same /64 collapse to one bucket key.
    expect(normalizeClientIp('2001:db8:1:2::1')).toBe('2001:0db8:0001:0002::/64')
    expect(normalizeClientIp('2001:0DB8:0001:0002:abcd:ef01:2345:6789')).toBe('2001:0db8:0001:0002::/64')
    expect(normalizeClientIp('2001:db8:1:2::ffff%eth0')).toBe('2001:0db8:0001:0002::/64')
    expect(normalizeClientIp('2001:db8:1:3::1')).toBe('2001:0db8:0001:0003::/64')
    expect(normalizeClientIp('::1')).toBe('0000:0000:0000:0000::/64')
    expect(normalizeClientIp('2001:db8::')).toBe('2001:0db8:0000:0000::/64')

    // IPv4-mapped IPv6 addresses map back to their IPv4 address instead of sharing ::/64.
    expect(normalizeClientIp('::ffff:192.0.2.128')).toBe('192.0.2.128')
    expect(normalizeClientIp('0:0:0:0:0:FFFF:c000:0280')).toBe('192.0.2.128')

    // Malformed strings fall back deterministically without throwing.
    expect(normalizeClientIp('2001:db8::1::2')).toBe('2001:db8::1::2')
    expect(normalizeClientIp('1.2.3.4::1')).toBe('1.2.3.4::1')

    expect(getClientIp(new Request(API_ORIGIN))).toBe('unknown')
    expect(getClientIp(new Request(API_ORIGIN, {
      headers: { 'CF-Connecting-IP': '2001:db8:a:b::99' },
    }))).toBe('2001:0db8:000a:000b::/64')
  })
})
/* eslint-enable sonarjs/no-hardcoded-ip */

describe('admin login per-IP isolation and high-entropy secret enforcement', () => {
  it('blocks rotating IPv6 host bits within a /64 while never locking out clean operator IPs', async () => {
    const { store, db } = createInMemoryWallStoreWithDb()
    const adminPassword = randomBytes(24).toString('base64url')
    const env = createAdminEnv(store, adminPassword)

    // Rotating host bits inside one IPv6 /64 exhausts the shared 5-attempt /64 bucket.
    for (let i = 1; i <= 5; i += 1) {
      const res = await handleWorkerFetch(
        loginRequest('wrong-password', `2001:db8:dead:beef::${i}`),
        env,
        { nowSec: () => NOW_SEC },
      )
      expect(res.status).toBe(401)
    }
    const blockedIpv6 = await handleWorkerFetch(
      loginRequest(adminPassword, '2001:db8:dead:beef:1234:5678:9abc:def0'),
      env,
      { nowSec: () => NOW_SEC },
    )
    expect(blockedIpv6.status).toBe(429)

    // Distributed bad logins across many distinct IPs cannot lock out the legitimate operator.
    for (let ip = 1; ip <= 25; ip += 1) {
      const res = await handleWorkerFetch(
        loginRequest('wrong-password', `198.51.100.${ip}`),
        env,
        { nowSec: () => NOW_SEC },
      )
      expect(res.status).toBe(401)
    }

    const operatorLogin = await handleWorkerFetch(
      loginRequest(adminPassword, '203.0.113.9'),
      env,
      { nowSec: () => NOW_SEC },
    )
    expect(operatorLogin.status).toBe(200)

    // Verify no admin_login:global row is ever persisted in SQLite.
    const globalRows = db.prepare(
      "SELECT bucket_key FROM rate_limits WHERE bucket_key = 'admin_login:global'",
    ).all()
    expect(globalRows).toHaveLength(0)
  })

  it('retains both per-IP and global caps for crypto checkout intents', () => {
    const { store } = createInMemoryWallStoreWithDb()
    for (let ip = 0; ip < 12; ip += 1) {
      for (let attempt = 0; attempt < 10; attempt += 1) {
        expect(store.consumeRateLimit('crypto', `crypto-ip-${ip}`, NOW_SEC).allowed).toBe(true)
      }
      expect(store.consumeRateLimit('crypto', `crypto-ip-${ip}`, NOW_SEC).allowed).toBe(false)
    }
    const globalBlocked = store.consumeRateLimit('crypto', 'crypto-ip-overflow', NOW_SEC)
    expect(globalBlocked.allowed).toBe(false)
    expect(globalBlocked.retryAfterSeconds).toBeGreaterThan(0)
  })

  it('rejects short or whitespace-padded DONATE_ADMIN_PASSWORD fail-closed before touching the DO', async () => {
    const { store } = createInMemoryWallStoreWithDb()
    let doCalls = 0

    for (const badSecret of [
      undefined,
      '',
      'admin123',
      'a'.repeat(MIN_ADMIN_PASSWORD_LENGTH - 1),
      `   ${'a'.repeat(MIN_ADMIN_PASSWORD_LENGTH - 1)}   `,
      ' '.repeat(MIN_ADMIN_PASSWORD_LENGTH + 8),
    ]) {
      const env = createAdminEnv(store, badSecret, () => {
        doCalls += 1
      })
      const resolution = resolveAdminPassword(env)
      expect(resolution.ok).toBe(false)

      const loginRes = await handleWorkerFetch(
        loginRequest(badSecret ?? 'anything', '203.0.113.10'),
        env,
        { nowSec: () => NOW_SEC },
      )
      expect(loginRes.status).toBe(503)
      expect(await loginRes.json()).toEqual({ error: 'admin_not_configured' })
    }
    expect(doCalls).toBe(0)

    const boundarySecret = randomBytes(16).toString('base64')
    expect(boundarySecret).toHaveLength(MIN_ADMIN_PASSWORD_LENGTH)
    const okEnv = createAdminEnv(store, boundarySecret)
    expect(resolveAdminPassword(okEnv)).toEqual({ ok: true, password: boundarySecret })
    const okRes = await handleWorkerFetch(
      loginRequest(boundarySecret, '203.0.113.10'),
      okEnv,
      { nowSec: () => NOW_SEC },
    )
    expect(okRes.status).toBe(200)
  })
})
