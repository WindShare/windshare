import {
  emptyVisibilityCounts,
  type AdminDonationsCursor,
  type AdminDonationsFilter,
  type AdminVisibilityCounts,
  type VisibilityKind,
} from './model'

export interface BuiltSqlQuery {
  readonly sql: string
  readonly bindings: readonly unknown[]
}

const ADMIN_DONATION_COLUMNS = `id, provider, external_id, display_name, amount_minor, currency,
  minor_digits, message, visibility_kind, visibility_json, donated_at`

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
