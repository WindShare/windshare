import assert from 'node:assert/strict';
import { createHash, createPublicKey, generateKeyPairSync, randomBytes, sign } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { fixture, signedDescriptor } from './local-worker.mjs';

const MAX_DESCRIPTOR_BYTES = 16 * 1024;
const MAX_DNS_BYTES = 16 * 1024;
const MAX_DIRECTORY_BYTES = 256 * 1024;
const KV_DIRECTORY_KEY = 'windshare-network';

const release = JSON.parse(
  await readFile(new URL('../../../../deploy/network/release.json', import.meta.url), 'utf8'),
);

function releaseUsable() {
  const decode = raw => JSON.parse(Buffer.from(raw, 'base64url'));
  const payload = decode(release.snapshot.payload);
  const grant = decode(release.snapshot.certificate.payload);
  return (
    Date.now() + 300_000 >= Date.parse(payload.issued_at) &&
    Date.now() < Math.min(Date.parse(grant.expires_at), Date.parse(payload.expires_at) + 7 * 86400_000)
  );
}

function directoryDigest(envelope) {
  return createHash('sha256').update(Buffer.from(envelope.payload, 'base64url')).digest('base64url');
}

function makeEd25519Identity() {
  const privateKey = generateKeyPairSync('ed25519').privateKey;
  const rawPublic = createPublicKey(privateKey).export({ type: 'spki', format: 'der' }).subarray(-32);
  return { privateKey, rawPublic, publicKey: rawPublic.toString('base64url') };
}

function signVantageProof(identity, domain, payloadObject) {
  const payload = Buffer.from(JSON.stringify(payloadObject));
  const signature = sign(null, Buffer.concat([Buffer.from(domain), payload]), identity.privateKey);
  return {
    key: identity.publicKey,
    payload: payload.toString('base64url'),
    signature: signature.toString('base64url'),
  };
}

async function initAdmin(worker) {
  const response = await worker.fetch('https://localhost/v1/admin/initialize', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Origin: 'https://localhost' },
    body: JSON.stringify({
      username: 'operator',
      password: 'Operator-Password-2026!',
      token: 'local-initialization-token',
    }),
  });
  const body = await response.json();
  assert.equal(response.status, 200, JSON.stringify(body));
  return {
    cookie: response.headers.get('set-cookie').split(';')[0],
    csrf: body.csrf,
  };
}

