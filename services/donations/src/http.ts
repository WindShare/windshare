import { ADMIN_CSP } from './admin/assets'
import type { DonationWallRpc, DonationsEnv } from './env'

export const MAX_CRYPTO_INTENT_BODY_BYTES = 8 * 1024
export const MAX_WEBHOOK_BODY_BYTES = 64 * 1024
export const MAX_ADMIN_BODY_BYTES = 8 * 1024
export const DONATION_WALL_INSTANCE_NAME = 'global'
// __Host- binds the admin session cookie to this host (Secure, Path=/, no Domain)
// so sibling *.windshare.cc origins cannot shadow it via a Domain=.windshare.cc cookie.
export const ADMIN_COOKIE_NAME = '__Host-windshare_donate_admin'

export function getDonationWallStub(env: DonationsEnv): DurableObjectStub<DonationWallRpc> {
  return env.DONATION_WALL.get(env.DONATION_WALL.idFromName(DONATION_WALL_INSTANCE_NAME))
}

export type BodyReadResult =
  | { readonly ok: true; readonly text: string }
  | { readonly ok: false; readonly reason: 'body_too_large' }

export function jsonResponse(
  body: unknown,
  status = 200,
  extraHeaders?: Record<string, string>,
): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      'Content-Type': 'application/json; charset=utf-8',
      'X-Content-Type-Options': 'nosniff',
      ...extraHeaders,
    },
  })
}

export function adminSecurityHeaders(contentType: string): Record<string, string> {
  return {
    'Content-Type': contentType,
    'Cache-Control': 'no-store',
    'Content-Security-Policy': ADMIN_CSP,
    'Referrer-Policy': 'no-referrer',
    'X-Content-Type-Options': 'nosniff',
  }
}

export function adminJsonResponse(
  body: unknown,
  status = 200,
  extraHeaders?: Record<string, string>,
): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      ...adminSecurityHeaders('application/json; charset=utf-8'),
      ...extraHeaders,
    },
  })
}

export function hasMediaType(request: Request, expectedMime: string): boolean {
  const raw = request.headers.get('Content-Type')
  if (raw === null) return false
  const mainType = raw.split(';', 1)[0]?.trim().toLowerCase()
  return mainType === expectedMime.toLowerCase()
}

export async function readBoundedText(
  request: Request,
  maxBytes: number,
): Promise<BodyReadResult> {
  const declaredLength = request.headers.get('Content-Length')
  if (declaredLength !== null) {
    const parsedLength = Number(declaredLength)
    if (Number.isFinite(parsedLength) && parsedLength > maxBytes) {
      return { ok: false, reason: 'body_too_large' }
    }
  }

  if (request.body === null) {
    return { ok: true, text: '' }
  }

  const reader = request.body.getReader()
  const chunks: Uint8Array[] = []
  let totalBytes = 0

  try {
    while (true) {
      const { done, value } = await reader.read()
      if (done) break
      totalBytes += value.byteLength
      if (totalBytes > maxBytes) {
        await reader.cancel()
        return { ok: false, reason: 'body_too_large' }
      }
      chunks.push(value)
    }
  } finally {
    reader.releaseLock()
  }

  const combined = new Uint8Array(totalBytes)
  let offset = 0
  for (const chunk of chunks) {
    combined.set(chunk, offset)
    offset += chunk.byteLength
  }
  return { ok: true, text: new TextDecoder().decode(combined) }
}

export function parseCookie(header: string | null, name: string): string | null {
  if (header === null || header.length === 0) return null
  for (const part of header.split(';')) {
    const eqIndex = part.indexOf('=')
    if (eqIndex <= 0) continue
    const key = part.slice(0, eqIndex).trim()
    if (key === name) {
      const value = part.slice(eqIndex + 1).trim()
      return value.length > 0 ? value : null
    }
  }
  return null
}

