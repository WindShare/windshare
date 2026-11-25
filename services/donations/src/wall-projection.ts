import { createHash } from 'node:crypto'
import { logWallProjectionRebuilt, type WallProjectionRebuildReason } from './log'
import {
  DONATION_PROVIDERS,
  PUBLIC_WALL_LIMIT,
  TOP_SUPPORTERS_LIMIT,
  projectPublicWallEntry,
  type CommitDonationResult,
  type DonationRecord,
  type PublicWallEntry,
  type PublicWallResponse,
  type VisibilityKind,
} from './model'
import {
  RANKING_RULE_SIGNATURE,
  TopSupportersAccumulator,
  selectTopSupporters,
  type RankableSupporterEntry,
} from './ranking'
import {
  PUBLIC_WALL_ENTRY_COLUMNS,
  PUBLIC_WALL_RANKABLE_SQL,
  PUBLIC_WALL_RECENT_SQL,
  SEED_EMPTY_PUBLIC_WALL_PROJECTION_SQL,
  SELECT_PUBLIC_WALL_PROJECTION_SQL,
  UPSERT_PUBLIC_WALL_PROJECTION_SQL,
  rowToPublicWallEntry,
  rowToRankableSupporterEntry,
} from './wall-queries'

// Truncate SHA-256 to 64 bits (16 hex chars) for compact strong ETags and projection fingerprints.
const WALL_ETAG_DIGEST_HEX_LENGTH = 16
const PROJECTION_FINGERPRINT_HEX_LENGTH = 16
// Bump whenever PublicWallResponse or PublicWallEntry serialized JSON shape changes.
export const PUBLIC_WALL_PROJECTION_SCHEMA_VERSION = 1

export interface SqlStoragePort {
  exec(query: string, ...bindings: unknown[]): Iterable<Record<string, unknown>>
}

export interface WallSnapshot {
  readonly version: number
  readonly etag: string
  readonly json: string
}

export interface PublicWallDelta {
  readonly added: readonly PublicWallEntry[]
  readonly removedIds: ReadonlySet<string>
}

const EMPTY_REMOVED_IDS: ReadonlySet<string> = new Set()

export function singleAddedPublicWallDelta(record: DonationRecord): PublicWallDelta {
  return { added: [projectPublicWallEntry(record)], removedIds: EMPTY_REMOVED_IDS }
}

export function commitResultToPublicWallDelta(result: CommitDonationResult): PublicWallDelta | null {
  if (result.outcome === 'inserted' && result.record.visibility.kind === 'published') {
    return singleAddedPublicWallDelta(result.record)
  }
  if (result.outcome === 'redacted_private' && result.previousVisibility === 'published') {
    return { added: [], removedIds: new Set([result.record.id]) }
  }
  return null
}

export function recordBatchPublicWallDelta(
  result: CommitDonationResult,
  addedById: Map<string, PublicWallEntry>,
  removedIds: Set<string>,
): void {
  if (result.outcome === 'inserted' && result.record.visibility.kind === 'published') {
    addedById.set(result.record.id, projectPublicWallEntry(result.record))
    return
  }
  if (result.outcome === 'redacted_private' && result.previousVisibility === 'published') {
    if (addedById.has(result.record.id)) {
      addedById.delete(result.record.id)
    } else {
      removedIds.add(result.record.id)
    }
  }
}

export function visibilityTransitionToPublicWallDelta(
  previousKind: VisibilityKind,
  nextKind: VisibilityKind,
  updatedRecord: DonationRecord,
): PublicWallDelta | null {
  if (previousKind !== 'published' && nextKind === 'published') {
    return singleAddedPublicWallDelta(updatedRecord)
  }
  if (previousKind === 'published' && nextKind !== 'published') {
    return { added: [], removedIds: new Set([updatedRecord.id]) }
  }
  return null
}

