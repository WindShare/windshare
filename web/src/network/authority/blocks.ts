import { encodeBase64Url } from '../../crypto/bytes'
import { sha256 } from '../../crypto/digest'
import { object, strictJSON } from '../strict-json'
import { CLOCK_SKEW_SECONDS, unsigned, verifyGeneration, verifySigned, type AuthorityGeneration } from './trust'

export const MAX_BLOCK_ENVELOPE_BYTES = 8 * 1024 * 1024
const MAX_BLOCK_IDENTITIES = 100_000
const FRESH_SNAPSHOT_SECONDS = 60 * 60
const encoder = new TextEncoder()
const decoder = new TextDecoder('utf-8', { fatal: true })

export interface BlockCursor {
  readonly generation: number; readonly certificate: string; readonly version: number; readonly digest: string
}
export interface BlockSnapshot {
  readonly cursor: BlockCursor; readonly authority: AuthorityGeneration; readonly issuedAt: number
  readonly shares: ReadonlySet<string>; readonly accounts: ReadonlySet<string>; readonly envelope: string
}

function identities(value: unknown, width: number): ReadonlySet<string> {
  if (!Array.isArray(value) || value.length > MAX_BLOCK_IDENTITIES) throw new TypeError('Invalid authority block list')
  const result = new Set<string>()
  for (const raw of value as unknown[]) {
    if (!Array.isArray(raw) || raw.length !== width || raw.some(byte => typeof byte !== 'number' || !Number.isInteger(byte) || byte < 0 || byte > 255)) throw new TypeError('Invalid blocked identity')
    const bytes = Uint8Array.from(raw as number[])
    const id = encodeBase64Url(bytes)
    if (bytes.every(byte => byte === 0) || result.has(id)) throw new TypeError('Duplicate or empty blocked identity')
    result.add(id)
  }
  return result
}

export async function verifyBlocks(envelope: string, root: string): Promise<BlockSnapshot> {
  if (encoder.encode(envelope).length > MAX_BLOCK_ENVELOPE_BYTES) throw new TypeError('Authority block snapshot too large')
  const signed = object(strictJSON(envelope), ['certificate', 'body'])
  if (typeof signed.certificate !== 'string') throw new TypeError('Missing authority certificate')
  const authority = await verifyGeneration(signed.certificate, root)
  const bytes = await verifySigned(signed.body, MAX_BLOCK_ENVELOPE_BYTES, 'windshare/public-blocks/v1\0', authority.key)
  const payload = object(strictJSON(decoder.decode(bytes)), ['version', 'shares', 'accounts', 'issued_at'])
  const version = unsigned(payload.version), issuedAt = unsigned(payload.issued_at)
  if (issuedAt < authority.notBefore || issuedAt >= authority.expiresAt) throw new TypeError('Block snapshot outside authority lifetime')
  const shares = identities(payload.shares, 12), accounts = identities(payload.accounts, 16)
  // Refreshing the signature time does not create a new policy revision.
  const digest = encodeBase64Url(await sha256(encoder.encode(JSON.stringify([[...shares].sort(), [...accounts].sort()]))))
  const cursor = Object.freeze({ generation: authority.number, certificate: signed.certificate, version, digest })
  return Object.freeze({ cursor, authority, issuedAt, shares, accounts, envelope })
}

export function freshBlocks(snapshot: BlockSnapshot, now: number): boolean {
  const seconds = now / 1000
  return snapshot.issuedAt <= seconds + CLOCK_SKEW_SECONDS && seconds < snapshot.issuedAt + FRESH_SNAPSHOT_SECONDS &&
    snapshot.authority.notBefore <= seconds + CLOCK_SKEW_SECONDS && seconds < snapshot.authority.expiresAt
}

export function rejectBlockRollback(previous: BlockCursor | undefined, next: BlockCursor): void {
  if (previous && (next.generation < previous.generation ||
    next.generation === previous.generation && next.certificate !== previous.certificate ||
    next.version < previous.version || next.version === previous.version && next.digest !== previous.digest)) {
    throw new Error('Authority policy rollback or equivocation')
  }
}
