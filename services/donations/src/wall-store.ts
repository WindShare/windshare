import { randomBytes } from 'node:crypto'
import {
  ADMIN_SESSION_TTL_SECONDS,
  CRYPTO_INTENT_TTL_SECONDS,
  MIN_PUBLIC_CNY_MINOR,
  MIN_PUBLIC_USD_MINOR,
  PUBLIC_WALL_LIMIT,
  applyVisibilityCommand,
  clampDonatedAt,
  projectPublicWallEntry,
  type AdminConfigPatch,
  type AdminDonationsFilter,
  type AdminDonationsPage,
  type AdminSessionRecord,
  type AdminVisibilityCommand,
  type AdminVisibilityCounts,
  type CommitDonationInput,
  type CommitDonationResult,
  type CreateAdminSessionInput,
  type CreateCryptoIntentInput,
  type CryptoCheckoutIntent,
  type DonationConfig,
  type DonationProvider,
  type DonationRecord,
  type PublicWallResponse,
  type WallVisibility,
} from './model'
import { determineInitialVisibility } from './sanitize'
import { buildAdminDonationsQuery, reduceVisibilityCounts } from './wall-queries'

const CLEANUP_BATCH_SIZE = 32
const RATE_LIMIT_STALE_SECONDS = 3600
const DEFAULT_ADMIN_PAGE_LIMIT = 50
const MAX_ADMIN_PAGE_LIMIT = 100
// Keep parameterized IN-clause chunks well under SQLite's 999 bound parameter ceiling.
const SQL_IN_BATCH_SIZE = 100
const CURSOR_PATTERN = /^(\d+):([A-Za-z0-9_-]+)$/

export type RateLimitScope = 'crypto' | 'admin_login'

interface BucketPolicy {
  readonly capacity: number
  readonly windowSeconds: number
}

interface ScopeRateLimitPolicy {
  readonly ip: BucketPolicy
  readonly global?: BucketPolicy
}

const RATE_LIMIT_POLICIES: Readonly<Record<RateLimitScope, ScopeRateLimitPolicy>> = Object.freeze({
  crypto: {
    // Bound unauthenticated NOWPayments invoice creation and 7-day intent storage
    // while leaving headroom for legitimate retries and shared-NAT donors.
    ip: { capacity: 5, windowSeconds: 60 },
    global: { capacity: 30, windowSeconds: 60 },
  },
  admin_login: {
    // Omit a shared global bucket so unauthenticated callers cannot lock out the operator;
    // distributed guessing is bounded by requiring a high-entropy DONATE_ADMIN_PASSWORD.
    ip: { capacity: 5, windowSeconds: 300 },
  },
})

export interface SqlStoragePort {
  exec(query: string, ...bindings: unknown[]): Iterable<Record<string, unknown>>
}

export interface WallSnapshot {
  readonly version: number
  readonly etag: string
  readonly json: string
}

export interface RateLimitDecision {
  readonly allowed: boolean
  readonly retryAfterSeconds: number
}

export type CreateIntentResult =
  | { readonly ok: true; readonly intent: CryptoCheckoutIntent }
  | { readonly ok: false; readonly code: 'crypto_checkout_paused' }
  | { readonly ok: false; readonly code: 'rate_limited'; readonly retryAfterSeconds: number }

export type ModerateVisibilityResult =
  | {
    readonly ok: true
    readonly record: DonationRecord
    readonly fromVisibility: WallVisibility
    readonly toVisibility: WallVisibility
    readonly counts: AdminVisibilityCounts
  }
  | { readonly ok: false; readonly code: 'not_found' | 'private_donor_immutable' | 'invalid_transition' }

