import { ADMIN_CSP } from './admin/assets'

export const MAX_CRYPTO_INTENT_BODY_BYTES = 8 * 1024
export const MAX_WEBHOOK_BODY_BYTES = 64 * 1024
export const MAX_ADMIN_BODY_BYTES = 8 * 1024
export const DONATION_WALL_INSTANCE_NAME = 'global'
// __Host- binds the admin session cookie to this host (Secure, Path=/, no Domain)
// so sibling *.windshare.cc origins cannot shadow it via a Domain=.windshare.cc cookie.
export const ADMIN_COOKIE_NAME = '__Host-windshare_donate_admin'

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

export function getClientIp(request: Request): string {
  const cfIp = request.headers.get('CF-Connecting-IP')?.trim()
  if (cfIp !== undefined && cfIp.length > 0) return cfIp
  return 'unknown'
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
