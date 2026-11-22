import { randomBytes } from 'node:crypto'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { IDEMPOTENCY_KEY_HEADER } from '../src/admin/manual'
import type { DonationsEnv } from '../src/env'
import type { DonationRecord, PublicWallResponse } from '../src/model'
import { handleWorkerFetch } from '../src/router'
import { createInMemoryWallStore, createWallNamespaceFromStore } from './wall-harness'

const API_ORIGIN = 'https://donate.windshare.cc'
const KOFI_TOKEN = 'test-kofi-verification-token'

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

describe('WallStore private callback precedence over pre-existing records', () => {
  it('atomically redacts manually published Ko-fi entries, refreshes wall/Top 3 caches, and stays idempotent on retries', () => {
    const store = createInMemoryWallStore()
    const now = 1_700_000_000

    // Seed three public donations so a later manual Ko-fi entry displaces #3 from Top 3.
    for (const [id, name, amountMinor, offset] of [
      ['BASE-1', 'Alpha', 10_000, 1],
      ['BASE-2', 'Beta', 8_000, 2],
      ['BASE-3', 'Gamma', 6_000, 3],
    ] as const) {
      store.commitDonation({
        provider: 'kofi',
        externalId: id,
        displayName: name,
        amountMinor,
        currency: 'USD',
        minorDigits: 2,
        message: 'Public thanks',
        isPublic: true,
        donatedAt: now + offset,
      }, now + offset)
    }

    const manualInput = {
      provider: 'kofi' as const,
      externalId: 'KOFI-TX-PRIV',
      displayName: 'Secret Patron',
      amountMinor: 50_000,
      currency: 'USD',
      minorDigits: 2,
      message: 'Please keep this private',
      donatedAt: now + 10,
      idempotencyKey: 'manual-kofi-key-1',
    }
    const manual = store.createManualDonation(manualInput, now + 10)
    expect(manual.ok).toBe(true)
    if (!manual.ok) throw new Error('expected manual donation')
    expect(manual.record.visibility).toEqual({ kind: 'published' })

    const beforeRedact = store.getPublicWallSnapshot()
    const beforeWall = JSON.parse(beforeRedact.json) as PublicWallResponse
    expect(beforeWall.entries.some(e => e.id === manual.record.id)).toBe(true)
    expect(beforeWall.topSupporters.map(e => e.displayName)).toEqual(['Secret Patron', 'Alpha', 'Beta'])

    // Verified private Ko-fi callback overrides the pre-existing published manual record.
    const redacted = store.commitDonation({
      provider: 'kofi',
      externalId: 'KOFI-TX-PRIV',
      displayName: null,
      amountMinor: 50_000,
      currency: 'USD',
      minorDigits: 2,
      message: null,
      isPublic: false,
      donatedAt: now + 10,
    }, now + 20)
    expect(redacted).toEqual({
      outcome: 'redacted_private',
      previousVisibility: 'published',
      record: {
        ...manual.record,
        displayName: null,
        message: null,
        visibility: { kind: 'private_donor' },
      },
    })

    const afterRedact = store.getPublicWallSnapshot()
    expect(afterRedact.version).toBe(beforeRedact.version + 1)
    expect(afterRedact.etag).not.toBe(beforeRedact.etag)
    const afterWall = JSON.parse(afterRedact.json) as PublicWallResponse
    expect(afterWall.entries.some(e => e.id === manual.record.id)).toBe(false)
    expect(afterWall.topSupporters.map(e => e.displayName)).toEqual(['Alpha', 'Beta', 'Gamma'])

    // Admin list & counts reflect private_donor with scrubbed text and immutable status.
    const adminPage = store.listAdminDonations(null, 10)
    expect(adminPage.counts.published).toBe(3)
    expect(adminPage.counts.private_donor).toBe(1)
    expect(adminPage.items.find(item => item.id === manual.record.id)).toEqual(redacted.record)
    expect(store.moderateDonationVisibility(manual.record.id, { action: 'approve' }, now + 21)).toEqual({
      ok: false,
      code: 'private_donor_immutable',
    })

    // Retrying the private callback is strictly idempotent and does not bump wall_version again.
    const retryPrivate = store.commitDonation({
      provider: 'kofi',
      externalId: 'KOFI-TX-PRIV',
      displayName: null,
      amountMinor: 50_000,
      currency: 'USD',
      minorDigits: 2,
      message: null,
      isPublic: false,
      donatedAt: now + 10,
    }, now + 30)
    expect(retryPrivate).toEqual({ outcome: 'deduplicated', record: redacted.record })
    expect(store.getPublicWallSnapshot().version).toBe(afterRedact.version)

    // Replaying the original manual Idempotency-Key returns the redacted private record without reviving text.
    const manualReplay = store.createManualDonation(manualInput, now + 40)
    expect(manualReplay).toEqual({
      ok: true,
      record: redacted.record,
      counts: adminPage.counts,
      replayed: true,
    })
    expect(store.getPublicWallSnapshot().version).toBe(afterRedact.version)
  })

  it('redacts hidden manual entries to immutable private_donor and keeps public duplicates untouched', () => {
    const store = createInMemoryWallStore()
    const now = 1_700_000_000

    const hiddenManual = store.createManualDonation({
      provider: 'kofi',
      externalId: 'KOFI-HIDDEN-1',
      displayName: 'Hidden Secret',
      amountMinor: 1_500,
      currency: 'USD',
      minorDigits: 2,
      message: 'Private text still stored while hidden',
      donatedAt: now,
      idempotencyKey: null,
    }, now)
    expect(hiddenManual.ok).toBe(true)
    if (!hiddenManual.ok) throw new Error('expected manual donation')

    expect(store.moderateDonationVisibility(
      hiddenManual.record.id,
      { action: 'hide', reason: 'operator noticed' },
      now + 5,
    ).ok).toBe(true)

    const redacted = store.commitDonation({
      provider: 'kofi',
      externalId: 'KOFI-HIDDEN-1',
      displayName: null,
      amountMinor: 1_500,
      currency: 'USD',
      minorDigits: 2,
      message: null,
      isPublic: false,
      donatedAt: now,
    }, now + 10)
    expect(redacted.outcome).toBe('redacted_private')
    if (redacted.outcome !== 'redacted_private') throw new Error('expected redacted_private')
    expect(redacted.previousVisibility).toBe('hidden')
    expect(redacted.record.displayName).toBeNull()
    expect(redacted.record.message).toBeNull()
    expect(redacted.record.visibility).toEqual({ kind: 'private_donor' })
    expect(store.moderateDonationVisibility(hiddenManual.record.id, { action: 'restore' }, now + 15)).toEqual({
      ok: false,
      code: 'private_donor_immutable',
    })

    // A verified public Ko-fi callback matching an existing manual public record stays a no-op duplicate.
    const publicManual = store.createManualDonation({
      provider: 'kofi',
      externalId: 'KOFI-PUB-1',
      displayName: 'Public Manual Name',
      amountMinor: 2_000,
      currency: 'USD',
      minorDigits: 2,
      message: 'Public manual note',
      donatedAt: now + 20,
      idempotencyKey: null,
    }, now + 20)
    expect(publicManual.ok).toBe(true)
    if (!publicManual.ok) throw new Error('expected manual donation')

    const versionBefore = store.getPublicWallSnapshot().version
    const dupPublic = store.commitDonation({
      provider: 'kofi',
      externalId: 'KOFI-PUB-1',
      displayName: 'Different Webhook Name',
      amountMinor: 2_000,
      currency: 'USD',
      minorDigits: 2,
      message: 'Different webhook note',
      isPublic: true,
      donatedAt: now + 20,
    }, now + 25)
    expect(dupPublic).toEqual({ outcome: 'deduplicated', record: publicManual.record })
    expect(store.getPublicWallSnapshot().version).toBe(versionBefore)
  })
})