const SCHEMA_STATEMENTS: readonly string[] = [
  `CREATE TABLE IF NOT EXISTS donations (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    external_id TEXT NOT NULL,
    display_name TEXT,
    amount_minor INTEGER NOT NULL,
    currency TEXT NOT NULL,
    minor_digits INTEGER NOT NULL,
    message TEXT,
    visibility_kind TEXT NOT NULL,
    visibility_json TEXT NOT NULL,
    donated_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE(provider, external_id)
  )`,
  `CREATE INDEX IF NOT EXISTS idx_donations_public_wall
    ON donations (visibility_kind, donated_at DESC, id DESC)`,
  `CREATE INDEX IF NOT EXISTS idx_donations_admin_cursor
    ON donations (donated_at DESC, id DESC)`,
  `CREATE TABLE IF NOT EXISTS crypto_intents (
    intent_id TEXT PRIMARY KEY,
    display_name TEXT,
    message TEXT,
    requested_usd_minor INTEGER NOT NULL,
    return_origin TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
  )`,
  `CREATE INDEX IF NOT EXISTS idx_crypto_intents_expires_at
    ON crypto_intents (expires_at)`,
  `CREATE TABLE IF NOT EXISTS admin_sessions (
    session_hash TEXT PRIMARY KEY,
    csrf_token TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
  )`,
  `CREATE INDEX IF NOT EXISTS idx_admin_sessions_expires_at
    ON admin_sessions (expires_at)`,
  `CREATE TABLE IF NOT EXISTS rate_limits (
    bucket_key TEXT PRIMARY KEY,
    tokens REAL NOT NULL,
    updated_at INTEGER NOT NULL
  )`,
  `CREATE INDEX IF NOT EXISTS idx_rate_limits_updated_at
    ON rate_limits (updated_at)`,
  `CREATE TABLE IF NOT EXISTS donation_config (
    singleton_id INTEGER PRIMARY KEY CHECK (singleton_id = 1),
    min_public_cny_minor INTEGER NOT NULL,
    min_public_usd_minor INTEGER NOT NULL,
    moderation_enabled INTEGER NOT NULL,
    crypto_checkout_paused INTEGER NOT NULL,
    wall_version INTEGER NOT NULL
  )`,
]

function generateUrlSafeId(byteLength: number): string {
  return randomBytes(byteLength).toString('base64url')
}

function firstRow(cursor: Iterable<Record<string, unknown>>): Record<string, unknown> | null {
  for (const row of cursor) {
    return row
  }
  return null
}

function rowToDonationRecord(row: Record<string, unknown>): DonationRecord {
  return {
    id: String(row.id),
    provider: String(row.provider) as DonationProvider,
    externalId: String(row.external_id),
    displayName: row.display_name === null ? null : String(row.display_name),
    amountMinor: Number(row.amount_minor),
    currency: String(row.currency),
    minorDigits: Number(row.minor_digits),
    message: row.message === null ? null : String(row.message),
    visibility: JSON.parse(String(row.visibility_json)) as WallVisibility,
    donatedAt: Number(row.donated_at),
  }
}

function rowToCryptoIntent(row: Record<string, unknown>): CryptoCheckoutIntent {
  return {
    intentId: String(row.intent_id),
    displayName: row.display_name === null ? null : String(row.display_name),
    message: row.message === null ? null : String(row.message),
    requestedUsdMinor: Number(row.requested_usd_minor),
    returnOrigin: String(row.return_origin),
    createdAt: Number(row.created_at),
    expiresAt: Number(row.expires_at),
  }
}

function normalizeEtag(raw: string): string {
  const trimmed = raw.trim()
  return trimmed.startsWith('W/') ? trimmed.slice(2).trim() : trimmed
}

export function matchesIfNoneMatch(header: string | null, strongEtag: string): boolean {
  if (header === null || header.trim().length === 0) return false
  const target = normalizeEtag(strongEtag)
  for (const part of header.split(',')) {
    const candidate = part.trim()
    if (candidate === '*' || normalizeEtag(candidate) === target) {
      return true
    }
  }
  return false
}

export function decodeAdminCursor(
  raw: string | null,
): { readonly donatedAt: number; readonly id: string } | null {
  if (raw === null) return null
  const match = CURSOR_PATTERN.exec(raw.trim())
  if (match === null) return null
  const donatedAt = Number(match[1])
  const id = match[2] ?? ''
  if (!Number.isSafeInteger(donatedAt) || id.length === 0) return null
  return { donatedAt, id }
}

function normalizeAdminPageLimit(rawLimit?: number | null): number {
  // Guard SQLite's integer LIMIT invariant here so fractional, NaN, or unsafe-integer
  // inputs from any caller cannot trigger SQLITE_MISMATCH.
  const candidate = rawLimit !== null && rawLimit !== undefined && Number.isSafeInteger(rawLimit)
    ? rawLimit
    : DEFAULT_ADMIN_PAGE_LIMIT
  return Math.min(MAX_ADMIN_PAGE_LIMIT, Math.max(1, candidate))
}