interface ProjectionRowInspection {
  readonly currentWallVersion: number
  readonly mismatchReason: WallProjectionRebuildReason | null
  readonly row: Record<string, unknown>
}

function firstRow(cursor: Iterable<Record<string, unknown>>): Record<string, unknown> | null {
  for (const row of cursor) {
    return row
  }
  return null
}

export function computePublicWallProjectionFingerprint(
  rankingSignature: string = RANKING_RULE_SIGNATURE,
): string {
  const descriptor = `schema=${PUBLIC_WALL_PROJECTION_SCHEMA_VERSION}|wallLimit=${PUBLIC_WALL_LIMIT}|${rankingSignature}`
  const digest = createHash('sha256')
    .update(descriptor, 'utf8')
    .digest('hex')
    .slice(0, PROJECTION_FINGERPRINT_HEX_LENGTH)
  return `proj-v${PUBLIC_WALL_PROJECTION_SCHEMA_VERSION}-${digest}`
}

export function computePublicWallEtag(wallVersion: number, serializedJson: string): string {
  // Bind the strong ETag to the serialized representation bytes as well as wallVersion
  // so projection changes (schema fields, ranking/FX rules) invalidate stale 304 caches
  // even when no database mutation incremented wall_version.
  const digest = createHash('sha256')
    .update(serializedJson, 'utf8')
    .digest('hex')
    .slice(0, WALL_ETAG_DIGEST_HEX_LENGTH)
  return `"wall-v${wallVersion}-${digest}"`
}

const HTTP_SP_CODE = 0x20
const HTTP_HTAB_CODE = 0x09
const ETAG_EXCLAMATION_CODE = 0x21
const ETAG_HASH_CODE = 0x23
const ETAG_TILDE_CODE = 0x7e
const ETAG_OBS_TEXT_MIN_CODE = 0x80
const ETAG_OBS_TEXT_MAX_CODE = 0xff
const WEAK_ETAG_PREFIX = 'W/'
const ETAG_QUOTE = '"'

function isHttpOws(code: number): boolean {
  return code === HTTP_SP_CODE || code === HTTP_HTAB_CODE
}

function isEtagChar(code: number): boolean {
  return code === ETAG_EXCLAMATION_CODE
    || (code >= ETAG_HASH_CODE && code <= ETAG_TILDE_CODE)
    || (code >= ETAG_OBS_TEXT_MIN_CODE && code <= ETAG_OBS_TEXT_MAX_CODE)
}

function skipHttpOws(input: string, index: number): number {
  let cursor = index
  while (cursor < input.length && isHttpOws(input.charCodeAt(cursor))) {
    cursor += 1
  }
  return cursor
}

function skipListSeparators(input: string, index: number): number {
  let cursor = index
  while (cursor < input.length) {
    const code = input.charCodeAt(cursor)
    if (!isHttpOws(code) && input[cursor] !== ',') {
      break
    }
    cursor += 1
  }
  return cursor
}

// Parse one RFC 9110 §8.8.3 entity-tag starting at `start`: an optional case-sensitive
// `W/` immediately followed by a quoted opaque-tag (no whitespace after `W/`, no escapes).
function parseEntityTagAt(
  input: string,
  start: number,
): { readonly opaqueTag: string; readonly nextIndex: number } | null {
  let quoteStart = start
  if (input.startsWith(WEAK_ETAG_PREFIX, quoteStart)) {
    quoteStart += WEAK_ETAG_PREFIX.length
  }
  if (input[quoteStart] !== ETAG_QUOTE) return null

  let cursor = quoteStart + 1
  while (cursor < input.length && input[cursor] !== ETAG_QUOTE) {
    if (!isEtagChar(input.charCodeAt(cursor))) return null
    cursor += 1
  }
  if (cursor >= input.length) return null

  return {
    opaqueTag: input.slice(quoteStart, cursor + 1),
    nextIndex: cursor + 1,
  }
}

