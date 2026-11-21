const HEADERS = { 'Access-Control-Allow-Origin': '*', 'Cache-Control': 'public, max-age=60', 'Content-Type': 'application/json' };
const GRACE_MS = 7 * 24 * 60 * 60 * 1000;

function expiry(encoded) {
  const bytes = atob(encoded.replaceAll('-', '+').replaceAll('_', '/'));
  return Date.parse(JSON.parse(bytes).expires_at);
}
function response(request, body) {
  return new Response(request.method === 'HEAD' ? null : body, { headers: HEADERS });
}

export async function serveDirectory(request, env, context, cache = globalThis.caches?.default) {
  if (!['GET', 'HEAD'].includes(request.method)) return new Response(null, { status: 405, headers: { Allow: 'GET, HEAD' } });
  const key = new URL('/.windshare-cache/last-directory', request.url).href;
  if (env.DIRECTORY) {
    try {
      // KV contains only authority-verified publications. The signed payload and grant
      // bound the lifetime of the backup copy even when a region loses access to KV.
      const current = await env.DIRECTORY.get('windshare-network', { cacheTtl: 60 });
      if (current) {
        const envelope = JSON.parse(current);
        const until = Math.min(expiry(envelope.payload) + GRACE_MS, expiry(envelope.certificate.payload));
        const ttl = Math.floor((until - Date.now()) / 1000);
        if (ttl > 0) {
          if (cache && context) context.waitUntil(cache.put(key, new Response(current, {
            headers: { 'Cache-Control': 'public, max-age=' + ttl, 'X-WindShare-Trusted-Until': String(until) },
          })).catch(() => {}));
          return response(request, current);
        }
      }
    } catch { /* Retain the last trusted copy on a regional KV outage. */ }
    const saved = await cache?.match(key).catch(() => undefined);
    if (saved && Number(saved.headers.get('X-WindShare-Trusted-Until')) > Date.now()) return response(request, saved.body);
  }
  const bundled = await env.ASSETS.fetch(request);
  if (!bundled.ok || bundled.headers.get('content-type')?.includes('text/html')) {
    return new Response(null, { status: 503, headers: { ...HEADERS, 'Retry-After': '30' } });
  }
  return response(request, bundled.body);
}
