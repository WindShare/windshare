import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readFileSync } from 'node:fs';
import { directoryRevision, MAX_BYTES } from './directory-revision.mjs';
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
test('published mirror serves newer KV revisions with a 60s TTL and falls back cleanly on outage', async t => {
  let now = Date.parse(vectors.now);
  t.mock.method(Date, 'now', () => now);
  const warnings = [];
  t.mock.method(console, 'warn', (event, fields) => warnings.push({ event, ...fields }));
  const baseline = signed('valid'), newer = signed('new_generation');
  let current = signed('older_serial'), kvError = null, reads = 0;
  const env = {
    BUNDLED_DIRECTORY_REVISION: JSON.stringify(await directoryRevision(baseline)),
    DIRECTORY: {
      async get(key, options) {
        assert.equal(key, 'windshare-network');
        assert.deepEqual(options, { cacheTtl: 60 });
        if (kvError) throw kvError;
        return current;
      },
    },
    ASSETS: { fetch: async () => { reads++; return new Response(baseline); } },
  };
  const request = new Request('https://windshare.cc/.well-known/windshare-network');
  const serve = (req = request, bindings = env) => serveDirectory(req, bindings);

  assert.equal(await (await serve()).text(), baseline);
  current = newer;
  const live = await serve();
  assert.equal(await live.text(), newer);
  assert.equal(live.headers.get('cache-control'), 'public, max-age=60');
  assert.equal(await (await serve(new Request(request, { method: 'HEAD' }))).text(), '');
  assert.equal(reads, 1);

  // Clearing KV or encountering a KV outage falls back to the valid bundled snapshot without sticky state.
  current = null;
  assert.equal(await (await serve()).text(), baseline);
  kvError = new Error('kv outage');
  assert.equal(await (await serve()).text(), baseline);
  kvError = null;
  assert.equal(warnings.filter(w => w.event === 'directory_kv_read_failed').length, 1);

  // Once both KV and bundled revisions expire, the mirror returns an uncached 503.
  current = newer;
  now = (await directoryRevision(newer)).until + 1;
  const unavailable = await serve();
  assert.equal(unavailable.status, 503);
  assert.equal(unavailable.headers.get('cache-control'), 'no-store');
  assert.equal(unavailable.headers.get('retry-after'), '30');
});

test('mirror self-heals immediately after a bad high-revision KV write without locking out valid snapshots', async t => {
  let now = Date.parse(vectors.now);
  t.mock.method(Date, 'now', () => now);
  const baseline = signed('valid'), newer = signed('new_generation'), expiredHigh = signed('expired_generation');
  const encodeJson = value => Buffer.from(JSON.stringify(value)).toString('base64url');
  const forgedHigh = JSON.stringify({
    payload: encodeJson({
      format: 1, network: 'test', generation: 999, serial: 1,
      issued_at: '2026-11-12T23:00:00Z', expires_at: '2026-11-14T00:00:00Z',
    }),
    certificate: {
      root: 'primary',
      payload: encodeJson({
        format: 1, network: 'test', generation: 999, key: 'forged',
        not_before: '2026-11-12T00:00:00Z', expires_at: '2026-12-13T00:00:00Z',
      }),
      signature: 'bad',
    },
    signature: 'bad',
  });
  let current = forgedHigh;
  const env = {
    BUNDLED_DIRECTORY_REVISION: JSON.stringify(await directoryRevision(baseline)),
    DIRECTORY: { get: async () => current },
    ASSETS: { fetch: async () => new Response(baseline) },
  };
  const request = new Request('https://windshare.cc/.well-known/windshare-network');

  // Even if an unverified high revision enters KV, responses stay short-cached and never latch monotonically.
  const poisoned = await serveDirectory(request, env);
  assert.equal(await poisoned.text(), forgedHigh);
  assert.equal(poisoned.headers.get('cache-control'), 'public, max-age=60');

  // Restoring KV to the real lower generation or clearing KV immediately recovers.
  current = newer;
  assert.equal(await (await serveDirectory(request, env)).text(), newer);
  current = null;
  assert.equal(await (await serveDirectory(request, env)).text(), baseline);

  // An expired high-revision KV copy never locks out a still-usable bundled snapshot.
  now = (await directoryRevision(expiredHigh)).until + 1;
  current = expiredHigh;
  assert.equal(await (await serveDirectory(request, env)).text(), baseline);
});

