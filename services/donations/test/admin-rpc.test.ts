import { randomBytes } from 'node:crypto'
import { describe, expect, it, vi } from 'vitest'
import type { DonationsEnv } from '../src/env'
import { handleWorkerFetch } from '../src/router'
import {
  createInMemoryWallStore,
  createWallNamespaceFromStore,
  type AsyncWallStub,
} from './wall-harness'

describe('Admin DO RPC fail-closed error boundary', () => {
  it('fails closed with 503 and structured error logs whenever admin DO RPC calls reject', async () => {
    const store = createInMemoryWallStore()
    const now = 1_700_000_000
    const origin = 'https://donate.windshare.cc'
    const adminPassword = randomBytes(16).toString('hex')
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})

    const makeEnv = (overrides?: Partial<AsyncWallStub>): DonationsEnv => ({
      DONATION_WALL: createWallNamespaceFromStore(store, overrides === undefined ? undefined : { overrides }),
      AFDIAN_USER_ID: 'creator',
      ALLOWED_ORIGINS: 'https://windshare.cc',
      DONATE_API_ORIGIN: origin,
      DONATE_ADMIN_PASSWORD: adminPassword,
    })

    const postLogin = (env: DonationsEnv) => handleWorkerFetch(
      new Request(`${origin}/v1/admin/login`, {
        method: 'POST',
        headers: {
          Origin: origin,
          'Content-Type': 'application/json',
          'CF-Connecting-IP': '198.51.100.88',
        },
        body: JSON.stringify({ password: adminPassword }),
      }),
      env,
      { nowSec: () => now },
    )

    // 1. Rate-limit RPC failure fails closed (never checks password or creates session).
    let sessionCreateCalls = 0
    const overloadedError = Object.assign(new Error('Durable Object overloaded'), { overloaded: true })
    const rateLimitFail = await postLogin(makeEnv({
      consumeAdminLoginRateLimit: async () => {
        throw overloadedError
      },
      createAdminSession: async (input) => {
        sessionCreateCalls += 1
        return store.createAdminSession(input)
      },
    }))
    expect(rateLimitFail.status).toBe(503)
    expect(rateLimitFail.headers.get('cache-control')).toBe('no-store')
    expect(rateLimitFail.headers.get('set-cookie')).toBeNull()
    expect(await rateLimitFail.json()).toEqual({ error: 'service_unavailable' })
    expect(sessionCreateCalls).toBe(0)

    // 2. Session creation RPC failure never returns 200 or emits an unpersisted Set-Cookie.
    const createFail = await postLogin(makeEnv({
      createAdminSession: async () => {
        throw Object.assign(new Error('sqlite write failed'), { retryable: true })
      },
    }))
    expect(createFail.status).toBe(503)
    expect(createFail.headers.get('set-cookie')).toBeNull()
    expect(await createFail.json()).toEqual({ error: 'service_unavailable' })

    // Establish a real session for authenticated route failure checks.
    const okLogin = await postLogin(makeEnv())
    expect(okLogin.status).toBe(200)
    const cookie = (okLogin.headers.get('set-cookie') ?? '').split(';', 1)[0] ?? ''
    const { csrfToken } = await okLogin.json() as { csrfToken: string }

    // 3. Session verify RPC failure returns 503 instead of misreporting 401 unauthorized.
    const verifyFail = await handleWorkerFetch(
      new Request(`${origin}/v1/admin/session`, { headers: { Cookie: cookie } }),
      makeEnv({
        verifyAdminSession: async () => {
          throw new Error('storage timeout')
        },
      }),
      { nowSec: () => now + 10 },
    )
    expect(verifyFail.status).toBe(503)
    expect(await verifyFail.json()).toEqual({ error: 'service_unavailable' })

    // 4. Config read RPC failure returns 503 instead of 200 with config: undefined.
    const configFail = await handleWorkerFetch(
      new Request(`${origin}/v1/admin/session`, { headers: { Cookie: cookie } }),
      makeEnv({
        getAdminConfig: async () => {
          throw new Error('config read failed')
        },
      }),
      { nowSec: () => now + 10 },
    )
    expect(configFail.status).toBe(503)
    expect(await configFail.json()).toEqual({ error: 'service_unavailable' })

    // 5. Logout revoke RPC failure returns 503 instead of clearing cookie while session remains active.
    const revokeFail = await handleWorkerFetch(
      new Request(`${origin}/v1/admin/logout`, {
        method: 'POST',
        headers: { Origin: origin, Cookie: cookie, 'X-CSRF-Token': csrfToken },
      }),
      makeEnv({
        revokeAdminSession: async () => {
          throw new Error('revoke failed')
        },
      }),
      { nowSec: () => now + 10 },
    )
    expect(revokeFail.status).toBe(503)
    expect(revokeFail.headers.get('set-cookie')).toBeNull()
    expect(await revokeFail.json()).toEqual({ error: 'service_unavailable' })

    expect(errorSpy).toHaveBeenCalledTimes(5)
    const firstLog = JSON.parse(String(errorSpy.mock.calls[0]?.[0])) as Record<string, unknown>
    expect(firstLog).toMatchObject({
      event: 'donation.admin_storage_error',
      method: 'POST',
      pathname: '/v1/admin/login',
      overloaded: true,
      retryable: false,
    })
  })
})
