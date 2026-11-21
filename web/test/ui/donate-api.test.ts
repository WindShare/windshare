import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  DonateApiError,
  createCryptoIntent,
  fetchDonationWall,
  parseCryptoIntentResponse,
  parsePublicWallResponse,
} from '../../src/ui/donate/api'

describe('donate API request signal composition', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('aborts createCryptoIntent when either the caller signal or timeout fires', async () => {
    const timeoutController = new AbortController()
    const timeoutSpy = vi.spyOn(AbortSignal, 'timeout').mockReturnValue(timeoutController.signal)
    let capturedSignal: AbortSignal | undefined
    vi.spyOn(globalThis, 'fetch').mockImplementation((_input, init) => {
      capturedSignal = init?.signal ?? undefined
      return new Promise<Response>((_resolve, reject) => {
        capturedSignal?.addEventListener('abort', () => {
          reject(capturedSignal?.reason ?? new DOMException('Aborted', 'AbortError'))
        }, { once: true })
      })
    })

    const caller = new AbortController()
    const pending = createCryptoIntent(
      { displayName: null, message: null, amountUsd: '10' },
      caller.signal,
    )
    expect(timeoutSpy).toHaveBeenCalledWith(10_000)
    expect(capturedSignal).toBeDefined()
    expect(capturedSignal?.aborted).toBe(false)

    // Firing the timeout aborts the combined fetch signal without marking the caller cancelled.
    timeoutController.abort(new DOMException('Timed out', 'TimeoutError'))
    await expect(pending).rejects.toThrow('Timed out')
    expect(capturedSignal?.aborted).toBe(true)
    expect(caller.signal.aborted).toBe(false)
  })

  it('aborts createCryptoIntent and fetchDonationWall immediately when caller cancels', async () => {
    const timeoutController = new AbortController()
    vi.spyOn(AbortSignal, 'timeout').mockReturnValue(timeoutController.signal)
    const capturedSignals: Array<AbortSignal | undefined> = []
    vi.spyOn(globalThis, 'fetch').mockImplementation((_input, init) => {
      const signal = init?.signal ?? undefined
      capturedSignals.push(signal)
      return new Promise<Response>((_resolve, reject) => {
        signal?.addEventListener('abort', () => {
          reject(signal.reason ?? new DOMException('Aborted', 'AbortError'))
        }, { once: true })
      })
    })

    const intentCaller = new AbortController()
    const intentPending = createCryptoIntent(
      { displayName: 'Alice', message: 'Thanks', amountUsd: '25' },
      intentCaller.signal,
    )
    intentCaller.abort(new DOMException('Unmounted', 'AbortError'))
    await expect(intentPending).rejects.toThrow('Unmounted')
    expect(capturedSignals[0]?.aborted).toBe(true)

    const wallCaller = new AbortController()
    const wallPending = fetchDonationWall({ signal: wallCaller.signal })
    wallCaller.abort(new DOMException('Cancelled', 'AbortError'))
    await expect(wallPending).rejects.toThrow('Cancelled')
    expect(capturedSignals[1]?.aborted).toBe(true)
  })
})

describe('donate crypto intent response validation', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('accepts valid HTTPS checkout URLs without coupling to a single provider hostname', () => {
    expect(
      parseCryptoIntentResponse({
        intentId: 'intent-1',
        invoiceUrl: 'https://nowpayments.io/payment/?iid=123',
      }),
    ).toEqual({
      intentId: 'intent-1',
      invoiceUrl: 'https://nowpayments.io/payment/?iid=123',
    })

    expect(
      parseCryptoIntentResponse({
        intentId: 'intent-2',
        invoiceUrl: 'https://checkout.example/invoice/456',
      }),
    ).toEqual({
      intentId: 'intent-2',
      invoiceUrl: 'https://checkout.example/invoice/456',
    })
  })

  it('rejects missing fields, javascript: URLs, non-HTTPS schemes, credentials, and relative URLs', () => {
    const insecureHttpUrl = ['http', '//nowpayments.io/payment/?iid=1'].join(':')
    for (const malformed of [
      null,
      [],
      'not-an-object',
      {},
      { intentId: '   ', invoiceUrl: 'https://nowpayments.io/payment/?iid=1' },
      { intentId: 'intent-1' },
      { intentId: 'intent-1', invoiceUrl: '' },
      { intentId: 'intent-1', invoiceUrl: '/undefined' },
      { intentId: 'intent-1', invoiceUrl: 'javascript:alert(1)' },
      { intentId: 'intent-1', invoiceUrl: 'data:text/html,<script>alert(1)</script>' },
      { intentId: 'intent-1', invoiceUrl: insecureHttpUrl },
      { intentId: 'intent-1', invoiceUrl: 'https://user:pass@nowpayments.io/payment/?iid=1' },
    ]) {
      expect(parseCryptoIntentResponse(malformed)).toBeNull()
    }
  })

  it('throws DonateApiError(invalid_response) when createCryptoIntent receives unsafe or malformed 2xx payloads', async () => {
    const payload = { displayName: null, message: null, amountUsd: '10' }

    vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(
      Response.json({ intentId: 'intent-1', invoiceUrl: 'javascript:alert(1)' }, { status: 201 }),
    )
    await expect(createCryptoIntent(payload)).rejects.toMatchObject({
      name: 'DonateApiError',
      status: 201,
      code: 'invalid_response',
    })

    vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(
      new Response('<html>proxy error</html>', { status: 200 }),
    )
    await expect(createCryptoIntent(payload)).rejects.toBeInstanceOf(DonateApiError)

    vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(
      Response.json({
        intentId: 'intent-ok',
        invoiceUrl: 'https://nowpayments.io/payment/?iid=ok',
      }, { status: 201 }),
    )
    await expect(createCryptoIntent(payload)).resolves.toEqual({
      intentId: 'intent-ok',
      invoiceUrl: 'https://nowpayments.io/payment/?iid=ok',
    })
  })
})