function parseSingleOpaqueTag(raw: string): string | null {
  const start = skipHttpOws(raw, 0)
  const parsed = parseEntityTagAt(raw, start)
  if (parsed === null) return null
  return skipHttpOws(raw, parsed.nextIndex) === raw.length ? parsed.opaqueTag : null
}

function isWildcardIfNoneMatch(header: string, start: number): boolean {
  return header[start] === '*' && skipHttpOws(header, start + 1) === header.length
}

export function matchesIfNoneMatch(header: string | null, strongEtag: string): boolean {
  if (header === null) return false
  const target = parseSingleOpaqueTag(strongEtag)
  if (target === null) return false

  const start = skipHttpOws(header, 0)
  if (start === header.length) return false
  if (isWildcardIfNoneMatch(header, start)) return true

  // Scan the entire RFC 9110 §13.1.2 #entity-tag list so quoted commas stay intact
  // and trailing malformed tokens cannot produce order-dependent matches.
  let matched = false
  let cursor = skipListSeparators(header, start)
  while (cursor < header.length) {
    const parsed = parseEntityTagAt(header, cursor)
    if (parsed === null) return false
    matched ||= parsed.opaqueTag === target
    const afterTag = skipHttpOws(header, parsed.nextIndex)
    if (afterTag < header.length && header[afterTag] !== ',') return false
    cursor = skipListSeparators(header, afterTag)
  }
  return matched
}

function isNullableString(value: unknown): value is string | null {
  return value === null || typeof value === 'string'
}

function hasValidEntryStrings(candidate: Record<string, unknown>): boolean {
  if (typeof candidate.id !== 'string' || typeof candidate.currency !== 'string') return false
  if (typeof candidate.provider !== 'string') return false
  if (!(DONATION_PROVIDERS as readonly string[]).includes(candidate.provider)) return false
  return isNullableString(candidate.displayName) && isNullableString(candidate.message)
}

function hasValidEntryNumbers(candidate: Record<string, unknown>): boolean {
  return typeof candidate.amountMinor === 'number'
    && Number.isSafeInteger(candidate.amountMinor)
    && typeof candidate.minorDigits === 'number'
    && Number.isSafeInteger(candidate.minorDigits)
    && typeof candidate.donatedAt === 'number'
    && Number.isSafeInteger(candidate.donatedAt)
}

function isValidPublicWallEntry(value: unknown): value is PublicWallEntry {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    return false
  }
  const candidate = value as Record<string, unknown>
  return hasValidEntryStrings(candidate) && hasValidEntryNumbers(candidate)
}

function parsePersistedTopSupporters(raw: unknown): PublicWallEntry[] | null {
  if (typeof raw !== 'string') return null
  try {
    const parsed: unknown = JSON.parse(raw)
    if (!Array.isArray(parsed) || parsed.length > TOP_SUPPORTERS_LIMIT) {
      return null
    }
    return parsed.every(isValidPublicWallEntry) ? parsed : null
  } catch {
    return null
  }
}

function inspectPersistedMetadata(
  row: Record<string, unknown> | null,
  expectedFingerprint: string,
): ProjectionRowInspection {
  if (row === null) {
    throw new Error('donation_config singleton row is missing')
  }
  const currentWallVersion = Number(row.current_wall_version)
  if (row.projected_wall_version === null || row.projected_wall_version === undefined) {
    return { currentWallVersion, mismatchReason: 'missing_projection', row }
  }
  if (Number(row.projected_wall_version) !== currentWallVersion) {
    return { currentWallVersion, mismatchReason: 'version_mismatch', row }
  }
  if (String(row.projection_fingerprint) !== expectedFingerprint) {
    return { currentWallVersion, mismatchReason: 'fingerprint_mismatch', row }
  }
  return { currentWallVersion, mismatchReason: null, row }
}

