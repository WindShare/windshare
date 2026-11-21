import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readFileSync } from 'node:fs';
import { directoryRevision } from './directory-revision.mjs';
import { frontendConfig } from './configuration.mjs';
import { deployFrontend, readFrontendInputs } from './frontend.mjs';
import { STATE_BINDING } from './deployment.mjs';
import worker from './frontend-entry.mjs';
import { serveDirectory } from './directory-mirror.mjs';

const vectors = JSON.parse(readFileSync(new URL('../../../testdata/network/directory.json', import.meta.url), 'utf8'));
const signed = name => JSON.stringify(vectors.entries.find(entry => entry.name === name).envelope);
const inputs = readFrontendInputs({ CLOUDFLARE_ACCOUNT_ID: 'a'.repeat(32), CLOUDFLARE_WORKER_NAME: 'official-web' });
function cloud() {
  let settings = null;
  return { subdomain: async () => 'account', settings: async () => settings,
    route: async () => ({ enabled: true, previews_enabled: false }),
    retain: (state, kv = state.directory) => {
      settings = {
        bindings: [
          { name: STATE_BINDING, type: 'plain_text', text: JSON.stringify(state) },
          ...(kv ? [{ name: 'DIRECTORY', type: 'kv_namespace', namespace_id: kv }] : []),
        ],
      };
    },
  };
}
test('frontend creates and updates without credentials, namespaces or TURN', async () => {
  const cloudflare = cloud();
  const deploy = async plan => cloudflare.retain(plan.state);
  const first = await deployFrontend({ inputs, cloudflare, deploy });
  const next = await deployFrontend({ inputs, cloudflare, deploy });
  assert.equal(first.kind, 'create');
  assert.equal(next.kind, 'update');
  assert.equal(next.state.deploymentId, first.state.deploymentId);
  const config = frontendConfig(inputs);
  assert.equal(config.durable_objects, undefined);
  assert.equal(config.build, undefined);
  assert.equal(config.assets.binding, 'ASSETS');
});
test('frontend rejects existing relay storage and unrelated Workers before upload', async () => {
  for (const settings of [{ bindings: [{ type: 'durable_object_namespace' }] }, { bindings: [] }]) {
    const cloudflare = { ...cloud(), settings: async () => settings };
    await assert.rejects(deployFrontend({ inputs, cloudflare, deploy: () => assert.fail('must not upload') }));
  }
});
test('validated SaaS can attach to an owned frontend without moving either established domain', async () => {
  const primary = readFrontendInputs({ CLOUDFLARE_ACCOUNT_ID: 'a'.repeat(32), CLOUDFLARE_WORKER_NAME: 'official-web',
    CLOUDFLARE_ZONE_ID: 'b'.repeat(32), CLOUDFLARE_FRONTEND_DOMAIN: 'windshare.cc' });
  const linked = { ...primary, route: { ...primary.route, saasHostname: 'ws.ourworld.top' } };
  let attached = false;
  const cloudflare = { ...cloud(),
    zone: async () => ({ id: primary.route.zoneId, account: { id: primary.accountId }, name: 'windshare.cc', status: 'active' }),
    domains: async () => [{ hostname: primary.route.hostname, zone_id: primary.route.zoneId,
      service: primary.workerName, environment: 'production', enabled: true, previews_enabled: false }],
    route: async () => ({ enabled: false, previews_enabled: false }),
    customHostnames: async (_, hostname) => [{ hostname, status: 'active', ssl: { status: 'active' } }],
    workerRoutes: async () => attached ? [{ pattern: 'ws.ourworld.top/*', script: primary.workerName }] : [],
  };
  const deploy = async plan => { cloudflare.retain(plan.state); attached = !!plan.state.route.saasHostname; };
  const initial = await deployFrontend({ inputs: primary, cloudflare, deploy });
  const upgraded = await deployFrontend({ inputs: linked, cloudflare, deploy });
  assert.equal(upgraded.state.deploymentId, initial.state.deploymentId);
  assert.deepEqual(upgraded.state.route, linked.route);
  await assert.rejects(deployFrontend({ inputs: primary, cloudflare, deploy: () => assert.fail('must retain link host') }), /not this frontend/);
  const moved = { ...linked, route: { ...linked.route, saasHostname: 'other.ourworld.top' } };
  await assert.rejects(deployFrontend({ inputs: moved, cloudflare, deploy: () => assert.fail('must not rebind link host') }), /not this frontend/);
});
test('SaaS link hostname requires validated TLS and refuses another Worker route', async () => {
  const inputs = readFrontendInputs({ CLOUDFLARE_ACCOUNT_ID: 'a'.repeat(32), CLOUDFLARE_WORKER_NAME: 'official-web',
    CLOUDFLARE_ZONE_ID: 'b'.repeat(32), CLOUDFLARE_FRONTEND_DOMAIN: 'windshare.cc', CLOUDFLARE_FRONTEND_SAAS_HOST: 'ws.ourworld.top' });
  let active = false;
  const cloudflare = { ...cloud(),
    zone: async () => ({ id: inputs.route.zoneId, account: { id: inputs.accountId }, name: 'windshare.cc', status: 'active' }),
    domains: async () => [],
    customHostnames: async () => [{ hostname: 'ws.ourworld.top', status: 'active', ssl: { status: active ? 'active' : 'pending_validation' } }],
    workerRoutes: async () => [{ pattern: 'ws.ourworld.top/*', script: 'unrelated-worker' }],
  };
  const deploy = () => assert.fail('must not replace another route');
  await assert.rejects(deployFrontend({ inputs, cloudflare, deploy }), /certificate validation/);
  active = true;
  await assert.rejects(deployFrontend({ inputs, cloudflare, deploy }), /route must belong/);
  assert.deepEqual(frontendConfig(inputs).routes[1], { pattern: 'ws.ourworld.top/*', zone_id: inputs.route.zoneId });
});
test('frontend serves assets, refuses absent directory and redirects only versioned installer paths', async () => {
  const env = { ASSETS: { fetch: async () => new Response('page', { headers: { 'Content-Type': 'text/html' } }) } };
  assert.equal((await worker.fetch(new Request('https://ws.ourworld.top/share'), env)).status, 200);
  assert.equal((await worker.fetch(new Request('https://ws.ourworld.top/.well-known/windshare-network'), env)).status, 503);
  const download = await worker.fetch(new Request('https://ws.ourworld.top/downloads/v0.1.3/windshare-v0.1.3-windows-amd64.zip'), env);
  assert.equal(download.status, 302);
  assert.equal(download.headers.get('Location'), 'https://github.com/WindShare/windshare/releases/download/v0.1.3/windshare-v0.1.3-windows-amd64.zip');
  assert.equal((await worker.fetch(new Request('https://ws.ourworld.top/downloads/'), env)).status, 404);
  assert.equal((await worker.fetch(new Request('https://ws.ourworld.top/downloads/latest/windshare-v0.1.3-windows-amd64.zip'), env)).status, 404);
  assert.equal((await worker.fetch(new Request('https://ws.ourworld.top/downloads/v0.1.3/https:%2F%2Fevil'), env)).status, 404);
  assert.equal((await worker.fetch(new Request('https://ws.ourworld.top/downloads/v0.1.3/windshare-v0.1.3-windows-amd64.zip', { method: 'POST' }), env)).status, 405);
});