test('discover_node enforces SSRF, DNS, redirect, size, and role/identity guards at the Worker edge', async t => {
  const vantage = makeEd25519Identity();
  let customOutbound = null;
  const { worker, requests, enrollment } = await fixture(
    t,
    {
      API_ORIGIN: 'https://api.example',
      FLEET_MAINTENANCE_TOKEN: 'too-short-token',
      VANTAGE_KEYS: JSON.stringify([{ id: 'global-a', key: vantage.publicKey, zone: 'global' }]),
    },
    {
      outbound: async (request, url) => (customOutbound ? customOutbound(request, url) : undefined),
    },
  );
  const admin = await initAdmin(worker);

  async function adminPost(path, body, status = 200) {
    const response = await worker.fetch('https://localhost' + path, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Origin: 'https://localhost',
        Cookie: admin.cookie,
        'X-CSRF-Token': admin.csrf,
      },
      body: JSON.stringify(body),
    });
    const value = await response.json();
    assert.equal(response.status, status, JSON.stringify(value));
    return value;
  }

  // Maintenance credentials shorter than 32 bytes are rejected at the DO boundary.
  const shortTokenRes = await worker.fetch('https://localhost/v1/maintenance/status', {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: 'Bearer too-short-token',
    },
    body: JSON.stringify({ node_key: enrollment.node_key, operation_id: 'short-token-check-01' }),
  });
  assert.equal(shortTokenRes.status, 403);
  assert.equal((await shortTokenRes.json()).reason, 'maintenance_credential');

  // Reserved frontend/control origins and non-canonical URLs fail before any outbound DNS or HTTP request.
  for (const blockedURL of [
    release.link_host,
    release.backup_entry,
    'https://api.example',
    'https://localhost',
    'https://foo.localhost',
    'https://foo.internal',
    'http://relay.example',
    'https://1.1.1.1',
    'https://relay.example:0',
    'https://relay.example/custom',
  ]) {
    const before = requests.length;
    const result = await adminPost('/v1/admin/nodes/discover', { url: blockedURL }, 400);
    assert.equal(result.kind, 'invalid', blockedURL);
    assert.equal(requests.length, before, `expected no outbound fetch for ${blockedURL}`);
  }

  // DoH validation rejects private/loopback IPs, incomplete/error replies, empty answers, and oversized responses
  // before touching the target host's descriptor endpoint.
  const dnsFailureCases = [
    { name: 'private IPv4', answer: type => (type === '1' ? [{ type: 1, data: '10.0.0.1' }] : []) },
    { name: 'loopback IPv4', answer: type => (type === '1' ? [{ type: 1, data: '127.0.0.1' }] : []) },
    { name: 'empty A and AAAA', answer: () => [] },
    { name: 'truncated DNS reply', reply: { Status: 0, TC: true, Answer: [{ type: 1, data: '1.1.1.1' }] } },
    { name: 'SERVFAIL DNS reply', reply: { Status: 2, TC: false, Answer: [] } },
    {
      name: 'oversized DoH body',
      raw: () => new Response(' '.repeat(MAX_DNS_BYTES + 1), { headers: { 'Content-Type': 'application/dns-json' } }),
    },
  ];
  for (const scenario of dnsFailureCases) {
    const before = requests.length;
    customOutbound = (_req, url) => {
      assert.equal(url.hostname, 'cloudflare-dns.com', `${scenario.name} must not fetch descriptor`);
      if (scenario.raw) return scenario.raw();
      if (scenario.reply) return Response.json(scenario.reply);
      return Response.json({ Status: 0, TC: false, Answer: scenario.answer(url.searchParams.get('type')) });
    };
    const result = await adminPost('/v1/admin/nodes/discover', { url: enrollment.url }, 400);
    assert.equal(result.kind, 'invalid', scenario.name);
    assert.ok(requests.length > before, `${scenario.name} should query DoH`);
    assert.ok(
      requests.slice(before).every(href => new URL(href).hostname === 'cloudflare-dns.com'),
      `${scenario.name} leaked request to target host`,
    );
  }

  // Public IPv6-only DNS succeeds through try_join!(A, AAAA).
  customOutbound = (_req, url) => {
    if (url.hostname === 'cloudflare-dns.com') {
      return Response.json({
        Status: 0,
        TC: false,
        Answer: url.searchParams.get('type') === '28' ? [{ type: 28, data: '2606:4700::1' }] : [],
      });
    }
    return undefined;
  };
  const ipv6Discovery = await adminPost('/v1/admin/nodes/discover', { url: enrollment.url });
  assert.equal(ipv6Discovery.node_key, enrollment.node_key);

  // Redirect responses are refused without following Location.
  const beforeRedirect = requests.length;
  customOutbound = (_req, url) => {
    if (url.hostname === 'relay.example') {
      return new Response(null, { status: 302, headers: { Location: 'https://redirected.example/.well-known/windshare' } });
    }
    return undefined;
  };
  assert.equal((await adminPost('/v1/admin/nodes/discover', { url: enrollment.url }, 400)).kind, 'invalid');
  assert.ok(
    !requests.slice(beforeRedirect).some(href => href.includes('redirected.example')),
    'discover_node must not follow redirects',
  );

  // Non-200 HTTP status and streamed bodies exceeding MAX_DESCRIPTOR_BYTES are rejected.
  customOutbound = (_req, url) => (url.hostname === 'relay.example' ? new Response('boom', { status: 500 }) : undefined);
  assert.equal((await adminPost('/v1/admin/nodes/discover', { url: enrollment.url }, 400)).kind, 'invalid');

  customOutbound = (_req, url) => {
    if (url.hostname !== 'relay.example') return undefined;
    const chunk = new Uint8Array(4096).fill(0x20);
    const tail = new Uint8Array(1).fill(0x20);
    const body = new ReadableStream({
      start(controller) {
        for (let i = 0; i < MAX_DESCRIPTOR_BYTES / chunk.length; i++) controller.enqueue(chunk);
        controller.enqueue(tail);
        controller.close();
      },
    });
    return new Response(body, { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  assert.equal((await adminPost('/v1/admin/nodes/discover', { url: enrollment.url }, 400)).kind, 'invalid');

  // Edge staging rejects mismatched node_key, undeclared roles, and keys enrolled as vantages.
  customOutbound = null;
  assert.equal(
    (await adminPost('/v1/admin/nodes', { ...enrollment, node_key: makeEd25519Identity().publicKey }, 400)).kind,
    'invalid',
  );
  assert.equal(
    (await adminPost('/v1/admin/nodes', { ...enrollment, roles: ['relay', 'stun'] }, 400)).kind,
    'invalid',
  );

  customOutbound = (_req, url) => {
    if (url.hostname === 'cloudflare-dns.com') {
      return Response.json({ Status: 0, TC: false, Answer: url.searchParams.get('type') === '1' ? [{ type: 1, data: '1.1.1.1' }] : [] });
    }
    if (url.href === 'https://vantage-node.example/.well-known/windshare') {
      return Response.json(signedDescriptor('vantage-node.example', vantage.privateKey, vantage.rawPublic));
    }
    return undefined;
  };
  assert.equal(
    (
      await adminPost(
        '/v1/admin/nodes',
        { ...enrollment, url: 'https://vantage-node.example', node_key: vantage.publicKey },
        400,
      )
    ).kind,
    'invalid',
  );
});

test('vantage routes, scheduled mirror audit, maintenance/manage boundaries, and public directory fallbacks work across the Worker', async t => {
  const cnVantage = makeEd25519Identity();
  const globalVantage = makeEd25519Identity();
  const unknownVantage = makeEd25519Identity();
  const maintenanceToken = randomBytes(32).toString('base64url');
  const { worker, mf, requests, mirrors, enrollment } = await fixture(t, {
    FLEET_MAINTENANCE_TOKEN: maintenanceToken,
    VANTAGE_KEYS: JSON.stringify([
      { id: 'cn-a', key: cnVantage.publicKey, zone: 'cn' },
      { id: 'global-a', key: globalVantage.publicKey, zone: 'global' },
    ]),
  });
  const admin = await initAdmin(worker);

  async function call(path, body, { method = 'POST', status = 200, adminAuth = false, secret, headers = {} } = {}) {
    const reqHeaders = { 'Content-Type': 'application/json', ...headers };
    if (adminAuth) {
      reqHeaders.Origin = 'https://localhost';
      reqHeaders.Cookie = admin.cookie;
      reqHeaders['X-CSRF-Token'] = admin.csrf;
    }
    if (secret) reqHeaders.Authorization = 'Bearer ' + secret;
    const response = await worker.fetch('https://localhost' + path, {
      method,
      headers: reqHeaders,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await response.text();
    assert.equal(response.status, status, `${path}: ${text}`);
    return { response, text, value: text && response.headers.get('content-type')?.includes('json') ? JSON.parse(text) : text };
  }

  // Vantage protocol auth, content-type, target assignment, staging verification, and cycle reporting.
  const now = Math.floor(Date.now() / 1000);
  const missingJson = await worker.fetch('https://localhost/v1/vantage/targets', { method: 'POST', body: '{}' });
  assert.equal(missingJson.status, 415);

  const unknownRes = await call(
    '/v1/vantage/targets',
    signVantageProof(unknownVantage, 'windshare/fleet-targets/v1\0', { at: now }),
    { status: 403 },
  );
  assert.equal(unknownRes.value.reason, 'unknown_vantage');

  const badSigProof = signVantageProof(cnVantage, 'windshare/fleet-targets/v1\0', { at: now });
  badSigProof.signature = randomBytes(64).toString('base64url');
  const badSigRes = await call('/v1/vantage/targets', badSigProof, { status: 403 });
  assert.equal(badSigRes.value.reason, 'vantage_signature');

  const staged = (await call('/v1/admin/nodes', enrollment, { adminAuth: true })).value;
  assert.equal(staged.listing.state.check.kind, 'queued');

  const releaseNodes = JSON.parse(Buffer.from(release.snapshot.payload, 'base64url')).nodes;
  const cnTargets = (
    await call('/v1/vantage/targets', signVantageProof(cnVantage, 'windshare/fleet-targets/v1\0', { at: now }))
  ).value;
  assert.equal(cnTargets.zone, 'cn');
  assert.deepEqual(cnTargets.origins, [release.link_host, release.backup_entry, 'https://self-hosted.example']);
  assert.equal(cnTargets.nodes.length, releaseNodes.length);
  assert.ok(!cnTargets.nodes.some(item => item.spec.node_key === enrollment.node_key));

  const uniqueShares = () => [1, 2, 3].map(() => [...randomBytes(12)]);
  const wrongZonePoll = (
    await call(
      '/v1/vantage/poll',
      signVantageProof(cnVantage, 'windshare/vantage-poll/v1\0', { at: now, shares: uniqueShares() }),
    )
  ).value;
  assert.equal(wrongZonePoll, null);

  const work = (
    await call(
      '/v1/vantage/poll',
      signVantageProof(globalVantage, 'windshare/vantage-poll/v1\0', { at: now, shares: uniqueShares() }),
    )
  ).value;
  assert.equal(work.operation_id, staged.listing.operation_id);
  assert.equal(work.spec.node_key, enrollment.node_key);
  assert.equal(work.tickets.length, 3);
  assert.deepEqual(work.origins, [release.link_host, release.backup_entry, 'https://self-hosted.example']);

  await call(
    '/v1/vantage/result',
    signVantageProof(globalVantage, 'windshare/vantage-result/v1\0', {
      at: now,
      node_key: enrollment.node_key,
      lease: work.lease,
      outcome: { kind: 'passed' },
    }),
  );
  const verifiedNode = (
    await call(`/v1/admin/nodes/${enrollment.node_key}`, undefined, { method: 'GET', adminAuth: true })
  ).value;
  assert.equal(verifiedNode.listing.state.check.kind, 'passed');
  assert.equal(verifiedNode.listing.state.check.vantage_key, globalVantage.publicKey);

  const expectedDigest = directoryDigest(release.snapshot);
  const cycleProof = signVantageProof(cnVantage, 'windshare/fleet-cycle/v1\0', {
    at: now,
    mirror: expectedDigest,
    nodes: {},
  });
  await call('/v1/vantage/cycle', cycleProof);
  await call('/v1/vantage/cycle', cycleProof, { status: 400 });

  const consoleBeforeScheduled = (
    await call('/v1/admin/console', undefined, { method: 'GET', adminAuth: true })
  ).value;
  assert.deepEqual(consoleBeforeScheduled.directory.silent_vantages, ['global-a']);
  assert.equal(consoleBeforeScheduled.directory.vantages.length, 1);
  assert.equal(consoleBeforeScheduled.directory.vantages[0].id, 'cn-a');
  assert.equal(consoleBeforeScheduled.directory.mirrors_current, false);

  // Scheduled cron runs reconcile -> KV publish (deferring mirror checks), then audits mirrors and updates console.
  const mirrorURLs = [release.link_host, release.backup_entry].map(origin => origin + '/.well-known/windshare-network');
  for (const url of mirrorURLs) mirrors.set(url, release.snapshot);
  const kv = await mf.getKVNamespace('DIRECTORY');

  requests.length = 0;
  const firstCron = await worker.scheduled({ cron: '*/5 * * * *' });
  assert.equal(firstCron.outcome, 'ok');
  assert.equal(requests.length, 0, 'fresh KV publish must defer immediate mirror probe');
  const publishedRaw = await kv.get(KV_DIRECTORY_KEY);
  assert.ok(publishedRaw, 'scheduled tick must publish directory snapshot to KV');
  assert.equal(directoryDigest(JSON.parse(publishedRaw)), expectedDigest);

  requests.length = 0;
  const secondCron = await worker.scheduled({ cron: '*/5 * * * *' });
  assert.equal(secondCron.outcome, 'ok');
  assert.deepEqual(new Set(requests), new Set(mirrorURLs));
  const consoleSynced = (
    await call('/v1/admin/console', undefined, { method: 'GET', adminAuth: true })
  ).value;
  assert.equal(consoleSynced.directory.mirrors_current, true);

  mirrors.set(mirrorURLs[0], {});
  const thirdCron = await worker.scheduled({ cron: '*/5 * * * *' });
  assert.equal(thirdCron.outcome, 'ok');
  const consoleLagging = (
    await call('/v1/admin/console', undefined, { method: 'GET', adminAuth: true })
  ).value;
  assert.equal(consoleLagging.directory.mirrors_current, false);

  // Deleting KV simulates drift; the next scheduled AuditKv repairs KV and defers mirror probes again.
  await kv.delete(KV_DIRECTORY_KEY);
  requests.length = 0;
  const repairCron = await worker.scheduled({ cron: '*/5 * * * *' });
  assert.equal(repairCron.outcome, 'ok');
  assert.equal(requests.length, 0, 'KV repair must defer immediate mirror probe');
  assert.equal(await kv.get(KV_DIRECTORY_KEY), publishedRaw);

  // Public directory endpoint method guard and fallback across corrupt/oversized/missing KV values.
  const publicURL = 'https://localhost/.well-known/windshare-network';
  const badMethod = await worker.fetch(publicURL, { method: 'POST' });
  assert.equal(badMethod.status, 405);
  assert.equal(badMethod.headers.get('allow'), 'GET, HEAD, OPTIONS');
  assert.equal(badMethod.headers.get('cache-control'), 'no-store');
  assert.equal(badMethod.headers.get('access-control-allow-origin'), '*');

  const usable = releaseUsable();
  for (const kvValue of ['{corrupt-json', ' '.repeat(MAX_DIRECTORY_BYTES + 1), null]) {
    if (kvValue === null) await kv.delete(KV_DIRECTORY_KEY);
    else await kv.put(KV_DIRECTORY_KEY, kvValue);
    const res = await worker.fetch(publicURL);
    assert.equal(res.status, usable ? 200 : 503);
    assert.equal(res.headers.get('access-control-allow-origin'), '*');
    if (usable) {
      assert.match(res.headers.get('cache-control'), /^public, max-age=\d+$/);
      assert.equal(directoryDigest(await res.json()), expectedDigest);
    } else {
      assert.equal(res.headers.get('cache-control'), 'no-store');
      assert.equal(res.headers.get('retry-after'), '30');
    }
  }

  // Maintenance probe/drain/release and admin manage boundaries.
  const op = { node_key: enrollment.node_key, operation_id: 'maintenance-boundary-01' };
  const unauthProbe = await call('/v1/maintenance/probe', { operation: op, shares: uniqueShares() }, { status: 403 });
  assert.equal(unauthProbe.value.reason, 'maintenance_credential');
  assert.equal(
    (await call('/v1/maintenance/probe', { operation: op, shares: uniqueShares(), extra: true }, { secret: maintenanceToken, status: 400 })).value.kind,
    'invalid',
  );
  // Valid DTO reaches Authority::maintenance_probe and rejects because no hold is active.
  assert.equal(
    (await call('/v1/maintenance/probe', { operation: op, shares: uniqueShares() }, { secret: maintenanceToken, status: 400 })).value.kind,
    'invalid',
  );

  for (const path of ['/v1/maintenance/drain', '/v1/maintenance/release']) {
    const denied = await call(path, op, { adminAuth: true, status: 403 });
    assert.equal(denied.value.reason, 'maintenance_credential');
    const badBody = await call(path, { ...op, extra: true }, { secret: maintenanceToken, status: 400 });
    assert.equal(badBody.value.kind, 'invalid');
    // Valid payloads reach signer(env), which is unavailable without a production directory signing seed.
    const reachedSigner = await call(path, op, { secret: maintenanceToken, status: 503 });
    assert.equal(reachedSigner.text, 'fleet temporarily unavailable');
  }

  const manageNoCsrf = await call(
    '/v1/admin/nodes/manage',
    { node_key: enrollment.node_key, operation_id: staged.listing.operation_id, action: { kind: 'drain' } },
    { status: 403, headers: { Origin: 'https://localhost', Cookie: admin.cookie, 'X-CSRF-Token': 'wrong' } },
  );
  assert.equal(manageNoCsrf.value.reason, 'csrf');
  const manageBadAction = await call(
    '/v1/admin/nodes/manage',
    { node_key: enrollment.node_key, operation_id: staged.listing.operation_id, action: { kind: 'bogus' } },
    { adminAuth: true, status: 400 },
  );
  assert.equal(manageBadAction.value.kind, 'invalid');
  const manageBadAbort = await call(
    '/v1/admin/nodes/manage',
    { node_key: enrollment.node_key, operation_id: staged.listing.operation_id, action: { kind: 'abort_maintenance' } },
    { adminAuth: true, status: 400 },
  );
  assert.equal(manageBadAbort.value.kind, 'invalid');
  const manageReachedSigner = await call(
    '/v1/admin/nodes/manage',
    { node_key: enrollment.node_key, operation_id: staged.listing.operation_id, action: { kind: 'drain' } },
    { adminAuth: true, status: 503 },
  );
  assert.equal(manageReachedSigner.text, 'fleet temporarily unavailable');
  const manageAbortReachedSigner = await call(
    '/v1/admin/nodes/manage',
    {
      node_key: enrollment.node_key,
      operation_id: staged.listing.operation_id,
      action: { kind: 'abort_maintenance', maintenance_id: 'maintenance-boundary-01' },
    },
    { adminAuth: true, status: 503 },
  );
  assert.equal(manageAbortReachedSigner.text, 'fleet temporarily unavailable');
});