function evaluateBucket(
  row: Record<string, unknown> | null,
  policy: BucketPolicy,
  nowSec: number,
): { readonly available: number; readonly retryAfterSeconds: number } {
  const refillPerSec = policy.capacity / policy.windowSeconds
  let available = policy.capacity
  if (row !== null) {
    const prevTokens = Number(row.tokens)
    const prevUpdated = Number(row.updated_at)
    const elapsed = Math.max(0, nowSec - prevUpdated)
    available = Math.min(policy.capacity, prevTokens + elapsed * refillPerSec)
  }
  if (available >= 1) {
    return { available, retryAfterSeconds: 0 }
  }
  const deficit = 1 - available
  return {
    available,
    retryAfterSeconds: Math.max(1, Math.ceil(deficit / refillPerSec)),
  }
}

export class WallStore {
  private readonly sql: SqlStoragePort
  private readonly runTx: <T>(closure: () => T) => T
  private cachedWall: WallSnapshot | null = null

  constructor(sql: SqlStoragePort, runTx: <T>(closure: () => T) => T = fn => fn()) {
    this.sql = sql
    this.runTx = runTx
    this.ensureSchema()
  }

  private ensureSchema(): void {
    this.runTx(() => {
      for (const statement of SCHEMA_STATEMENTS) {
        this.sql.exec(statement)
      }
      this.sql.exec(
        `INSERT OR IGNORE INTO donation_config (
          singleton_id,
          min_public_cny_minor,
          min_public_usd_minor,
          moderation_enabled,
          crypto_checkout_paused,
          wall_version
        ) VALUES (1, ?, ?, 0, 0, 0)`,
        MIN_PUBLIC_CNY_MINOR,
        MIN_PUBLIC_USD_MINOR,
      )
    })
  }

  private readConfigRow(): { readonly config: DonationConfig; readonly wallVersion: number } {
    const row = firstRow(this.sql.exec(
      `SELECT min_public_cny_minor, min_public_usd_minor, moderation_enabled,
              crypto_checkout_paused, wall_version
       FROM donation_config WHERE singleton_id = 1`,
    ))
    if (row === null) {
      throw new Error('donation_config singleton row is missing')
    }
    return {
      config: {
        minPublicCnyMinor: Number(row.min_public_cny_minor),
        minPublicUsdMinor: Number(row.min_public_usd_minor),
        moderationEnabled: Number(row.moderation_enabled) === 1,
        cryptoCheckoutPaused: Number(row.crypto_checkout_paused) === 1,
      },
      wallVersion: Number(row.wall_version),
    }
  }

  private bumpWallVersion(): void {
    this.sql.exec('UPDATE donation_config SET wall_version = wall_version + 1 WHERE singleton_id = 1')
    this.cachedWall = null
  }

  getConfig(): DonationConfig {
    return this.readConfigRow().config
  }

  patchConfig(patch: AdminConfigPatch): DonationConfig {
    return this.runTx(() => {
      const { config } = this.readConfigRow()
      const next: DonationConfig = {
        minPublicCnyMinor: patch.minPublicCnyMinor ?? config.minPublicCnyMinor,
        minPublicUsdMinor: patch.minPublicUsdMinor ?? config.minPublicUsdMinor,
        moderationEnabled: patch.moderationEnabled ?? config.moderationEnabled,
        cryptoCheckoutPaused: patch.cryptoCheckoutPaused ?? config.cryptoCheckoutPaused,
      }
      // Config governs future intake and checkout admission only; it is not projected into the public wall.
      this.sql.exec(
        `UPDATE donation_config
         SET min_public_cny_minor = ?,
             min_public_usd_minor = ?,
             moderation_enabled = ?,
             crypto_checkout_paused = ?
         WHERE singleton_id = 1`,
        next.minPublicCnyMinor,
        next.minPublicUsdMinor,
        next.moderationEnabled ? 1 : 0,
        next.cryptoCheckoutPaused ? 1 : 0,
      )
      return next
    })
  }