test('signed directory mirrors are readable from self-hosted frontends', async t => {
  t.mock.method(Date, 'now', () => Date.parse(vectors.now));
  const bytes = signed('valid');
  const response = await worker.fetch(new Request('https://windshare.cc/.well-known/windshare-network', {
    headers: { Origin: 'https://self-hosted.example' },
  }), { ASSETS: { fetch: async () => new Response(bytes) } });
  assert.equal(response.headers.get('Access-Control-Allow-Origin'), '*');
  assert.equal(response.headers.get('Content-Type'), 'application/json');
  assert.equal(await response.text(), bytes);
});

test('frontend post-deploy verification retries stale state and lagging SaaS routes', async () => {
  const primary = readFrontendInputs({ CLOUDFLARE_ACCOUNT_ID: 'a'.repeat(32), CLOUDFLARE_WORKER_NAME: 'official-web',
    CLOUDFLARE_ZONE_ID: 'b'.repeat(32), CLOUDFLARE_FRONTEND_DOMAIN: 'windshare.cc' });
  const linked = { ...primary, route: { ...primary.route, saasHostname: 'ws.ourworld.top' } };
  const base = cloud();
  let previousSettings = null, deployed = false, postSettingsCalls = 0, postRouteCalls = 0;
  const waits = [], events = [];
  const cloudflare = { ...base,
    settings: async () => {
      if (deployed) {
        postSettingsCalls++;
        if (postSettingsCalls === 1) return { bindings: [] };
        if (postSettingsCalls === 2) return previousSettings;
      }
      return base.settings();
    },
    zone: async () => ({ id: primary.route.zoneId, account: { id: primary.accountId }, name: 'windshare.cc', status: 'active' }),
    domains: async () => [{ hostname: primary.route.hostname, zone_id: primary.route.zoneId,
      service: primary.workerName, environment: 'production', enabled: true, previews_enabled: false }],
    route: async () => ({ enabled: false, previews_enabled: false }),
    customHostnames: async (_, hostname) => [{ hostname, status: 'active', ssl: { status: 'active' } }],
    workerRoutes: async () => {
      if (!deployed) return [];
      return ++postRouteCalls === 1 ? [] : [{ pattern: 'ws.ourworld.top/*', script: primary.workerName }];
    },
  };
  await deployFrontend({ inputs: primary, cloudflare, deploy: async plan => base.retain(plan.state) });
  previousSettings = await base.settings();
  const upgraded = await deployFrontend({
    inputs: linked, cloudflare,
    deploy: async plan => { deployed = true; base.retain(plan.state); },
    log: (event, fields) => events.push({ event, ...fields }),
    wait: async ms => waits.push(ms),
    verificationDelaysMs: [10, 20, 40],
  });
  assert.deepEqual(upgraded.state.route, linked.route);
  assert.deepEqual(waits, [10, 20, 10]);
  assert.deepEqual(events.filter(e => e.event === 'deployment_verification_retry').map(e => e.reason), [
    'missing_binding:WINDSHARE_DEPLOYMENT',
    'stale_frontend_state',
    'saas_route_pending',
  ]);
});
test('published mirror compares signed revisions and only persists a changed copy', async t => {
  let now = Date.parse(vectors.now);
  t.mock.method(Date, 'now', () => now);
  const baseline = signed('valid'), newer = signed('new_generation');
  let current = signed('older_serial'), saved, writes = 0, reads = 0;
  const cache = { async put(key, value) { writes++; saved = await value.text(); },
    async match() { return saved && new Response(saved); } };
  const pending = [];
  const context = { waitUntil(promise) { pending.push(promise); } };
  const env = { BUNDLED_DIRECTORY_REVISION: JSON.stringify(await directoryRevision(baseline)),
    DIRECTORY: { get(key, options) { assert.deepEqual(options, { cacheTtl: 60 }); return current; } },
    ASSETS: { fetch: async () => { reads++; return new Response(baseline); } } };
  const request = new Request('https://windshare.cc/.well-known/windshare-network');
  const serve = (req = request, bindings = env) => serveDirectory(req, bindings, context, cache);
  assert.equal(await (await serve()).text(), baseline);
  current = newer;
  assert.equal(await (await serve()).text(), newer);
  assert.equal(await (await serve(new Request(request, { method: 'HEAD' }))).text(), '');
  await Promise.all(pending);
  assert.equal(writes, 1);
  assert.equal(reads, 1);
  current = null;
  assert.equal(await (await serve(request, { ...env })).text(), newer);
  now = (await directoryRevision(newer)).until + 1;
  const unavailable = await serve();
  assert.equal(unavailable.status, 503);
  assert.equal(unavailable.headers.get('cache-control'), 'no-store');
});
test('a temporary Cache API failure retries the same live revision after the edge TTL', async t => {
  let now = Date.parse(vectors.now), writes = 0, pending;
  t.mock.method(Date, 'now', () => now);
  t.mock.method(console, 'warn', () => {});
  const bytes = signed('valid');
  const env = { DIRECTORY: { get: async () => bytes }, BUNDLED_DIRECTORY_REVISION: 'null' };
  const cache = { match: async () => null, async put() { if (++writes === 1) throw new Error('temporarily unavailable'); } };
  const ctx = { waitUntil(promise) { pending = promise; } };
  const request = new Request('https://windshare.cc/.well-known/windshare-network');
  const read = async () => {
    assert.equal(await (await serveDirectory(request, env, ctx, cache)).text(), bytes);
    await pending;
  };
  await read();
  await read();
  assert.equal(writes, 1);
  now += 60_000;
  await read();
  await read();
  assert.equal(writes, 2);
});
test('mirror isolates request I/O promises, merges cold restores monotonically, and isolates cache writes', async t => {
  t.mock.method(Date, 'now', () => Date.parse(vectors.now));
  const baseline = signed('valid'), newer = signed('new_generation');
  const request = new Request('https://windshare.cc/.well-known/windshare-network');

  // 1. A stalled/canceled request on cold start must not poison subsequent requests' restore or bundle fetch.
  let matchCalls = 0, assetCalls = 0;
  const cache = {
    match: () => (++matchCalls === 1 ? new Promise(() => {}) : Promise.resolve(null)),
    put: async () => {},
  };
  const env = {
    DIRECTORY: { get: async () => null },
    ASSETS: { fetch: () => (++assetCalls === 1 ? new Promise(() => {}) : Promise.resolve(new Response(baseline))) },
  };
  // Request 1 hangs forever inside its own cache.match and ASSETS.fetch (like an aborted workerd IoContext).
  const req1 = serveDirectory(request, env, null, cache);
  const req2 = await serveDirectory(request, env, null, cache);
  assert.equal(req2.status, 200);
  assert.equal(await req2.text(), baseline);
  // Request 3 hits cached plain data without re-running cache.match or ASSETS.fetch.
  const req3 = await serveDirectory(request, env, null, cache);
  assert.equal(req3.status, 200);
  assert.equal(await req3.text(), baseline);
  assert.equal(matchCalls, 2);
  assert.equal(assetCalls, 2);
  void req1;

  // 2. Late cold-start restore resolving with an older revision must not downgrade a newer KV revision.
  let releaseSlowRestore, mergeMatchCalls = 0, mergeKv = newer;
  const slowRestore = new Promise(resolve => { releaseSlowRestore = resolve; });
  const mergeCache = {
    match: () => (++mergeMatchCalls === 1 ? slowRestore : Promise.resolve(null)),
    put: async () => {},
  };
  const mergeEnv = {
    DIRECTORY: { get: async () => mergeKv },
    BUNDLED_DIRECTORY_REVISION: 'null',
  };
  const slowReq = serveDirectory(request, mergeEnv, null, mergeCache);
  const fastReq = await serveDirectory(request, mergeEnv, null, mergeCache);
  assert.equal(await fastReq.text(), newer);
  releaseSlowRestore(new Response(baseline));
  assert.equal(await (await slowReq).text(), newer);
  mergeKv = null;
  assert.equal(await (await serveDirectory(request, mergeEnv, null, mergeCache)).text(), newer);

  // 3. A stalled waitUntil cache.put on an earlier revision must not block persisting a newer revision.
  let putCalls = 0, persisted = null, writeKv = baseline;
  const writeEnv = {
    DIRECTORY: { get: async () => writeKv },
    BUNDLED_DIRECTORY_REVISION: 'null',
  };
  const writeCache = {
    match: async () => null,
    put: async (key, value) => {
      if (++putCalls === 1) return new Promise(() => {});
      persisted = await value.text();
    },
  };
  const waits = [];
  const ctx = { waitUntil(promise) { waits.push(promise); } };
  assert.equal(await (await serveDirectory(request, writeEnv, ctx, writeCache)).text(), baseline);
  writeKv = newer;
  assert.equal(await (await serveDirectory(request, writeEnv, ctx, writeCache)).text(), newer);
  await waits[1];
  assert.equal(putCalls, 2);
  assert.equal(persisted, newer);
});