const IPV4_OCTET_PATTERN = /^(?:0|[1-9]\d{0,2})$/
const IPV6_HEXTET_PATTERN = /^[0-9a-f]{1,4}$/

function parseIpv4ToHextets(raw: string): readonly [number, number] | null {
  const parts = raw.split('.')
  if (parts.length !== 4) return null
  const octets: number[] = []
  for (const part of parts) {
    if (!IPV4_OCTET_PATTERN.test(part)) return null
    const value = Number(part)
    if (value > 255) return null
    octets.push(value)
  }
  const [a, b, c, d] = octets as [number, number, number, number]
  return [(a << 8) | b, (c << 8) | d]
}

function parseIpv6Segment(segment: string): number[] | null {
  if (segment.length === 0) return []
  const parts = segment.split(':')
  const values: number[] = []
  for (let i = 0; i < parts.length; i += 1) {
    const part = parts[i] ?? ''
    if (i === parts.length - 1 && part.includes('.')) {
      const embedded = parseIpv4ToHextets(part)
      if (embedded === null) return null
      values.push(embedded[0], embedded[1])
      continue
    }
    if (!IPV6_HEXTET_PATTERN.test(part)) return null
    values.push(Number.parseInt(part, 16))
  }
  return values
}

function parseIpv6Hextets(raw: string): number[] | null {
  const withoutZone = raw.split('%', 1)[0]?.toLowerCase() ?? ''
  const halves = withoutZone.split('::')
  if (halves.length > 2) return null

  const left = parseIpv6Segment(halves[0] ?? '')
  if (left === null) return null

  if (halves.length === 1) {
    return left.length === 8 ? left : null
  }
  // Embedded IPv4 notation is only legal at the end of an IPv6 address.
  if (halves[0]?.includes('.')) return null

  const right = parseIpv6Segment(halves[1] ?? '')
  if (right === null) return null

  const missing = 8 - left.length - right.length
  if (missing < 1) return null
  return [...left, ...Array<number>(missing).fill(0), ...right]
}

function isIpv4MappedIpv6(groups: readonly number[]): boolean {
  return groups[0] === 0
    && groups[1] === 0
    && groups[2] === 0
    && groups[3] === 0
    && groups[4] === 0
    && groups[5] === 0xffff
}

export function normalizeClientIp(raw: string): string {
  const trimmed = raw.trim()
  if (trimmed.length === 0) return 'unknown'
  if (!trimmed.includes(':')) return trimmed

  const groups = parseIpv6Hextets(trimmed)
  if (groups === null) return trimmed.toLowerCase()

  // Unwrap IPv4-mapped IPv6 (::ffff:a.b.c.d) so mapped callers keep distinct IPv4 buckets
  // rather than collapsing into the all-zero ::/64 prefix.
  if (isIpv4MappedIpv6(groups)) {
    const hi = groups[6] ?? 0
    const lo = groups[7] ?? 0
    return `${hi >> 8}.${hi & 0xff}.${lo >> 8}.${lo & 0xff}`
  }

  // Bucket IPv6 callers by /64 prefix so host-bit rotation within a single allocation
  // cannot bypass per-IP rate limits or bloat SQLite rate_limits storage.
  const prefix = groups
    .slice(0, 4)
    .map(group => group.toString(16).padStart(4, '0'))
    .join(':')
  return `${prefix}::/64`
}

export function getClientIp(request: Request): string {
  const cfIp = request.headers.get('CF-Connecting-IP')
  if (cfIp === null) return 'unknown'
  return normalizeClientIp(cfIp)
}

export function isSameOriginRequest(request: Request, expectedOrigin: string): boolean {
  const origin = request.headers.get('Origin')?.trim()
  return origin !== undefined && origin === expectedOrigin
}

export function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

export function parseJsonObject(text: string): Record<string, unknown> | null {
  try {
    const parsed: unknown = JSON.parse(text)
    return isPlainObject(parsed) ? parsed : null
  } catch {
    return null
  }
}