describe('donate public wall response validation', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  const validEntry = {
    id: 'don_1',
    provider: 'afdian',
    displayName: 'Alice',
    amountMinor: 1000,
    currency: 'CNY',
    minorDigits: 2,
    message: 'Keep going!',
    donatedAt: 1_700_000_000,
  }

  it('accepts valid wall payloads including forward-compatible unknown provider names', () => {
    const wall = {
      version: 3,
      moderationEnabled: false,
      entries: [
        validEntry,
        {
          ...validEntry,
          id: 'don_2',
          provider: 'github_sponsors',
          displayName: null,
          message: null,
        },
      ],
    }

    expect(parsePublicWallResponse(wall)).toEqual(wall)
  })

  it('rejects malformed envelopes, invalid entry fields, and out-of-range Date timestamps', () => {
    const outOfDateRangeSeconds = 10_000_000_000_000
    for (const malformed of [
      null,
      [],
      'not-an-object',
      {},
      { version: 1, moderationEnabled: false },
      { version: '1', moderationEnabled: false, entries: [] },
      { version: Number.NaN, moderationEnabled: false, entries: [] },
      { version: 1, moderationEnabled: 'false', entries: [] },
      { version: 1, moderationEnabled: false, entries: [null] },
      { version: 1, moderationEnabled: false, entries: [{ ...validEntry, id: '' }] },
      { version: 1, moderationEnabled: false, entries: [{ ...validEntry, provider: '   ' }] },
      { version: 1, moderationEnabled: false, entries: [{ ...validEntry, displayName: 123 }] },
      { version: 1, moderationEnabled: false, entries: [{ ...validEntry, amountMinor: -1 }] },
      { version: 1, moderationEnabled: false, entries: [{ ...validEntry, amountMinor: Number.NaN }] },
      { version: 1, moderationEnabled: false, entries: [{ ...validEntry, currency: '' }] },
      { version: 1, moderationEnabled: false, entries: [{ ...validEntry, minorDigits: Number.POSITIVE_INFINITY }] },
      { version: 1, moderationEnabled: false, entries: [{ ...validEntry, message: 42 }] },
      { version: 1, moderationEnabled: false, entries: [{ ...validEntry, donatedAt: '1700000000' }] },
      { version: 1, moderationEnabled: false, entries: [{ ...validEntry, donatedAt: Number.NaN }] },
      { version: 1, moderationEnabled: false, entries: [{ ...validEntry, donatedAt: Number.POSITIVE_INFINITY }] },
      { version: 1, moderationEnabled: false, entries: [{ ...validEntry, donatedAt: outOfDateRangeSeconds }] },
    ]) {
      expect(parsePublicWallResponse(malformed)).toBeNull()
    }
  })

  it('throws DonateApiError(invalid_response) when fetchDonationWall receives malformed 2xx payloads', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(
      Response.json({ version: 1, moderationEnabled: false, entries: [{ ...validEntry, donatedAt: 1e13 }] }, { status: 200 }),
    )
    await expect(fetchDonationWall()).rejects.toMatchObject({
      name: 'DonateApiError',
      status: 200,
      code: 'invalid_response',
    })

    vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(
      new Response('<html>captive portal</html>', { status: 200 }),
    )
    await expect(fetchDonationWall()).rejects.toBeInstanceOf(DonateApiError)

    const validWall = { version: 2, moderationEnabled: true, entries: [validEntry] }
    vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(Response.json(validWall, { status: 200 }))
    await expect(fetchDonationWall()).resolves.toEqual(validWall)
  })
})