test('frontend inputs normalize optional KV bindings and reject malformed IDs', () => {
  const account = { CLOUDFLARE_ACCOUNT_ID: 'a'.repeat(32), CLOUDFLARE_WORKER_NAME: 'official-web' };
  assert.equal(readFrontendInputs({ ...account, DIRECTORY_KV_ID: ' ' }).directory, undefined);
  assert.equal(readFrontendInputs({ ...account, DIRECTORY_KV_ID: ' ' + 'c'.repeat(32) + ' ' }).directory, 'c'.repeat(32));
  assert.throws(() => readFrontendInputs({ ...account, DIRECTORY_KV_ID: 'oops' }), /KV/);
});

test('frontend updates converge directory KV attachment, rotation, and removal', async () => {
  const account = { CLOUDFLARE_ACCOUNT_ID: 'a'.repeat(32), CLOUDFLARE_WORKER_NAME: 'official-web' };
  const firstKv = 'c'.repeat(32);
  const rotatedKv = 'd'.repeat(32);
  const plain = readFrontendInputs(account);
  const attached = readFrontendInputs({ ...account, DIRECTORY_KV_ID: firstKv });
  const rotated = readFrontendInputs({ ...account, DIRECTORY_KV_ID: rotatedKv });
  const base = cloud();
  const events = [];
  let pendingSettings = [];
  const cloudflare = {
    ...base,
    settings: async () => pendingSettings.length > 0 ? pendingSettings.shift() : base.settings(),
  };
  const initial = await deployFrontend({ inputs: plain, cloudflare, deploy: async plan => base.retain(plan.state) });
  const priorSettings = await base.settings();

  const upgraded = await deployFrontend({
    inputs: attached, cloudflare,
    deploy: async plan => {
      // Simulate Cloudflare returning stale state, then updated state without the KV binding yet.
      pendingSettings = [
        priorSettings,
        { bindings: [{ name: STATE_BINDING, type: 'plain_text', text: JSON.stringify(plan.state) }] },
      ];
      base.retain(plan.state);
    },
    log: (event, fields) => events.push({ event, ...fields }),
    wait: async () => {},
    verificationDelaysMs: [10, 20, 40],
  });
  assert.equal(upgraded.kind, 'update');
  assert.equal(upgraded.state.deploymentId, initial.state.deploymentId);
  assert.equal(upgraded.state.directory, firstKv);
  assert.deepEqual(events.filter(e => e.event === 'deployment_verification_retry').map(e => e.reason), [
    'stale_frontend_state',
    'missing_binding:DIRECTORY',
  ]);

  const moved = await deployFrontend({
    inputs: rotated, cloudflare,
    deploy: async plan => {
      pendingSettings = [
        { bindings: [{ name: STATE_BINDING, type: 'plain_text', text: JSON.stringify(plan.state) },
          { name: 'DIRECTORY', type: 'kv_namespace', namespace_id: firstKv }] },
      ];
      base.retain(plan.state);
    },
    log: (event, fields) => events.push({ event, ...fields }),
    wait: async () => {},
    verificationDelaysMs: [10, 20],
  });
  assert.equal(moved.state.directory, rotatedKv);
  assert.equal(events.filter(e => e.event === 'deployment_verification_retry').at(-1)?.reason, 'directory_binding_pending');

  const detached = await deployFrontend({
    inputs: plain, cloudflare,
    deploy: async plan => {
      pendingSettings = [
        { bindings: [{ name: STATE_BINDING, type: 'plain_text', text: JSON.stringify(plan.state) },
          { name: 'DIRECTORY', type: 'kv_namespace', namespace_id: rotatedKv }] },
      ];
      base.retain(plan.state);
    },
    wait: async () => {},
    verificationDelaysMs: [10, 20],
  });
  assert.equal(detached.state.directory, undefined);
});
