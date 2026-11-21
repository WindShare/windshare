import { describe, expect, it } from 'vitest'
import { parseAllowedOrigins, type DonationsEnv } from '../src/env'
import { handleWorkerFetch } from '../src/router'
import { parseFixedPointAmount, sanitizePlainText } from '../src/sanitize'
import { constantTimeEqual, hmacSha512Hex, md5Hex } from '../src/verify'

function createStubEnv(): DonationsEnv {
  const namespace = {
    idFromName: (name: string) => ({ toString: () => name }),
    get: () => ({
      fetch: async () => Response.json(
        { version: 0, moderationEnabled: false, entries: [] },
        { headers: { ETag: '"wall-v0"' } },
      ),
    }),
  } as unknown as DurableObjectNamespace

  return {
    DONATION_WALL: namespace,
    AFDIAN_USER_ID: 'test-user',
    KOFI_HANDLE: 'windshare',
    ALLOWED_ORIGINS: 'https://ws.ourworld.top, https://windshare.cc, invalid-origin',
    DONATE_API_ORIGIN: 'https://donate.windshare.cc',
  }
}

describe('donations worker scaffold', () => {
  it('parses valid allowed origins and ignores malformed entries', () => {
    const origins = parseAllowedOrigins('https://ws.ourworld.top/, https://windshare.cc, not-a-url')
    expect([...origins]).toEqual(['https://ws.ourworld.top', 'https://windshare.cc'])
  })

  it('sanitizes plain text by stripping control characters and enforcing code-point limits', () => {
    expect(sanitizePlainText('  hello\u0000\u200B\nworld  ', 32)).toBe('hello world')
    expect(sanitizePlainText('😀😀😀😀', 3)).toBe('😀😀😀')
    expect(sanitizePlainText('   \n\t  ', 32)).toBeNull()
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
    expect(parseFixedPointAmount('12.345', 'CAD')).toEqual({
      amountMinor: 12345,
      currency: 'CAD',
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
    expect(wall.headers.get('etag')).toBe('"wall-v0"')
    expect(await wall.json()).toEqual({ version: 0, moderationEnabled: false, entries: [] })

    const admin = await handleWorkerFetch(new Request('https://donate.windshare.cc/admin'), env)
    expect(admin.status).toBe(200)
    expect(admin.headers.get('cache-control')).toBe('no-store')
    expect(admin.headers.get('content-security-policy')).toContain("script-src 'self'")

    const missing = await handleWorkerFetch(new Request('https://donate.windshare.cc/unknown'), env)
    expect(missing.status).toBe(404)
  })
})