  getPublicWallSnapshot(): WallSnapshot {
    if (this.cachedWall !== null) {
      return this.cachedWall
    }
    const { wallVersion } = this.readConfigRow()
    const entries = []
    for (const row of this.sql.exec(
      `SELECT id, provider, external_id, display_name, amount_minor, currency,
              minor_digits, message, visibility_kind, visibility_json, donated_at
       FROM donations
       WHERE visibility_kind = 'published'
       ORDER BY donated_at DESC, id DESC
       LIMIT ?`,
      PUBLIC_WALL_LIMIT,
    )) {
      entries.push(projectPublicWallEntry(rowToDonationRecord(row)))
    }
    const payload: PublicWallResponse = {
      version: wallVersion,
      entries,
    }
    this.cachedWall = {
      version: wallVersion,
      etag: `"wall-v${wallVersion}"`,
      json: JSON.stringify(payload),
    }
    return this.cachedWall
  }

  private consumeRateLimitInTx(
    scope: RateLimitScope,
    clientIp: string,
    nowSec: number,
  ): RateLimitDecision {
    const policy = RATE_LIMIT_POLICIES[scope]
    const ipKey = `${scope}:ip:${clientIp}`
    const globalKey = policy.global === undefined ? null : `${scope}:global`

    this.sql.exec(
      `DELETE FROM rate_limits WHERE bucket_key IN (
        SELECT bucket_key FROM rate_limits WHERE updated_at <= ? LIMIT ?
      )`,
      nowSec - RATE_LIMIT_STALE_SECONDS,
      CLEANUP_BATCH_SIZE,
    )

    const ipRow = firstRow(this.sql.exec(
      'SELECT tokens, updated_at FROM rate_limits WHERE bucket_key = ?',
      ipKey,
    ))
    const ipEval = evaluateBucket(ipRow, policy.ip, nowSec)

    const globalEval = policy.global !== undefined && globalKey !== null
      ? evaluateBucket(
        firstRow(this.sql.exec(
          'SELECT tokens, updated_at FROM rate_limits WHERE bucket_key = ?',
          globalKey,
        )),
        policy.global,
        nowSec,
      )
      : null

    const retryAfterSeconds = Math.max(
      ipEval.retryAfterSeconds,
      globalEval?.retryAfterSeconds ?? 0,
    )
    if (retryAfterSeconds > 0) {
      return { allowed: false, retryAfterSeconds }
    }

    const upsertSql = `INSERT INTO rate_limits (bucket_key, tokens, updated_at)
      VALUES (?, ?, ?)
      ON CONFLICT(bucket_key) DO UPDATE SET tokens = excluded.tokens, updated_at = excluded.updated_at`
    this.sql.exec(upsertSql, ipKey, ipEval.available - 1, nowSec)
    if (globalKey !== null && globalEval !== null) {
      this.sql.exec(upsertSql, globalKey, globalEval.available - 1, nowSec)
    }
    return { allowed: true, retryAfterSeconds: 0 }
  }

  consumeRateLimit(scope: RateLimitScope, clientIp: string, nowSec: number): RateLimitDecision {
    return this.runTx(() => this.consumeRateLimitInTx(scope, clientIp, nowSec))
  }

  createCryptoIntent(input: CreateCryptoIntentInput): CreateIntentResult {
    return this.runTx(() => {
      const { config } = this.readConfigRow()
      if (config.cryptoCheckoutPaused) {
        return { ok: false, code: 'crypto_checkout_paused' }
      }
      const rateLimit = this.consumeRateLimitInTx('crypto', input.clientIp, input.nowSec)
      if (!rateLimit.allowed) {
        return { ok: false, code: 'rate_limited', retryAfterSeconds: rateLimit.retryAfterSeconds }
      }

      // Lazily evict a bounded batch of expired intents to avoid unbounded storage growth.
      this.sql.exec(
        `DELETE FROM crypto_intents WHERE intent_id IN (
          SELECT intent_id FROM crypto_intents WHERE expires_at <= ? LIMIT ?
        )`,
        input.nowSec,
        CLEANUP_BATCH_SIZE,
      )

      const intent: CryptoCheckoutIntent = {
        intentId: generateUrlSafeId(16),
        displayName: input.displayName,
        message: input.message,
        requestedUsdMinor: input.requestedUsdMinor,
        returnOrigin: input.returnOrigin,
        createdAt: input.nowSec,
        expiresAt: input.nowSec + CRYPTO_INTENT_TTL_SECONDS,
      }
      this.sql.exec(
        `INSERT INTO crypto_intents (
          intent_id, display_name, message, requested_usd_minor,
          return_origin, created_at, expires_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?)`,
        intent.intentId,
        intent.displayName,
        intent.message,
        intent.requestedUsdMinor,
        intent.returnOrigin,
        intent.createdAt,
        intent.expiresAt,
      )
      return { ok: true, intent }
    })
  }