function extractPersistedSnapshot(
  inspection: ProjectionRowInspection,
): { readonly ok: true; readonly snapshot: WallSnapshot } | { readonly ok: false; readonly reason: WallProjectionRebuildReason } {
  if (inspection.mismatchReason !== null) {
    return { ok: false, reason: inspection.mismatchReason }
  }
  const json = inspection.row.snapshot_json
  const etag = inspection.row.etag
  if (typeof json !== 'string' || json.length === 0 || typeof etag !== 'string' || etag.length === 0) {
    return { ok: false, reason: 'corrupt_projection' }
  }
  return {
    ok: true,
    snapshot: {
      version: inspection.currentWallVersion,
      etag,
      json,
    },
  }
}

export class PublicWallProjectionStore {
  private readonly sql: SqlStoragePort
  private readonly runTx: <T>(closure: () => T) => T
  private readonly projectionFingerprint: string
  private cachedWall: WallSnapshot | null = null

  constructor(
    sql: SqlStoragePort,
    runTx: <T>(closure: () => T) => T,
    projectionFingerprint: string = computePublicWallProjectionFingerprint(),
  ) {
    this.sql = sql
    this.runTx = runTx
    this.projectionFingerprint = projectionFingerprint
  }

  seedEmptyProjectionInTx(): void {
    const emptyPayload: PublicWallResponse = {
      version: 0,
      entries: [],
      topSupporters: [],
    }
    const json = JSON.stringify(emptyPayload)
    const etag = computePublicWallEtag(0, json)
    this.sql.exec(
      SEED_EMPTY_PUBLIC_WALL_PROJECTION_SQL,
      this.projectionFingerprint,
      json,
      etag,
    )
  }

  getSnapshot(): WallSnapshot {
    if (this.cachedWall !== null) {
      return this.cachedWall
    }
    const row = firstRow(this.sql.exec(SELECT_PUBLIC_WALL_PROJECTION_SQL))
    const extracted = extractPersistedSnapshot(
      inspectPersistedMetadata(row, this.projectionFingerprint),
    )
    if (extracted.ok) {
      this.cachedWall = extracted.snapshot
      return extracted.snapshot
    }

    const rebuilt = this.runTx(() => this.rebuildSnapshotInTx())
    this.cachedWall = rebuilt
    return rebuilt
  }

  // Execute a write transaction, persist any derived public wall delta atomically inside
  // the transaction, and populate DO memory cache only after SQLite commits.
  runProjectedTx<T>(
    closure: () => { readonly result: T; readonly delta: PublicWallDelta | null },
  ): T {
    const { result, snapshot } = this.runTx(() => {
      const outcome = closure()
      const snapshot = outcome.delta === null ? null : this.applyDeltaInTx(outcome.delta)
      return { result: outcome.result, snapshot }
    })
    if (snapshot !== null) {
      this.cachedWall = snapshot
    }
    return result
  }

  private applyDeltaInTx(delta: PublicWallDelta): WallSnapshot | null {
    if (delta.added.length === 0 && delta.removedIds.size === 0) {
      return null
    }
    const inspection = inspectPersistedMetadata(
      firstRow(this.sql.exec(SELECT_PUBLIC_WALL_PROJECTION_SQL)),
      this.projectionFingerprint,
    )
    const nextVersion = inspection.currentWallVersion + 1
    this.sql.exec('UPDATE donation_config SET wall_version = wall_version + 1 WHERE singleton_id = 1')

    const topSupporters = this.resolveTopSupportersAfterDeltaInTx(inspection, delta, nextVersion)
    const entries = this.queryRecentPublishedEntries()
    return this.persistSnapshotInTx(nextVersion, entries, topSupporters)
  }

  private rebuildSnapshotInTx(): WallSnapshot {
    const inspection = inspectPersistedMetadata(
      firstRow(this.sql.exec(SELECT_PUBLIC_WALL_PROJECTION_SQL)),
      this.projectionFingerprint,
    )
    const extracted = extractPersistedSnapshot(inspection)
    if (extracted.ok) {
      return extracted.snapshot
    }
    const entries = this.queryRecentPublishedEntries()
    const topSupporters = this.rebuildTopSupportersFromPublishedInTx(
      extracted.reason,
      inspection.currentWallVersion,
    )
    return this.persistSnapshotInTx(inspection.currentWallVersion, entries, topSupporters)
  }

