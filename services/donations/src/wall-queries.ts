import { createHash } from 'node:crypto'
import {
  emptyVisibilityCounts,
  type AdminDonationsCursor,
  type AdminDonationsFilter,
  type AdminVisibilityCounts,
  type CreateManualDonationInput,
  type CryptoCheckoutIntent,
  type DonationProvider,
  type DonationRecord,
  type PublicWallEntry,
  type VisibilityKind,
  type WallVisibility,
} from './model'

const DEFAULT_ADMIN_PAGE_LIMIT = 50
const MAX_ADMIN_PAGE_LIMIT = 100

export interface BuiltSqlQuery {
  readonly sql: string
  readonly bindings: readonly unknown[]
}

export const WALL_SCHEMA_STATEMENTS: readonly string[] = [
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
  `CREATE TABLE IF NOT EXISTS manual_donation_idempotency (
    idempotency_key TEXT PRIMARY KEY,
    request_hash TEXT NOT NULL,
    donation_id TEXT NOT NULL,
    created_at INTEGER NOT NULL
  )`,
]

export const ADMIN_DONATION_COLUMNS = `id, provider, external_id, display_name, amount_minor, currency,
  minor_digits, message, visibility_kind, visibility_json, donated_at`

export const PUBLIC_WALL_PUBLISHED_SQL = `SELECT id, provider, display_name, amount_minor, currency,
       minor_digits, message, donated_at
FROM donations
WHERE visibility_kind = 'published'
ORDER BY donated_at DESC, id DESC`

export const INSERT_DONATION_SQL = `INSERT INTO donations (
  id, provider, external_id, display_name, amount_minor, currency,
  minor_digits, message, visibility_kind, visibility_json, donated_at, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

export const REDACT_PRIVATE_DONATION_SQL = `UPDATE donations
SET display_name = NULL,
    message = NULL,
    visibility_kind = ?,
    visibility_json = ?
WHERE id = ?`

export const SELECT_MANUAL_IDEMPOTENCY_SQL = `SELECT request_hash, donation_id
FROM manual_donation_idempotency
WHERE idempotency_key = ?`

export const INSERT_MANUAL_IDEMPOTENCY_SQL = `INSERT INTO manual_donation_idempotency (
  idempotency_key, request_hash, donation_id, created_at
) VALUES (?, ?, ?, ?)`

export function rowToCryptoIntent(row: Record<string, unknown>): CryptoCheckoutIntent {
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

export function normalizeAdminPageLimit(rawLimit?: number | null): number {
  // Guard SQLite's integer LIMIT invariant here so fractional, NaN, or unsafe-integer
  // inputs from any caller cannot trigger SQLITE_MISMATCH.
  const candidate = rawLimit !== null && rawLimit !== undefined && Number.isSafeInteger(rawLimit)
    ? rawLimit
    : DEFAULT_ADMIN_PAGE_LIMIT
  return Math.min(MAX_ADMIN_PAGE_LIMIT, Math.max(1, candidate))
}

export function rowToDonationRecord(row: Record<string, unknown>): DonationRecord {
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

export function rowToPublicWallEntry(row: Record<string, unknown>): PublicWallEntry {
  return {
    id: String(row.id),
    provider: String(row.provider) as DonationProvider,
    displayName: row.display_name === null ? null : String(row.display_name),
    amountMinor: Number(row.amount_minor),
    currency: String(row.currency),
    minorDigits: Number(row.minor_digits),
    message: row.message === null ? null : String(row.message),
    donatedAt: Number(row.donated_at),
  }
}

function escapeLikePattern(raw: string): string {
  return raw.replaceAll('\\', '\\\\').replaceAll('%', '\\%').replaceAll('_', '\\_')
}

export function buildAdminDonationsQuery(
  cursor: AdminDonationsCursor | null,
  limit: number,
  filter?: AdminDonationsFilter,
): BuiltSqlQuery {
  const clauses: string[] = []
  const bindings: unknown[] = []

  if (filter?.visibility !== undefined) {
    clauses.push('visibility_kind = ?')
    bindings.push(filter.visibility)
  }
  if (filter?.provider !== undefined) {
    clauses.push('provider = ?')
    bindings.push(filter.provider)
  }
  const search = filter?.query?.trim()
  if (search !== undefined && search.length > 0) {
    const pattern = `%${escapeLikePattern(search.toLowerCase())}%`
    clauses.push(
      `(LOWER(COALESCE(display_name, '')) LIKE ? ESCAPE '\\'`
      + ` OR LOWER(COALESCE(message, '')) LIKE ? ESCAPE '\\'`
      + ` OR LOWER(external_id) LIKE ? ESCAPE '\\'`
      + ` OR LOWER(id) LIKE ? ESCAPE '\\')`,
    )
    bindings.push(pattern, pattern, pattern, pattern)
  }
  if (cursor !== null) {
    clauses.push('((donated_at < ?) OR (donated_at = ? AND id < ?))')
    bindings.push(cursor.donatedAt, cursor.donatedAt, cursor.id)
  }

  const whereClause = clauses.length > 0 ? `WHERE ${clauses.join(' AND ')}\n` : ''
  bindings.push(limit + 1)

  return {
    sql: `SELECT ${ADMIN_DONATION_COLUMNS}
      FROM donations
      ${whereClause}ORDER BY donated_at DESC, id DESC
      LIMIT ?`,
    bindings,
  }
}

export function reduceVisibilityCounts(
  rows: Iterable<Record<string, unknown>>,
): AdminVisibilityCounts {
  const counts: Record<VisibilityKind | 'total', number> = { ...emptyVisibilityCounts() }
  for (const row of rows) {
    const kind = String(row.visibility_kind)
    const count = Number(row.count)
    if (!Number.isFinite(count) || count <= 0) continue
    if (
      kind === 'published'
      || kind === 'pending_review'
      || kind === 'below_threshold'
      || kind === 'hidden'
      || kind === 'private_donor'
    ) {
      counts[kind] += count
      counts.total += count
    }
  }
  return counts
}

export function computeManualDonationRequestHash(input: CreateManualDonationInput): string {
  // Hash normalized caller-supplied fields before server defaults (generated externalId and
  // nowSec clamping) so identical retries replay across clock ticks while edited payloads conflict.
  const canonical = JSON.stringify([
    input.provider,
    input.externalId,
    input.displayName,
    input.amountMinor,
    input.currency,
    input.minorDigits,
    input.message,
    input.donatedAt,
  ])
  return createHash('sha256').update(canonical).digest('hex')
}
