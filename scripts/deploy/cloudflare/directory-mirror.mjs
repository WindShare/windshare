import { compare, directoryRevision } from './directory-revision.mjs';

const mirrors = new WeakMap();
const EDGE_TTL = 60;
const PUBLIC = { 'Access-Control-Allow-Origin': '*', 'X-Content-Type-Options': 'nosniff', 'Referrer-Policy': 'no-referrer' };
const usable = (revision, now) => revision && revision.start <= now && now < revision.until;
function atLeast(candidate, floor) {
  if (!floor) return true;
  const order = compare(candidate, floor);
  return order > 0 || order === 0 && candidate.digest === floor.digest;
}
function unavailable() {
  return new Response(null, { status: 503, headers: { ...PUBLIC, 'Cache-Control': 'no-store', 'Retry-After': '30' } });
}

async function asset(request, env) {
  const response = await env.ASSETS.fetch(new Request(request.url, { method: 'GET' }));
  if (!response.ok || response.headers.get('Content-Type')?.includes('text/html')) {
    await response.body?.cancel();
    return null;
  }
  return response.text();
}

function kvCopy(state, raw) {
  if (!raw || raw === state.last?.body) return null;
  // KV can keep returning the same stale revision for its entire edge TTL.
  if (state.kv?.raw !== raw) state.kv = { raw, parsed: directoryRevision(raw).then(revision => ({ body: raw, revision }), () => null) };
  return state.kv.parsed;
}

async function restore(cache, key) {
  const saved = await cache?.match(key);
  if (!saved) return null;
  const body = await saved.text();
  return { body, revision: await directoryRevision(body) };
}

function bundled(env, request, state) {
  state.bundle ??= asset(request, env).then(async body => body && { revision: await directoryRevision(body), body })
    .catch(error => { state.bundle = undefined; throw error; });
  return state.bundle;
}
async function bundledRevision(env, request, state) {
  // Production Workers know their immutable asset revision without reading the body.
  if (env.BUNDLED_DIRECTORY_REVISION !== undefined) return JSON.parse(env.BUNDLED_DIRECTORY_REVISION);
  return (await bundled(env, request, state))?.revision;
}

function persist(context, cache, key, state, copy, now) {
  if (!context || !cache || copy.retryAt === undefined || now < copy.retryAt) return;
  copy.retryAt = undefined;
  // Serializing the rare changed revisions prevents concurrent puts completing in reverse order.
  state.writing = state.writing.then(() => cache.put(key, new Response(copy.body, {
    headers: { 'Cache-Control': 'public, max-age=' + Math.ceil((copy.revision.until - now) / 1000) },
  }))).catch(error => {
    copy.retryAt = Date.now() + EDGE_TTL * 1000;
    console.warn('directory_cache_write_failed', { generation: copy.revision.generation, serial: copy.revision.serial, error: String(error) });
  });
  context.waitUntil(state.writing);
}

export async function serveDirectory(request, env, context, cache = globalThis.caches?.default) {
  if (!['GET', 'HEAD'].includes(request.method)) return new Response(null, { status: 405, headers: { ...PUBLIC, Allow: 'GET, HEAD', 'Cache-Control': 'no-store' } });
  const key = new URL('/.windshare-cache/last-directory', request.url).href;
  let state = mirrors.get(env);
  if (!state) {
    state = { last: null, writing: Promise.resolve() };
    mirrors.set(env, state);
    state.ready = restore(env.DIRECTORY && cache, key).then(copy => { state.last = copy; }).catch(() => {});
  }
  const [, raw, bundle] = await Promise.all([state.ready,
    Promise.resolve().then(() => env.DIRECTORY?.get('windshare-network', { cacheTtl: EDGE_TTL })).catch(() => null),
    bundledRevision(env, request, state).catch(() => null),
  ]);
  const remote = await kvCopy(state, raw);
  const now = Date.now();
  const floor = bundle && bundle.start <= now ? bundle : null;
  if (remote && usable(remote.revision, now) && atLeast(remote.revision, floor) &&
      (!state.last || compare(remote.revision, state.last.revision) > 0)) {
    state.last = { ...remote, retryAt: 0 };
  }
  const last = state.last;
  const useLast = usable(last?.revision, now) && atLeast(last.revision, floor);
  // Expiry must not reopen a retired revision that this isolate already advanced past.
  const useBundle = usable(bundle, now) && (!last || compare(bundle, last.revision) >= 0);
  const revision = useLast ? last.revision : useBundle ? bundle : null;
  if (!revision) return unavailable();
  if (useLast) persist(context, cache, key, state, last, now);
  let body = null;
  if (request.method === 'GET') {
    if (useLast) body = last.body;
    else {
      const copy = await bundled(env, request, state).catch(() => null);
      if (!copy || compare(copy.revision, revision) !== 0 || copy.revision.digest !== revision.digest) return unavailable();
      body = copy.body;
    }
  }
  const ttl = Math.min(EDGE_TTL, Math.floor((revision.until - now) / 1000));
  return new Response(body, { headers: { ...PUBLIC, 'Content-Type': 'application/json', 'Cache-Control': 'public, max-age=' + ttl } });
}