describe('Ko-fi webhook HTTP boundary and structured redaction logging', () => {
  it('requires webhook verification before redacting and logs warn on redaction without leaking text', async () => {
    const logs = captureStructuredLogs()
    const store = createInMemoryWallStore()
    const adminPassword = randomBytes(16).toString('hex')
    let now = 1_700_000_000
    const env: DonationsEnv = {
      DONATION_WALL: createWallNamespaceFromStore(store),
      AFDIAN_USER_ID: 'creator',
      KOFI_VERIFICATION_TOKEN: KOFI_TOKEN,
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
    const cookie = (loginRes.headers.get('set-cookie') ?? '').split(';', 1)[0] ?? ''
    const { csrfToken } = await loginRes.json() as { csrfToken: string }

    const createRes = await handleWorkerFetch(
      new Request(`${API_ORIGIN}/v1/admin/donations`, {
        method: 'POST',
        headers: {
          Origin: API_ORIGIN,
          Cookie: cookie,
          'X-CSRF-Token': csrfToken,
          'Content-Type': 'application/json',
          [IDEMPOTENCY_KEY_HEADER]: 'http-manual-kofi-1',
        },
        body: JSON.stringify({
          provider: 'kofi',
          externalId: 'KOFI-HTTP-PRIV',
          amount: '100',
          currency: 'USD',
          displayName: 'Confidential Name',
          message: 'Confidential Note',
          donatedAt: now,
        }),
      }),
      env,
      { nowSec: () => now },
    )
    expect(createRes.status).toBe(201)
    const { record: created } = await createRes.json() as { record: DonationRecord }

    const sendKofi = (token: string, isPublic: boolean) => handleWorkerFetch(
      new Request(`${API_ORIGIN}/v1/donations/webhooks/kofi`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({
          data: JSON.stringify({
            verification_token: token,
            kofi_transaction_id: 'KOFI-HTTP-PRIV',
            type: 'Donation',
            amount: '100.00',
            currency: 'USD',
            is_public: isPublic,
            from_name: 'Confidential Name',
            message: 'Confidential Note',
            email: 'secret@example.com',
          }),
        }).toString(),
      }),
      env,
      { nowSec: () => now },
    )

    // Unauthenticated webhook cannot redact a manually published entry.
    now += 5
    expect((await sendKofi('wrong-token', false)).status).toBe(403)
    const wallBeforeRes = await handleWorkerFetch(new Request(`${API_ORIGIN}/v1/donations/wall`), env, { nowSec: () => now })
    const etagBefore = wallBeforeRes.headers.get('etag') ?? ''
    expect((await wallBeforeRes.json() as PublicWallResponse).entries).toHaveLength(1)

    // Verified private webhook redacts the record, invalidates wall ETag, and logs a warning.
    now += 5
    expect((await sendKofi(KOFI_TOKEN, false)).status).toBe(200)
    const wallAfterRes = await handleWorkerFetch(
      new Request(`${API_ORIGIN}/v1/donations/wall`, { headers: { 'If-None-Match': etagBefore } }),
      env,
      { nowSec: () => now },
    )
    expect(wallAfterRes.status).toBe(200)
    const etagAfter = wallAfterRes.headers.get('etag') ?? ''
    expect(etagAfter).not.toBe(etagBefore)
    const wallAfter = await wallAfterRes.json() as PublicWallResponse
    expect(wallAfter.entries).toHaveLength(0)
    expect(wallAfter.topSupporters).toHaveLength(0)

    // Duplicate delivery returns 200, keeps ETag unchanged, and logs deduplicated at info level.
    now += 5
    expect((await sendKofi(KOFI_TOKEN, false)).status).toBe(200)
    const wallRetryRes = await handleWorkerFetch(
      new Request(`${API_ORIGIN}/v1/donations/wall`, { headers: { 'If-None-Match': etagAfter } }),
      env,
      { nowSec: () => now },
    )
    expect(wallRetryRes.status).toBe(304)

    const committedLogs = logs.filter(entry => entry.payload.event === 'donation.committed')
    expect(committedLogs).toEqual([
      {
        level: 'warn',
        payload: {
          event: 'donation.committed',
          donation_id: created.id,
          provider: 'kofi',
          external_id: 'KOFI-HTTP-PRIV',
          currency: 'USD',
          amount_minor: 10_000,
          visibility: 'private_donor',
          outcome: 'redacted_private',
          previous_visibility: 'published',
        },
      },
      {
        level: 'info',
        payload: {
          event: 'donation.committed',
          donation_id: created.id,
          provider: 'kofi',
          external_id: 'KOFI-HTTP-PRIV',
          currency: 'USD',
          amount_minor: 10_000,
          visibility: 'private_donor',
          outcome: 'deduplicated',
        },
      },
    ])
    const serializedLogs = JSON.stringify(logs)
    expect(serializedLogs).not.toContain('Confidential Name')
    expect(serializedLogs).not.toContain('Confidential Note')
    expect(serializedLogs).not.toContain('secret@example.com')
  })
})