  private resolveTopSupportersAfterDeltaInTx(
    inspection: ProjectionRowInspection,
    delta: PublicWallDelta,
    nextVersion: number,
  ): PublicWallEntry[] {
    if (inspection.mismatchReason !== null) {
      return this.rebuildTopSupportersFromPublishedInTx(inspection.mismatchReason, nextVersion)
    }
    const existingTopSupporters = parsePersistedTopSupporters(inspection.row.top_supporters_json)
    if (existingTopSupporters === null) {
      return this.rebuildTopSupportersFromPublishedInTx('corrupt_projection', nextVersion)
    }
    const removedCurrentTopSupporter = existingTopSupporters.some(entry => delta.removedIds.has(entry.id))
    if (removedCurrentTopSupporter) {
      return this.rebuildTopSupportersFromPublishedInTx('top_supporter_removed', nextVersion)
    }
    if (delta.added.length === 0) {
      return existingTopSupporters
    }
    return selectTopSupporters([...existingTopSupporters, ...delta.added])
  }

  private queryRecentPublishedEntries(): PublicWallEntry[] {
    const entries: PublicWallEntry[] = []
    for (const row of this.sql.exec(PUBLIC_WALL_RECENT_SQL, PUBLIC_WALL_LIMIT)) {
      entries.push(rowToPublicWallEntry(row))
    }
    return entries
  }

  private rebuildTopSupportersFromPublishedInTx(
    reason: WallProjectionRebuildReason,
    wallVersion: number,
  ): PublicWallEntry[] {
    const accumulator = new TopSupportersAccumulator<RankableSupporterEntry>(TOP_SUPPORTERS_LIMIT)
    let scannedRows = 0
    for (const row of this.sql.exec(PUBLIC_WALL_RANKABLE_SQL)) {
      scannedRows += 1
      accumulator.add(rowToRankableSupporterEntry(row))
    }
    if (scannedRows > 0 || reason !== 'missing_projection') {
      logWallProjectionRebuilt({ reason, wallVersion, scannedRows })
    }
    return this.hydrateRankedEntries(accumulator.result())
  }

  private hydrateRankedEntries(winners: readonly RankableSupporterEntry[]): PublicWallEntry[] {
    if (winners.length === 0) return []
    const placeholders = winners.map(() => '?').join(', ')
    const byId = new Map<string, PublicWallEntry>()
    for (const row of this.sql.exec(
      `SELECT ${PUBLIC_WALL_ENTRY_COLUMNS} FROM donations WHERE id IN (${placeholders})`,
      ...winners.map(winner => winner.id),
    )) {
      const entry = rowToPublicWallEntry(row)
      byId.set(entry.id, entry)
    }
    const ordered: PublicWallEntry[] = []
    for (const winner of winners) {
      const full = byId.get(winner.id)
      if (full !== undefined) {
        ordered.push(full)
      }
    }
    return ordered
  }

  private persistSnapshotInTx(
    wallVersion: number,
    entries: readonly PublicWallEntry[],
    topSupporters: readonly PublicWallEntry[],
  ): WallSnapshot {
    const payload: PublicWallResponse = {
      version: wallVersion,
      entries,
      topSupporters,
    }
    const json = JSON.stringify(payload)
    const etag = computePublicWallEtag(wallVersion, json)
    const topSupportersJson = JSON.stringify(topSupporters)
    this.sql.exec(
      UPSERT_PUBLIC_WALL_PROJECTION_SQL,
      wallVersion,
      this.projectionFingerprint,
      topSupportersJson,
      json,
      etag,
    )
    return { version: wallVersion, etag, json }
  }
}
