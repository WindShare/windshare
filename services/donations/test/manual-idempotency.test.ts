import { randomBytes } from 'node:crypto'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { IDEMPOTENCY_KEY_HEADER, parseManualDonationInput } from '../src/admin/manual'
import type { DonationsEnv } from '../src/env'
import {
  MAX_MANUAL_IDEMPOTENCY_KEY_LENGTH,
  type AdminVisibilityCounts,
  type DonationRecord,
} from '../src/model'
import { handleWorkerFetch } from '../src/router'
import { createInMemoryWallStore, createWallNamespaceFromStore } from './wall-harness'

const API_ORIGIN = 'https://donate.windshare.cc'

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

afterEach(() => {
  vi.restoreAllMocks()
})

describe('manual donation Idempotency-Key validation and WallStore invariants', () => {
  it('validates optional Idempotency-Key headers strictly', () => {
    expect(parseManualDonationInput({ amount: '20', currency: 'USD' }, null)).toMatchObject({
      ok: true,
      input: { idempotencyKey: null },
    })
    expect(parseManualDonationInput({ amount: '20', currency: 'USD' }, '  req-uuid:1.0_a  ')).toMatchObject({
      ok: true,
      input: { idempotencyKey: 'req-uuid:1.0_a' },
    })

    for (const invalidKey of [
      '',
      '   ',
      'has space',
      'bad/slash',
      'a'.repeat(MAX_MANUAL_IDEMPOTENCY_KEY_LENGTH + 1),
    ]) {
      expect(parseManualDonationInput({ amount: '20', currency: 'USD' }, invalidKey)).toEqual({
        ok: false,
        code: 'invalid_idempotency_key',
      })
    }
  })

  it('replays blank and explicit externalId retries across clock ticks while preserving distinct donations and conflict detection', () => {
    const store = createInMemoryWallStore()
    const baseInput = {
      provider: 'direct' as const,
      externalId: null,
      displayName: 'Repeat Donor',
      amountMinor: 5000,
      currency: 'USD',
      minorDigits: 2,
      message: 'Keep shipping',
      donatedAt: null,
    }

    const first = store.createManualDonation({ ...baseInput, idempotencyKey: 'key-blank-1' }, 1_700_000_000)
    expect(first.ok).toBe(true)
    if (!first.ok) throw new Error('expected initial manual donation')
    expect(first.replayed).toBe(false)
    const versionAfterFirst = store.getPublicWallSnapshot().version

    // Retry later with same key and blank externalId/donatedAt replays original row without bumping wall_version.
    const replay = store.createManualDonation({ ...baseInput, idempotencyKey: 'key-blank-1' }, 1_700_000_045)
    expect(replay).toEqual({
      ok: true,
      record: first.record,
      counts: first.counts,
      replayed: true,
    })
    expect(store.getPublicWallSnapshot().version).toBe(versionAfterFirst)
    expect(store.listAdminDonations(null, 10).items).toHaveLength(1)

    // Same key with any changed business field returns conflict without creating a duplicate.
    const conflict = store.createManualDonation(
      { ...baseInput, amountMinor: 6000, idempotencyKey: 'key-blank-1' },
      1_700_000_050,
    )
    expect(conflict).toEqual({
      ok: false,
      code: 'idempotency_key_conflict',
      existingDonationId: first.record.id,
    })
    expect(store.listAdminDonations(null, 10).items).toHaveLength(1)

    // A new key with identical amount and fields creates a second distinct record.
    const secondDistinct = store.createManualDonation(
      { ...baseInput, idempotencyKey: 'key-blank-2' },
      1_700_000_060,
    )
    expect(secondDistinct.ok).toBe(true)
    if (!secondDistinct.ok) throw new Error('expected distinct manual donation')
    expect(secondDistinct.replayed).toBe(false)
    expect(secondDistinct.record.id).not.toBe(first.record.id)
    expect(secondDistinct.record.externalId).not.toBe(first.record.externalId)

    // Explicit externalId replays cleanly with the same key, while a different key still hits duplicate_external_id.
    const explicitInput = { ...baseInput, externalId: 'TX-EXPLICIT-1' }
    const explicitFirst = store.createManualDonation(
      { ...explicitInput, idempotencyKey: 'key-explicit-1' },
      1_700_000_070,
    )
    expect(explicitFirst.ok).toBe(true)
    if (!explicitFirst.ok) throw new Error('expected explicit manual donation')

    const explicitReplay = store.createManualDonation(
      { ...explicitInput, idempotencyKey: 'key-explicit-1' },
      1_700_000_080,
    )
    expect(explicitReplay).toEqual({
      ok: true,
      record: explicitFirst.record,
      counts: explicitFirst.counts,
      replayed: true,
    })

    const dupWithNewKey = store.createManualDonation(
      { ...explicitInput, idempotencyKey: 'key-explicit-2' },
      1_700_000_090,
    )
    expect(dupWithNewKey).toEqual({ ok: false, code: 'duplicate_external_id' })

    // A rejected duplicate_external_id must not burn key-explicit-2; fixing the externalId in the same dialog succeeds.
    const fixedRetry = store.createManualDonation(
      { ...baseInput, externalId: 'TX-EXPLICIT-2', idempotencyKey: 'key-explicit-2' },
      1_700_000_100,
    )
    expect(fixedRetry.ok).toBe(true)
    if (!fixedRetry.ok) throw new Error('expected fixed retry to succeed')
    expect(fixedRetry.replayed).toBe(false)
    expect(fixedRetry.record.externalId).toBe('TX-EXPLICIT-2')
  })
})