test('directoryRevision enforces structural bounds and rejects mismatched KV networks', async t => {
  t.mock.method(Date, 'now', () => Date.parse(vectors.now));
  const warnings = [];
  t.mock.method(console, 'warn', (event, fields) => warnings.push({ event, ...fields }));
  const encodeJson = value => Buffer.from(JSON.stringify(value)).toString('base64url');
  const envelope = (payload, grant) => JSON.stringify({
    payload: encodeJson(payload),
    certificate: { root: 'primary', payload: encodeJson(grant), signature: 'sig' },
    signature: 'sig',
  });
  const baseGrant = {
    format: 1, network: 'test', generation: 1, key: 'key',
    not_before: '2026-11-12T00:00:00Z', expires_at: '2026-12-13T00:00:00Z',
  };
  const basePayload = {
    format: 1, network: 'test', generation: 1, serial: 42,
    issued_at: '2026-11-12T23:00:00Z', expires_at: '2026-11-14T00:00:00Z',
  };

  // Production release export parses cleanly.
  const production = readFileSync(new URL('../../../deploy/network/windshare-network.json', import.meta.url), 'utf8');
  const prodRev = await directoryRevision(production);
  assert.equal(prodRev.network, 'windshare');

  // Generation forgery, excessive lifetimes, pre-grant issuance, and non-string timestamps are rejected.
  await assert.rejects(directoryRevision(signed('generation_forgery')), /Invalid directory revision/);
  await assert.rejects(directoryRevision(envelope({ ...basePayload, expires_at: '2027-03-01T00:00:00Z' }, baseGrant)), /Invalid directory revision/);
  await assert.rejects(directoryRevision(envelope(basePayload, { ...baseGrant, expires_at: '2027-06-01T00:00:00Z' })), /Invalid directory revision/);
  await assert.rejects(directoryRevision(envelope({ ...basePayload, issued_at: '2026-11-11T00:00:00Z' }, baseGrant)), /Invalid directory revision/);
  await assert.rejects(directoryRevision(envelope({ ...basePayload, expires_at: 2099 }, baseGrant)), /Invalid directory revision/);
  await assert.rejects(directoryRevision(envelope({ ...basePayload, format: 2 }, baseGrant)), /Invalid directory revision/);

  // A KV envelope for another network cannot displace the bundled network snapshot.
  const baseline = signed('valid');
  const wrongNetwork = envelope(
    { ...basePayload, network: 'staging', generation: 99, serial: 1 },
    { ...baseGrant, network: 'staging', generation: 99 },
  );
  const env = {
    BUNDLED_DIRECTORY_REVISION: JSON.stringify(await directoryRevision(baseline)),
    DIRECTORY: { get: async () => wrongNetwork },
    ASSETS: { fetch: async () => new Response(baseline) },
  };
  const request = new Request('https://windshare.cc/.well-known/windshare-network');
  assert.equal(await (await serveDirectory(request, env)).text(), baseline);
  assert.deepEqual(warnings, []);
});