  getCryptoIntent(intentId: string, nowSec: number): CryptoCheckoutIntent | null {
    const row = firstRow(this.sql.exec(
      `SELECT intent_id, display_name, message, requested_usd_minor,
              return_origin, created_at, expires_at
       FROM crypto_intents
       WHERE intent_id = ? AND expires_at > ?`,
      intentId,
      nowSec,
    ))
    return row === null ? null : rowToCryptoIntent(row)
  }

  hasDonation(provider: DonationProvider, externalId: string): boolean {
    const row = firstRow(this.sql.exec(
      'SELECT 1 AS found FROM donations WHERE provider = ? AND external_id = ? LIMIT 1',
      provider,
      externalId,
    ))
    return row !== null
  }

  findExistingExternalIds(
    provider: DonationProvider,
    externalIds: readonly string[],
  ): string[] {
    const uniqueIds = [...new Set(externalIds.filter(id => id.length > 0))]
    if (uniqueIds.length === 0) return []
    const existing: string[] = []
    for (let offset = 0; offset < uniqueIds.length; offset += SQL_IN_BATCH_SIZE) {
      const chunk = uniqueIds.slice(offset, offset + SQL_IN_BATCH_SIZE)
      const placeholders = chunk.map(() => '?').join(', ')
      const rows = this.sql.exec(
        `SELECT external_id FROM donations WHERE provider = ? AND external_id IN (${placeholders})`,
        provider,
        ...chunk,
      )
      for (const row of rows) {
        existing.push(String(row.external_id))
      }
    }
    return existing
  }