describe('POST /v1/admin/donations idempotency HTTP boundary and observability', () => {
  it('returns 201 on create, 200 on replay, 409 on conflict, 400 on malformed key, and logs structured events', async () => {
    const logs = captureStructuredLogs()
    const store = createInMemoryWallStore()
    const adminPassword = randomBytes(16).toString('hex')
    let now = 1_700_000_000
    const env: DonationsEnv = {
      DONATION_WALL: createWallNamespaceFromStore(store),
      AFDIAN_USER_ID: 'creator',
      ALLOWED_ORIGINS: 'https://windshare.cc',
      DONATE_API_ORIGIN: API_ORIGIN,
      DONATE_ADMIN_PASSWORD: adminPassword,
    }

    const loginRes = await handleWorkerFetch(
      new Request(`${API_ORIGIN}/v1/admin/login`, {
        method: 'POST',
        headers: { Origin: API_ORIGIN, 'Content-Type': 'application/json' },
        body: JSON.stringify({ password: adminPassword }),
      }),
      env,
      { nowSec: () => now },
    )
    expect(loginRes.status).toBe(200)
    const cookie = (loginRes.headers.get('set-cookie') ?? '').split(';', 1)[0] ?? ''
    const { csrfToken } = await loginRes.json() as { csrfToken: string }

    const postManual = (body: Record<string, unknown>, idempotencyKey?: string) => handleWorkerFetch(
      new Request(`${API_ORIGIN}/v1/admin/donations`, {
        method: 'POST',
        headers: {
          Origin: API_ORIGIN,
          Cookie: cookie,
          'X-CSRF-Token': csrfToken,
          'Content-Type': 'application/json',
          ...(idempotencyKey === undefined ? {} : { [IDEMPOTENCY_KEY_HEADER]: idempotencyKey }),
        },
        body: JSON.stringify(body),
      }),
      env,
      { nowSec: () => now },
    )

    const malformedRes = await postManual({ amount: '15', currency: 'USD' }, 'invalid key!')
    expect(malformedRes.status).toBe(400)
    expect(await malformedRes.json()).toEqual({ error: 'invalid_idempotency_key' })

    const key = '550e8400-e29b-41d4-a716-446655440000'
    const payload = { amount: '15', currency: 'USD', displayName: 'Alice', message: 'Thanks' }

    const createRes = await postManual(payload, key)
    expect(createRes.status).toBe(201)
    const created = await createRes.json() as { record: DonationRecord; counts: AdminVisibilityCounts }

    now += 12
    const replayRes = await postManual(payload, key)
    expect(replayRes.status).toBe(200)
    expect(await replayRes.json()).toEqual(created)

    const conflictRes = await postManual({ ...payload, message: 'Edited after drop' }, key)
    expect(conflictRes.status).toBe(409)
    expect(await conflictRes.json()).toEqual({ error: 'idempotency_key_conflict' })

    expect(logs).toContainEqual({
      level: 'info',
      payload: {
        event: 'donation.admin_manual_created',
        donation_id: created.record.id,
        provider: 'direct',
        external_id: created.record.externalId,
        currency: 'USD',
        amount_minor: 1500,
        idempotency_key: key,
      },
    })
    expect(logs).toContainEqual({
      level: 'info',
      payload: {
        event: 'donation.admin_manual_replayed',
        donation_id: created.record.id,
        provider: 'direct',
        external_id: created.record.externalId,
        idempotency_key: key,
      },
    })
    expect(logs).toContainEqual({
      level: 'warn',
      payload: {
        event: 'donation.admin_manual_idempotency_conflict',
        idempotency_key: key,
        existing_donation_id: created.record.id,
      },
    })
  })
})
