import { compare, directoryRevision, exceedsWireLimit } from './directory-revision.mjs';

const mirrors = new WeakMap();
const EDGE_TTL = 60;
const DIRECTORY_KEY = 'windshare-network';
const PUBLIC = { 'Access-Control-Allow-Origin': '*', 'X-Content-Type-Options': 'nosniff', 'Referrer-Policy': 'no-referrer' };

const usable = (revision, now) => Boolean(revision && revision.start <= now && now < revision.until);

function atLeast(candidate, floor) {
  if (!floor) return true;
  if (floor.network && candidate.network !== floor.network) return false;
  const order = compare(candidate, floor);
  return order > 0 || order === 0 && candidate.digest === floor.digest;
}

function sameRevision(a, b) {
  return compare(a, b) === 0
    && a.digest === b.digest
    && (!b.network || a.network === b.network);
}

function unavailable() {
  return new Response(null, { status: 503, headers: { ...PUBLIC, 'Cache-Control': 'no-store', 'Retry-After': '30' } });
}

async function asset(request, env) {
  const response = await env.ASSETS.fetch(new Request(request.url, { method: 'GET' }));
  if (response.status === 404) {
    await response.body?.cancel();
    return null;
  }
  if (!response.ok) {
    await response.body?.cancel();
    console.warn('directory_asset_fetch_failed', { status: response.status });
    throw new Error(`Bundled directory asset returned HTTP ${response.status}`);
  }
  // SPA fallback serves index.html with 200 when no directory asset was bundled.
  if (response.headers.get('Content-Type')?.toLowerCase().includes('text/html')) {
    await response.body?.cancel();
    return null;
  }
  return response.text();
}

async function readKv(env) {
  if (!env.DIRECTORY) return null;
  try {
    return await env.DIRECTORY.get(DIRECTORY_KEY, { cacheTtl: EDGE_TTL });
  } catch (error) {
    console.warn('directory_kv_read_failed', { error: String(error) });
    return null;
  }
}

async function kvCopy(state, raw) {
  if (typeof raw !== 'string' || !raw) {
    state.kv = null;
    return null;
  }
  // KV can keep returning the same revision for its entire edge TTL.
  if (state.kv?.raw === raw) return state.kv.copy;
  // Never retain oversized KV blobs in the isolate-scoped cache.
  if (exceedsWireLimit(raw)) {
    if (state.kv?.oversized !== raw.length) {
      console.warn('directory_kv_rejected', { reason: 'wire_limit', length: raw.length });
      state.kv = { oversized: raw.length };
    }
    return null;
  }
  let copy = null;
  try {
    copy = { body: raw, revision: await directoryRevision(raw) };
  } catch (error) {
    console.warn('directory_kv_rejected', { reason: 'invalid_revision', error: String(error) });
  }
  state.kv = { raw, copy };
  return copy;
}

async function bundled(env, request, state) {
  if (state.bundle !== undefined) return state.bundle;
  const body = await asset(request, env);
  // Memoize only definitive deployment facts (present or 404/SPA-absent); transient errors throw above.
  const copy = body === null ? null : { revision: await directoryRevision(body), body };
  state.bundle = copy;
  return copy;
}

async function bundledRevision(env, request, state) {
  // Production Workers know their immutable asset revision without reading the body.
  if (env.BUNDLED_DIRECTORY_REVISION !== undefined) return JSON.parse(env.BUNDLED_DIRECTORY_REVISION);
  return (await bundled(env, request, state))?.revision ?? null;
}

// Serve a stateless 60 s read-through mirror: clients independently verify signatures and
// enforce rollback protection, so the edge mirror avoids long-lived sticky state that
// cannot self-heal after a bad KV write.
export async function serveDirectory(request, env) {
  if (!['GET', 'HEAD'].includes(request.method)) {
    return new Response(null, { status: 405, headers: { ...PUBLIC, Allow: 'GET, HEAD', 'Cache-Control': 'no-store' } });
  }
  let state = mirrors.get(env);
  if (!state) {
    // Store only resolved plain data across requests; never share pending I/O promises across IoContexts.
    state = { bundle: undefined, kv: null };
    mirrors.set(env, state);
  }
  const [raw, bundle] = await Promise.all([
    readKv(env),
    bundledRevision(env, request, state).catch(() => null),
  ]);
  const remote = await kvCopy(state, raw);
  const now = Date.now();
  const floor = bundle && bundle.start <= now ? bundle : null;
  const useRemote = Boolean(remote && usable(remote.revision, now) && atLeast(remote.revision, floor));
  const useBundle = !useRemote && usable(bundle, now);
  const revision = useRemote ? remote.revision : useBundle ? bundle : null;
  if (!revision) return unavailable();
  let body = null;
  if (request.method === 'GET') {
    if (useRemote) body = remote.body;
    else {
      const copy = await bundled(env, request, state).catch(() => null);
      if (!copy || !sameRevision(copy.revision, revision)) return unavailable();
      body = copy.body;
    }
  }
  const ttl = Math.min(EDGE_TTL, Math.floor((revision.until - now) / 1000));
  return new Response(body, { headers: { ...PUBLIC, 'Content-Type': 'application/json', 'Cache-Control': 'public, max-age=' + ttl } });
}
