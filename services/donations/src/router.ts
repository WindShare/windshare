import { ADMIN_APP_CSS, ADMIN_APP_JS, ADMIN_CSP, ADMIN_HTML } from './admin/assets'
import { parseAllowedOrigins, type DonationsEnv } from './env'

const PUBLIC_WALL_CACHE_CONTROL = 'public, max-age=15, stale-while-revalidate=60'
const DONATION_WALL_INSTANCE_NAME = 'global'

function jsonResponse(body: unknown, status = 200, extraHeaders?: Record<string, string>): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      'Content-Type': 'application/json; charset=utf-8',
      'X-Content-Type-Options': 'nosniff',
      ...extraHeaders,
    },
  })
}

function adminSecurityHeaders(contentType: string): Record<string, string> {
  return {
    'Content-Type': contentType,
    'Cache-Control': 'no-store',
    'Content-Security-Policy': ADMIN_CSP,
    'Referrer-Policy': 'no-referrer',
    'X-Content-Type-Options': 'nosniff',
  }
}

function handleAdminRoute(method: string, pathname: string): Response | null {
  if (method === 'GET' && (pathname === '/admin' || pathname === '/admin/')) {
    return new Response(ADMIN_HTML, { status: 200, headers: adminSecurityHeaders('text/html; charset=utf-8') })
  }
  if (method === 'GET' && pathname === '/admin/app.js') {
    return new Response(ADMIN_APP_JS, { status: 200, headers: adminSecurityHeaders('application/javascript; charset=utf-8') })
  }
  if (method === 'GET' && pathname === '/admin/app.css') {
    return new Response(ADMIN_APP_CSS, { status: 200, headers: adminSecurityHeaders('text/css; charset=utf-8') })
  }
  if (pathname.startsWith('/v1/admin/')) {
    return jsonResponse({ error: 'not_implemented' }, 501, {
      'Cache-Control': 'no-store',
      'Referrer-Policy': 'no-referrer',
    })
  }
  return null
}

async function handlePublicWall(request: Request, env: DonationsEnv): Promise<Response> {
  const stub = env.DONATION_WALL.get(env.DONATION_WALL.idFromName(DONATION_WALL_INSTANCE_NAME))
  const headers = new Headers()
  const ifNoneMatch = request.headers.get('If-None-Match')
  if (ifNoneMatch !== null) headers.set('If-None-Match', ifNoneMatch)

  const upstream = await stub.fetch('https://donation-wall.internal/internal/wall', {
    method: 'GET',
    headers,
  })
  const responseHeaders = new Headers(upstream.headers)
  responseHeaders.set('Access-Control-Allow-Origin', '*')
  responseHeaders.set('Cache-Control', PUBLIC_WALL_CACHE_CONTROL)
  responseHeaders.set('X-Content-Type-Options', 'nosniff')
  return new Response(upstream.body, {
    status: upstream.status,
    headers: responseHeaders,
  })
}

function handleCryptoPreflight(request: Request, env: DonationsEnv): Response {
  const origin = request.headers.get('Origin')
  const allowed = parseAllowedOrigins(env.ALLOWED_ORIGINS)
  if (origin === null || !allowed.has(origin)) {
    return new Response(null, { status: 403 })
  }
  return new Response(null, {
    status: 204,
    headers: {
      'Access-Control-Allow-Origin': origin,
      'Access-Control-Allow-Methods': 'POST, OPTIONS',
      'Access-Control-Allow-Headers': 'Content-Type',
      'Access-Control-Max-Age': '600',
      'Vary': 'Origin',
    },
  })
}

export async function handleWorkerFetch(request: Request, env: DonationsEnv): Promise<Response> {
  const url = new URL(request.url)
  const { method } = request
  const { pathname } = url

  if (method === 'GET' && pathname === '/healthz') {
    return jsonResponse({ ok: true, service: 'windshare-donations' }, 200, {
      'Cache-Control': 'no-store',
    })
  }
  if (method === 'GET' && pathname === '/v1/donations/wall') {
    return handlePublicWall(request, env)
  }
  if (method === 'OPTIONS' && pathname === '/v1/donations/crypto-intents') {
    return handleCryptoPreflight(request, env)
  }
  if (method === 'POST' && pathname === '/v1/donations/crypto-intents') {
    return jsonResponse({ error: 'not_implemented' }, 501, { 'Cache-Control': 'no-store' })
  }
  if (method === 'POST' && pathname.startsWith('/v1/donations/webhooks/')) {
    return jsonResponse({ error: 'not_implemented' }, 501, { 'Cache-Control': 'no-store' })
  }

  const adminResponse = handleAdminRoute(method, pathname)
  if (adminResponse !== null) return adminResponse

  return jsonResponse({ error: 'not_found' }, 404, { 'Cache-Control': 'no-store' })
}