  private insertDonationInTx(
    input: CommitDonationInput,
    config: DonationConfig,
    nowSec: number,
  ): CommitDonationResult {
    const existing = firstRow(this.sql.exec(
      `SELECT id, provider, external_id, display_name, amount_minor, currency,
              minor_digits, message, visibility_kind, visibility_json, donated_at
       FROM donations
       WHERE provider = ? AND external_id = ?
       LIMIT 1`,
      input.provider,
      input.externalId,
    ))
    if (existing !== null) {
      return { record: rowToDonationRecord(existing), deduplicated: true }
    }

    // Enforce privacy invariant at the persistence boundary even if a caller passes text.
    const displayName = input.isPublic ? input.displayName : null
    const message = input.isPublic ? input.message : null
    const visibility = determineInitialVisibility({
      amountMinor: input.amountMinor,
      currency: input.currency,
      displayName,
      message,
      isPublic: input.isPublic,
      config,
    })
    const donatedAt = clampDonatedAt(input.donatedAt, nowSec)
    const record: DonationRecord = {
      id: generateUrlSafeId(12),
      provider: input.provider,
      externalId: input.externalId,
      displayName,
      amountMinor: input.amountMinor,
      currency: input.currency,
      minorDigits: input.minorDigits,
      message,
      visibility,
      donatedAt,
    }

    this.sql.exec(
      `INSERT INTO donations (
        id, provider, external_id, display_name, amount_minor, currency,
        minor_digits, message, visibility_kind, visibility_json, donated_at, created_at
      ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      record.id,
      record.provider,
      record.externalId,
      record.displayName,
      record.amountMinor,
      record.currency,
      record.minorDigits,
      record.message,
      record.visibility.kind,
      JSON.stringify(record.visibility),
      record.donatedAt,
      nowSec,
    )
    return { record, deduplicated: false }
  }

  commitDonation(input: CommitDonationInput, nowSec: number): CommitDonationResult {
    return this.runTx(() => {
      const { config } = this.readConfigRow()
      const result = this.insertDonationInTx(input, config, nowSec)
      if (!result.deduplicated) {
        this.bumpWallVersion()
      }
      return result
    })
  }

  commitDonationsBatch(
    inputs: readonly CommitDonationInput[],
    nowSec: number,
  ): CommitDonationResult[] {
    if (inputs.length === 0) return []
    return this.runTx(() => {
      const { config } = this.readConfigRow()
      const results: CommitDonationResult[] = []
      let insertedAny = false
      for (const input of inputs) {
        const result = this.insertDonationInTx(input, config, nowSec)
        if (!result.deduplicated) insertedAny = true
        results.push(result)
      }
      if (insertedAny) {
        this.bumpWallVersion()
      }
      return results
    })
  }

  createAdminSession(input: CreateAdminSessionInput): AdminSessionRecord {
    return this.runTx(() => {
      this.sql.exec(
        `DELETE FROM admin_sessions WHERE session_hash IN (
          SELECT session_hash FROM admin_sessions WHERE expires_at <= ? LIMIT ?
        )`,
        input.nowSec,
        CLEANUP_BATCH_SIZE,
      )
      const expiresAt = input.nowSec + ADMIN_SESSION_TTL_SECONDS
      this.sql.exec(
        `INSERT INTO admin_sessions (session_hash, csrf_token, created_at, expires_at)
         VALUES (?, ?, ?, ?)`,
        input.sessionHash,
        input.csrfToken,
        input.nowSec,
        expiresAt,
      )
      return { csrfToken: input.csrfToken, expiresAt }
    })
  }

  verifyAdminSession(sessionHash: string, nowSec: number): AdminSessionRecord | null {
    const row = firstRow(this.sql.exec(
      `SELECT csrf_token, expires_at
       FROM admin_sessions
       WHERE session_hash = ? AND expires_at > ?`,
      sessionHash,
      nowSec,
    ))
    if (row === null) return null
    return {
      csrfToken: String(row.csrf_token),
      expiresAt: Number(row.expires_at),
    }
  }

  revokeAdminSession(sessionHash: string): void {
    this.sql.exec('DELETE FROM admin_sessions WHERE session_hash = ?', sessionHash)
  }

  getVisibilityCounts(): AdminVisibilityCounts {
    return reduceVisibilityCounts(
      this.sql.exec(
        'SELECT visibility_kind, COUNT(*) AS count FROM donations GROUP BY visibility_kind',
      ),
    )
  }

  listAdminDonations(
    afterCursor: string | null,
    rawLimit?: number | null,
    filter?: AdminDonationsFilter,
  ): AdminDonationsPage {
    const limit = normalizeAdminPageLimit(rawLimit)
    const cursor = decodeAdminCursor(afterCursor)
    const built = buildAdminDonationsQuery(cursor, limit, filter)
    const rows = this.sql.exec(built.sql, ...built.bindings)

    const all: DonationRecord[] = []
    for (const row of rows) {
      all.push(rowToDonationRecord(row))
    }
    const hasMore = all.length > limit
    const items = hasMore ? all.slice(0, limit) : all
    const lastItem = items[items.length - 1]
    const nextCursor = hasMore && lastItem !== undefined
      ? `${lastItem.donatedAt}:${lastItem.id}`
      : null

    return {
      items,
      nextCursor,
      config: this.getConfig(),
      counts: this.getVisibilityCounts(),
    }
  }

  moderateDonationVisibility(
    id: string,
    command: AdminVisibilityCommand,
    nowSec: number,
  ): ModerateVisibilityResult {
    return this.runTx(() => {
      const row = firstRow(this.sql.exec(
        `SELECT id, provider, external_id, display_name, amount_minor, currency,
                minor_digits, message, visibility_kind, visibility_json, donated_at
         FROM donations WHERE id = ?`,
        id,
      ))
      if (row === null) {
        return { ok: false, code: 'not_found' }
      }
      const existing = rowToDonationRecord(row)
      const transition = applyVisibilityCommand(existing.visibility, command, nowSec)
      if (!transition.ok) {
        return transition
      }

      this.sql.exec(
        `UPDATE donations
         SET visibility_kind = ?, visibility_json = ?
         WHERE id = ?`,
        transition.next.kind,
        JSON.stringify(transition.next),
        id,
      )
      this.bumpWallVersion()
      return {
        ok: true,
        record: { ...existing, visibility: transition.next },
        fromVisibility: existing.visibility,
        toVisibility: transition.next,
        counts: this.getVisibilityCounts(),
      }
    })
  }
}