test('mirror isolates stalled ASSETS fetches across requests', async t => {
  t.mock.method(Date, 'now', () => Date.parse(vectors.now));
  const baseline = signed('valid');
  const request = new Request('https://windshare.cc/.well-known/windshare-network');

  // A stalled/canceled request on cold start must not poison subsequent requests' bundle fetch.
  let assetCalls = 0;
  const env = {
    DIRECTORY: { get: async () => null },
    ASSETS: { fetch: () => (++assetCalls === 1 ? new Promise(() => {}) : Promise.resolve(new Response(baseline))) },
  };
  const req1 = serveDirectory(request, env);
  const req2 = await serveDirectory(request, env);
  assert.equal(req2.status, 200);
  assert.equal(await req2.text(), baseline);
  // Request 3 hits cached plain data without re-running ASSETS.fetch.
  const req3 = await serveDirectory(request, env);
  assert.equal(req3.status, 200);
  assert.equal(await req3.text(), baseline);
  assert.equal(assetCalls, 2);
  void req1;
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

test('transient ASSETS 5xx does not poison bundled directory cache while 404 and SPA HTML stay memoized', async t => {
  t.mock.method(Date, 'now', () => Date.parse(vectors.now));
  const warnings = [];
  t.mock.method(console, 'warn', (event, fields) => warnings.push({ event, ...fields }));
  const baseline = signed('valid'), older = signed('older_serial');
  const request = new Request('https://windshare.cc/.well-known/windshare-network');

  // 1. Production mode (BUNDLED_DIRECTORY_REVISION set): transient 5xx (including HTML error pages) retries until 200.
  let prodAssetCalls = 0;
  const prodResponses = [
    () => new Response('<html>503</html>', { status: 503, headers: { 'Content-Type': 'text/html; charset=utf-8' } }),
    () => new Response(null, { status: 500 }),
    () => new Response(baseline),
  ];
  const prodEnv = {
    BUNDLED_DIRECTORY_REVISION: JSON.stringify(await directoryRevision(baseline)),
    DIRECTORY: { get: async () => null },
    ASSETS: { fetch: async () => prodResponses[prodAssetCalls++]() },
  };
  assert.equal((await serveDirectory(request, prodEnv)).status, 503);
  assert.equal((await serveDirectory(request, prodEnv)).status, 503);
  const prodRecovered = await serveDirectory(request, prodEnv);
  assert.equal(prodRecovered.status, 200);
  assert.equal(await prodRecovered.text(), baseline);
  const prodCached = await serveDirectory(request, prodEnv);
  assert.equal(prodCached.status, 200);
  assert.equal(await prodCached.text(), baseline);
  assert.equal(prodAssetCalls, 3);

  // 2. Without BUNDLED_DIRECTORY_REVISION: recovery restores bundled floor protection against older KV revisions.
  let devAssetCalls = 0;
  const devEnv = {
    DIRECTORY: { get: async () => older },
    ASSETS: {
      fetch: async () => (++devAssetCalls === 1
        ? new Response('<html>502</html>', { status: 502, headers: { 'Content-Type': 'text/html' } })
        : new Response(baseline)),
    },
  };
  assert.equal(await (await serveDirectory(request, devEnv)).text(), older);
  assert.equal(await (await serveDirectory(request, devEnv)).text(), baseline);
  assert.equal(await (await serveDirectory(request, devEnv)).text(), baseline);
  assert.equal(devAssetCalls, 2);

  // 3. Definitive absence (404 and 200 text/html SPA fallback) remains memoized across requests.
  for (const makeAbsent of [
    () => new Response('not found', { status: 404 }),
    () => new Response('<html>spa</html>', { status: 200, headers: { 'Content-Type': 'text/html' } }),
  ]) {
    let absentCalls = 0;
    const absentEnv = { ASSETS: { fetch: async () => { absentCalls++; return makeAbsent(); } } };
    assert.equal((await serveDirectory(request, absentEnv)).status, 503);
    assert.equal((await serveDirectory(request, absentEnv)).status, 503);
    assert.equal(absentCalls, 1);
  }

  assert.deepEqual(warnings, [
    { event: 'directory_asset_fetch_failed', status: 503 },
    { event: 'directory_asset_fetch_failed', status: 500 },
    { event: 'directory_asset_fetch_failed', status: 502 },
  ]);
});

test('oversized KV payloads are rejected without pinning isolate memory and recovery clears KV cache', async t => {
  t.mock.method(Date, 'now', () => Date.parse(vectors.now));
  const warnings = [];
  t.mock.method(console, 'warn', (event, fields) => warnings.push({ event, ...fields }));
  const baseline = signed('valid'), newer = signed('new_generation');
  const bundledRevision = JSON.stringify(await directoryRevision(baseline));
  const oversizedAscii = 'x'.repeat(MAX_BYTES + 1);
  const oversizedUtf8 = '中'.repeat(Math.floor(MAX_BYTES / 3) + 1);
  const invalidSmall = '{not-json';
  const request = new Request('https://windshare.cc/.well-known/windshare-network');
  let current = null;
  const env = {
    BUNDLED_DIRECTORY_REVISION: bundledRevision,
    DIRECTORY: { get: async () => current },
    ASSETS: { fetch: async () => new Response(baseline) },
  };
  const encodeSpy = t.mock.method(TextEncoder.prototype, 'encode');
  const encodedCount = value => encodeSpy.mock.calls.filter(call => call.arguments[0] === value).length;

  // Strings longer than MAX_BYTES fail fast without allocating an encoded UTF-8 buffer.
  await assert.rejects(directoryRevision(oversizedAscii), /wire limit/);
  assert.equal(encodedCount(oversizedAscii), 0);

  // Repeated oversized ASCII KV values fall back cleanly with zero TextEncoder allocations and one warning.
  current = oversizedAscii;
  assert.equal(await (await serveDirectory(request, env)).text(), baseline);
  assert.equal(await (await serveDirectory(request, env)).text(), baseline);
  assert.equal(encodedCount(oversizedAscii), 0);
  assert.equal(warnings.filter(w => w.event === 'directory_kv_rejected' && w.reason === 'wire_limit').length, 1);

  // Multi-byte UTF-8 payloads that exceed MAX_BYTES are never memoized in state.kv.
  current = oversizedUtf8;
  assert.equal(await (await serveDirectory(request, env)).text(), baseline);
  assert.equal(await (await serveDirectory(request, env)).text(), baseline);
  assert.equal(encodedCount(oversizedUtf8), 2);
  assert.equal(warnings.filter(w => w.event === 'directory_kv_rejected' && w.reason === 'wire_limit').length, 2);

  // Within-limit rejected payloads stay memoized so repeat requests do not re-evaluate them.
  current = invalidSmall;
  assert.equal(await (await serveDirectory(request, env)).text(), baseline);
  const smallFirstCalls = encodedCount(invalidSmall);
  assert.ok(smallFirstCalls > 0);
  assert.equal(await (await serveDirectory(request, env)).text(), baseline);
  assert.equal(encodedCount(invalidSmall), smallFirstCalls);
  assert.equal(warnings.filter(w => w.event === 'directory_kv_rejected' && w.reason === 'invalid_revision').length, 1);

  // Receiving an oversized KV value clears any prior cached KV entry.
  current = oversizedAscii;
  assert.equal(await (await serveDirectory(request, env)).text(), baseline);
  assert.equal(encodedCount(oversizedAscii), 0);
  current = invalidSmall;
  assert.equal(await (await serveDirectory(request, env)).text(), baseline);
  assert.equal(encodedCount(invalidSmall), smallFirstCalls * 2);

  // Clearing KV (null) or switching to a valid KV copy also clears prior rejected KV cache.
  current = null;
  assert.equal(await (await serveDirectory(request, env)).text(), baseline);
  current = invalidSmall;
  assert.equal(await (await serveDirectory(request, env)).text(), baseline);
  assert.equal(encodedCount(invalidSmall), smallFirstCalls * 3);

  current = newer;
  assert.equal(await (await serveDirectory(request, env)).text(), newer);
  current = invalidSmall;
  assert.equal(await (await serveDirectory(request, env)).text(), baseline);
  assert.equal(encodedCount(invalidSmall), smallFirstCalls * 4);
  current = newer;
  assert.equal(await (await serveDirectory(request, env)).text(), newer);
  current = invalidSmall;
  assert.equal(await (await serveDirectory(request, env)).text(), baseline);
  assert.equal(encodedCount(invalidSmall), smallFirstCalls * 5);
});
