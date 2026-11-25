import assert from 'node:assert/strict';
import { createHash, createPublicKey, generateKeyPairSync, randomBytes } from 'node:crypto';
import test from 'node:test';
import { derivePasswordVerifier, fixture, signedDescriptor } from './local-worker.mjs';

const MAX_DESCRIPTOR_BYTES = 16 * 1024;
const MAX_DNS_BYTES = 16 * 1024;
const MAX_DIRECTORY_BYTES = 512 * 1024;
const KV_DIRECTORY_KEY = 'windshare-network';

function directoryDigest(envelope) {
  return createHash('sha256').update(Buffer.from(envelope.payload, 'base64url')).digest('base64url');
}

function makeEd25519Identity() {
  const privateKey = generateKeyPairSync('ed25519').privateKey;
  const rawPublic = createPublicKey(privateKey).export({ type: 'spki', format: 'der' }).subarray(-32);
  return { privateKey, rawPublic, publicKey: rawPublic.toString('base64url') };
}

const OPERATOR_VERIFIER = derivePasswordVerifier('admin', 'operator', 'Operator-Password-2026!');

async function initAdmin(worker) {
  const response = await worker.fetch('https://localhost/v1/admin/initialize', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Origin: 'https://localhost' },
    body: JSON.stringify({
      username: 'operator',
      password_verifier: OPERATOR_VERIFIER,
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
  let customOutbound = null;
  const { worker, requests, enrollment, authority, release } = await fixture(
    t,
    {
      API_ORIGIN: 'https://api.example',
      AUTHORITY_ORIGIN: 'https://origin.example',
    },
    {
      outbound: async (request, url) => (customOutbound ? customOutbound(request, url) : undefined),
    },
  );
  const admin = await initAdmin(worker);

  async function adminPost(path, body, status = 200, headers = {}) {
    const response = await worker.fetch('https://localhost' + path, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Origin: 'https://localhost',
        Cookie: admin.cookie,
        'X-CSRF-Token': admin.csrf,
        ...headers,
      },
      body: JSON.stringify(body),
    });
    const value = await response.json();
    assert.equal(response.status, status, JSON.stringify(value));
    return value;
  }

  // Edge-prepared admin routes must fail CSRF in the DO before any outbound DNS or HTTP fetch.
  for (const [path, body] of [
    ['/v1/admin/nodes/discover', { url: enrollment.url }],
    ['/v1/admin/nodes', enrollment],
  ]) {
    const rejected = await adminPost(path, body, 403, { 'X-CSRF-Token': 'wrong' });
    assert.equal(rejected.reason, 'csrf', path);
    assert.equal(requests.length, 0, `${path} invalid CSRF must not trigger outbound fetch`);
  }

  // Reserved frontend/control origins and non-canonical URLs fail before any outbound DNS or HTTP request.
  for (const [blockedURL, reason] of [
    [release.link_host, 'reserved_node_host'],
    [release.backup_entry, 'reserved_node_host'],
    ['https://api.example', 'reserved_node_host'],
    ['https://origin.example', 'reserved_node_host'],
    ['https://localhost', 'invalid_node_url'],
    ['https://foo.localhost', 'invalid_node_url'],
    ['https://foo.internal', 'invalid_node_url'],
    ['http://relay.example', 'invalid_node_url'],
    ['https://1.1.1.1', 'invalid_node_url'],
    ['https://relay.example:0', 'invalid_node_url'],
    ['https://relay.example/custom', 'invalid_node_url'],
  ]) {
    const before = requests.length;
    const result = await adminPost('/v1/admin/nodes/discover', { url: blockedURL }, 400);
    assert.equal(result.kind, 'node_discovery', blockedURL);
    assert.equal(result.reason, reason, blockedURL);
    assert.equal(requests.length, before, `expected no outbound fetch for ${blockedURL}`);
  }

  // DoH validation rejects private/loopback IPs, incomplete/error replies, empty answers, and oversized responses
  // before touching the target host's descriptor endpoint.
  const dnsFailureCases = [
    {
      name: 'private IPv4',
      status: 422,
      reason: 'node_non_public_address',
      answer: type => (type === '1' ? [{ type: 1, data: '10.0.0.1' }] : []),
    },
    {
      name: 'loopback IPv4',
      status: 422,
      reason: 'node_non_public_address',
      answer: type => (type === '1' ? [{ type: 1, data: '127.0.0.1' }] : []),
    },
    { name: 'empty A and AAAA', status: 502, reason: 'node_dns_failed', answer: () => [] },
    {
      name: 'truncated DNS reply',
      status: 502,
      reason: 'node_dns_failed',
      reply: { Status: 0, TC: true, Answer: [{ type: 1, data: '1.1.1.1' }] },
    },
    {
      name: 'SERVFAIL DNS reply',
      status: 502,
      reason: 'node_dns_failed',
      reply: { Status: 2, TC: false, Answer: [] },
    },
    {
      name: 'oversized DoH body',
      status: 502,
      reason: 'node_dns_failed',
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
    const result = await adminPost('/v1/admin/nodes/discover', { url: enrollment.url }, scenario.status);
    assert.equal(result.kind, 'node_discovery', scenario.name);
    assert.equal(result.reason, scenario.reason, scenario.name);
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
  const redirectResult = await adminPost('/v1/admin/nodes/discover', { url: enrollment.url }, 502);
  assert.equal(redirectResult.kind, 'node_discovery');
  assert.equal(redirectResult.reason, 'node_redirect_refused');
  assert.ok(
    !requests.slice(beforeRedirect).some(href => href.includes('redirected.example')),
    'discover_node must not follow redirects',
  );

  // Transport failures, non-200 HTTP statuses, oversized responses, and descriptor rejections carry distinct reasons.
  customOutbound = (_req, url) => {
    if (url.hostname !== 'relay.example') return undefined;
    let sent = false;
    const body = new ReadableStream({
      pull(controller) {
        if (!sent) {
          sent = true;
          controller.enqueue(new Uint8Array([0x7b]));
          return;
        }
        controller.error(new Error('connection reset'));
      },
    });
    return new Response(body, {
      status: 200,
      headers: { 'Content-Type': 'application/json', 'Content-Length': '100' },
    });
  };
  const unreachableResult = await adminPost('/v1/admin/nodes/discover', { url: enrollment.url }, 502);
  assert.equal(unreachableResult.kind, 'node_discovery');
  assert.equal(unreachableResult.reason, 'node_unreachable');

  customOutbound = (_req, url) => (url.hostname === 'relay.example' ? new Response('not found', { status: 404 }) : undefined);
  const notFoundResult = await adminPost('/v1/admin/nodes/discover', { url: enrollment.url }, 502);
  assert.equal(notFoundResult.kind, 'node_discovery');
  assert.equal(notFoundResult.reason, 'node_http_status');
  assert.equal(notFoundResult.status, 404);

  customOutbound = (_req, url) => (url.hostname === 'relay.example' ? new Response('boom', { status: 500 }) : undefined);
  const serverErrorResult = await adminPost('/v1/admin/nodes/discover', { url: enrollment.url }, 502);
  assert.equal(serverErrorResult.kind, 'node_discovery');
  assert.equal(serverErrorResult.reason, 'node_http_status');
  assert.equal(serverErrorResult.status, 500);

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
  const oversizedResult = await adminPost('/v1/admin/nodes/discover', { url: enrollment.url }, 422);
  assert.equal(oversizedResult.kind, 'node_discovery');
  assert.equal(oversizedResult.reason, 'node_invalid_descriptor');

  const descriptorIdentity = makeEd25519Identity();
  const expiredAt = Math.floor(Date.now() / 1000) - 60;
  customOutbound = (_req, url) => {
    if (url.hostname !== 'relay.example') return undefined;
    return Response.json(
      signedDescriptor('relay.example', descriptorIdentity.privateKey, descriptorIdentity.rawPublic, authority, {
        issued_at: expiredAt - 300,
        expires_at: expiredAt,
      }),
    );
  };
  const expiredResult = await adminPost('/v1/admin/nodes/discover', { url: enrollment.url }, 422);
  assert.equal(expiredResult.kind, 'node_discovery');
  assert.equal(expiredResult.reason, 'node_descriptor_expired');

  customOutbound = (_req, url) => {
    if (url.hostname !== 'relay.example') return undefined;
    return Response.json(
      signedDescriptor('relay.example', descriptorIdentity.privateKey, descriptorIdentity.rawPublic, authority, {
        relay_endpoint: 'wss://wrong.example/v2/ws',
      }),
    );
  };
  const mismatchResult = await adminPost('/v1/admin/nodes/discover', { url: enrollment.url }, 422);
  assert.equal(mismatchResult.kind, 'node_discovery');
  assert.equal(mismatchResult.reason, 'node_descriptor_mismatch');

  // Re-fetching binds enrollment to the settings the operator inspected.
  customOutbound = null;
  const changedKey = await adminPost('/v1/admin/nodes', { ...enrollment, node_key: makeEd25519Identity().publicKey }, 409);
  assert.equal(changedKey.kind, 'conflict');
  assert.equal(changedKey.reason, 'node_settings_changed');
  const changedRoles = await adminPost('/v1/admin/nodes', { ...enrollment, roles: ['relay', 'stun'] }, 409);
  assert.equal(changedRoles.kind, 'conflict');
  assert.equal(changedRoles.reason, 'node_settings_changed');

  for (const configuration of [
    null,
    { ...authority, issuer: 'wrong' },
    { ...authority, url: 'https://other.example' },
    { ...authority, url: 'https://origin.example/v1' },
  ]) {
    customOutbound = (_req, url) => url.hostname === 'relay.example'
      ? Response.json(signedDescriptor('relay.example', descriptorIdentity.privateKey, descriptorIdentity.rawPublic, configuration))
      : undefined;
    const result = await adminPost('/v1/admin/nodes/discover', { url: enrollment.url }, 422);
    assert.equal(result.reason, 'authority_mismatch');
  }
  for (const equivalentURL of ['https://origin.example/', 'https://ORIGIN.example:443/', 'https://API.example:443']) {
    customOutbound = (_req, url) => url.hostname === 'relay.example'
      ? Response.json(signedDescriptor('relay.example', descriptorIdentity.privateKey, descriptorIdentity.rawPublic,
        { ...authority, url: equivalentURL })) : undefined;
    await adminPost('/v1/admin/nodes/discover', { url: enrollment.url });
  }
});

test('enrolling and managing a canary publishes signed revisions and coalesces KV updates', async t => {
  // Blank optional AUTHORITY_ORIGIN binding must be ignored rather than failing URL parsing with 500.
  const { worker, mf, enrollment } = await fixture(
    t,
    { AUTHORITY_ORIGIN: '   ' },
    { testControl: true, manualAlarm: true },
  );
  const admin = await initAdmin(worker);

  async function call(path, body, { method = 'POST', status = 200, adminAuth = false, secret, headers = {} } = {}) {
    const reqHeaders = { 'Content-Type': 'application/json' };
    if (adminAuth) {
      reqHeaders.Origin = 'https://localhost';
      reqHeaders.Cookie = admin.cookie;
      reqHeaders['X-CSRF-Token'] = admin.csrf;
    }
    if (secret) reqHeaders.Authorization = 'Bearer ' + secret;
    Object.assign(reqHeaders, headers);
    const response = await worker.fetch('https://localhost' + path, {
      method,
      headers: reqHeaders,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await response.text();
    assert.equal(response.status, status, `${path}: ${text}`);
    return { response, text, value: text && response.headers.get('content-type')?.includes('json') ? JSON.parse(text) : text };
  }

  const kv = await mf.getKVNamespace('DIRECTORY');
  const namespace = await mf.getDurableObjectNamespace('AUTHORITY', 'authority');
  const authority = namespace.get(namespace.idFromName('network-authority-v1'));
  const getAlarm = async () => (await (await authority.fetch('https://authority/__test/alarm')).json()).alarm;
  const directory = async () => (await call('/v1/admin/console', undefined, { method: 'GET', adminAuth: true })).value.directory;

  // Keep enroll -> manage adjacent so the second mutation lands inside the 2 s KV write cooldown.
  const enrolled = (await call('/v1/admin/nodes', enrollment, { adminAuth: true })).value;
  assert.equal(enrolled.state, 'canary');
  assert.equal(enrolled.report_stale, false);
  const head = await directory();
  assert.equal(head.kv_sync_pending, false);
  const command = { node_key: enrollment.node_key, expected_state: enrolled.state,
    expected_policy_version: enrolled.version, action: { kind: 'activate' } };
  await call('/v1/admin/nodes/manage', command, { adminAuth: true });
  const updated = await directory();
  assert.equal(updated.serial, head.serial + 1);
  assert.equal(updated.kv_sync_pending, true);
  assert.equal(directoryDigest(JSON.parse(await kv.get(KV_DIRECTORY_KEY))), head.digest);
  assert.ok((await getAlarm()) !== null, 'write cooldown must arm a DO alarm for the deferred KV update');
  await authority.fetch('https://authority/__test/age-delivery', { method: 'POST', body: JSON.stringify({ ms: 120_000 }) });
  const alarmResponse = await authority.fetch('https://authority/__test/run-alarm', { method: 'POST' });
  assert.equal(alarmResponse.status, 200);
  assert.equal(directoryDigest(JSON.parse(await kv.get(KV_DIRECTORY_KEY))), updated.digest);
  assert.equal((await directory()).kv_sync_pending, false);
  assert.equal(await getAlarm(), null, 'flushed KV update must leave no DO alarm armed');

  const publicURL = 'https://localhost/.well-known/windshare-network';
  const publicHead = await worker.fetch(publicURL);
  assert.equal(publicHead.status, 200);
  assert.equal(directoryDigest(await publicHead.json()), updated.digest);
  const badManageCsrf = await call('/v1/admin/nodes/manage', command, {
    adminAuth: true,
    headers: { 'X-CSRF-Token': 'wrong' },
    status: 403,
  });
  assert.equal(badManageCsrf.value.reason, 'csrf');
  const current = (await call('/v1/admin/nodes', enrollment, { adminAuth: true })).value;
  assert.equal(current.state, 'active');
  const stale = await call('/v1/admin/nodes/manage', command, { adminAuth: true, status: 409 });
  assert.equal(stale.value.reason, 'listing_changed');
  const method = await worker.fetch(publicURL, { method: 'POST' });
  assert.equal(method.status, 405);
  for (const bad of ['{corrupt', ' '.repeat(MAX_DIRECTORY_BYTES + 1)]) {
    await kv.put(KV_DIRECTORY_KEY, bad);
    assert.equal((await worker.fetch(publicURL)).status, 200);
  }

  // Post-commit KV and alarm-arming failures stay non-fatal on HTTP fleet mutations.
  await authority.fetch('https://authority/__test/age-delivery', { method: 'POST', body: JSON.stringify({ ms: 120_000 }) });
  await authority.fetch('https://authority/__test/kv-fail', { method: 'POST', body: JSON.stringify({ count: 1 }) });
  await authority.fetch('https://authority/__test/alarm-faults', { method: 'POST', body: JSON.stringify({ setAlarm: 1 }) });
  await call('/v1/admin/nodes/manage', {
    node_key: enrollment.node_key,
    expected_state: current.state,
    expected_policy_version: current.version,
    action: { kind: 'drain' },
  }, { adminAuth: true });
  const drained = await directory();
  assert.equal(drained.serial, updated.serial + 1);
  assert.equal(drained.kv_sync_pending, true);
  assert.equal(await getAlarm(), null);
  const drainedFlush = await authority.fetch('https://authority/__test/run-alarm', { method: 'POST' });
  assert.equal(drainedFlush.status, 200);
  assert.equal(directoryDigest(JSON.parse(await kv.get(KV_DIRECTORY_KEY))), drained.digest);
  assert.equal((await directory()).kv_sync_pending, false);

  // All partial indexes exist upfront from ensure_schema(), and neither report reads nor PoW pruning run DDL.
  const expectedIndexes = ['authority_reports_newest', 'registration_email_expiry', 'registration_work_expiry'];
  const beforeOnlineQueries = await (await authority.fetch('https://authority/__test/schema-audit')).json();
  assert.deepEqual(beforeOnlineQueries, {
    indexes: expectedIndexes,
    ddlCount: 4,
    ddlInTransaction: 0,
  });
  const emptyReports = (await call('/v1/admin/reports', undefined, { method: 'GET', adminAuth: true })).value;
  assert.deepEqual(emptyReports.reports, []);
  await call('/v1/admin/settings', { registration: 'proof_of_work' }, { method: 'PATCH', adminAuth: true });
  const powChallenge = (
    await call('/v1/accounts', { client_secret: randomBytes(32).toString('base64url') }, { status: 428 })
  ).value;
  assert.equal(powChallenge.kind, 'verification');
  const afterOnlineQueries = await (await authority.fetch('https://authority/__test/schema-audit')).json();
  assert.deepEqual(afterOnlineQueries, {
    indexes: expectedIndexes,
    ddlCount: 4,
    ddlInTransaction: 0,
  });
});

test('directory KV publish failures retry via bounded Durable Object alarms and recover cleanly', async t => {
  const { worker, mf, requests } = await fixture(t, {}, { testControl: true, manualAlarm: true });
  const kv = await mf.getKVNamespace('DIRECTORY');
  const namespace = await mf.getDurableObjectNamespace('AUTHORITY', 'authority');
  const authority = namespace.get(namespace.idFromName('network-authority-v1'));

  const getAlarm = async () => (await (await authority.fetch('https://authority/__test/alarm')).json()).alarm;
  const runAlarm = async () => {
    const response = await authority.fetch('https://authority/__test/run-alarm', { method: 'POST' });
    assert.equal(response.status, 200);
  };
  const failKvPuts = async count => {
    const response = await authority.fetch('https://authority/__test/kv-fail', {
      method: 'POST',
      body: JSON.stringify({ count }),
    });
    assert.equal(response.status, 200);
  };

  // A transient KV 429 during publish arms a ~2 s DO alarm and recovers on alarm execution.
  await failKvPuts(1);
  requests.length = 0;
  const before = Date.now();
  const initial = await worker.scheduled({ cron: '0 * * * *' });
  assert.equal(initial.outcome, 'ok');
  assert.equal(await kv.get(KV_DIRECTORY_KEY), null);
  assert.equal(requests.length, 0, 'tick should only reconcile KV');
  const firstAlarm = await getAlarm();
  assert.ok(firstAlarm && firstAlarm >= before + 1_500 && firstAlarm <= Date.now() + 5_000, `alarm=${firstAlarm}`);

  await runAlarm();
  const published = await kv.get(KV_DIRECTORY_KEY);
  assert.ok(published, 'alarm retry must publish pending directory snapshot to KV');
  assert.equal(await getAlarm(), null, 'successful alarm retry must leave no alarm armed');

  // Simulate the full alarm sequence without sleeping through the backoff.
  await authority.fetch('https://authority/__test/age-delivery', {
    method: 'POST',
    body: JSON.stringify({ ms: 120_000 }),
  });
  await kv.delete(KV_DIRECTORY_KEY);
  const delays = [2_000, 5_000, 15_000, 60_000, 300_000, 900_000];
  await failKvPuts(delays.length + 1);

  const repairAttempt = await worker.scheduled({ cron: '0 * * * *' });
  assert.equal(repairAttempt.outcome, 'ok');
  const alarm1 = await getAlarm();
  assert.ok(alarm1 !== null, 'first failure must schedule 2 s retry alarm');

  for (const delay of delays.slice(1)) {
    const beforeRetry = Date.now();
    await runAlarm();
    assert.ok(await getAlarm() >= beforeRetry + delay - 500);
  }
  await runAlarm();
  assert.equal(await getAlarm(), null, 'exhausted retries must stop arming DO alarms');
  assert.equal(await kv.get(KV_DIRECTORY_KEY), null);

  // Once KV recovers, the next scheduled tick repairs KV and leaves no alarm armed.
  const recovered = await worker.scheduled({ cron: '0 * * * *' });
  assert.equal(recovered.outcome, 'ok');
  assert.equal(await kv.get(KV_DIRECTORY_KEY), published);
  assert.equal(await getAlarm(), null);

  const setAlarmFaults = async faults => {
    const response = await authority.fetch('https://authority/__test/alarm-faults', {
      method: 'POST',
      body: JSON.stringify(faults),
    });
    assert.equal(response.status, 200);
  };
  const runAlarmExpectError = async () => {
    const response = await authority.fetch('https://authority/__test/run-alarm', { method: 'POST' });
    assert.equal(response.status, 500);
    return response.json();
  };

  // Unexpected SQLite reconciliation errors inside alarm() must propagate Err so Cloudflare retries.
  await authority.fetch('https://authority/__test/age-delivery', { method: 'POST', body: JSON.stringify({ ms: 120_000 }) });
  await kv.delete(KV_DIRECTORY_KEY);
  await failKvPuts(1);
  assert.equal((await worker.scheduled({ cron: '0 * * * *' })).outcome, 'ok');
  assert.ok((await getAlarm()) !== null);
  await setAlarmFaults({ deliverySql: 1 });
  const sqlFailure = await runAlarmExpectError();
  assert.match(sqlFailure.error, /delivery ledger unavailable/);
  assert.equal(await kv.get(KV_DIRECTORY_KEY), null);
  await runAlarm();
  assert.equal(await kv.get(KV_DIRECTORY_KEY), published);
  assert.equal(await getAlarm(), null);

  // Failing to re-arm a required retry alarm inside alarm() must also propagate Err for runtime retry.
  await authority.fetch('https://authority/__test/age-delivery', { method: 'POST', body: JSON.stringify({ ms: 120_000 }) });
  await kv.delete(KV_DIRECTORY_KEY);
  await failKvPuts(2);
  assert.equal((await worker.scheduled({ cron: '0 * * * *' })).outcome, 'ok');
  assert.ok((await getAlarm()) !== null);
  await setAlarmFaults({ setAlarm: 1 });
  const rearmFailure = await runAlarmExpectError();
  assert.match(rearmFailure.error, /setAlarm unavailable/);
  assert.equal(await getAlarm(), null);
  assert.equal(await kv.get(KV_DIRECTORY_KEY), null);
  await runAlarm();
  assert.equal(await kv.get(KV_DIRECTORY_KEY), published);
  assert.equal(await getAlarm(), null);

  // Best-effort deleteAlarm failure after a successful publish must not fail reconciliation
  // and must invalidate the cached deadline so a subsequent deferred publish re-arms from storage.
  await authority.fetch('https://authority/__test/age-delivery', { method: 'POST', body: JSON.stringify({ ms: 120_000 }) });
  await kv.delete(KV_DIRECTORY_KEY);
  await failKvPuts(1);
  assert.equal((await worker.scheduled({ cron: '0 * * * *' })).outcome, 'ok');
  assert.ok((await getAlarm()) !== null);
  await setAlarmFaults({ deleteAlarm: 1 });
  assert.equal((await worker.scheduled({ cron: '0 * * * *' })).outcome, 'ok');
  assert.equal(await kv.get(KV_DIRECTORY_KEY), published);
  assert.ok((await getAlarm()) !== null, 'failed deleteAlarm leaves the existing alarm untouched');
  await authority.fetch('https://authority/__test/alarm', { method: 'DELETE' });
  await authority.fetch('https://authority/__test/age-delivery', { method: 'POST', body: JSON.stringify({ ms: 120_000 }) });
  await kv.delete(KV_DIRECTORY_KEY);
  await failKvPuts(1);
  assert.equal((await worker.scheduled({ cron: '0 * * * *' })).outcome, 'ok');
  assert.ok((await getAlarm()) !== null, 'invalidated deadline cache must re-arm a fresh alarm');
  await runAlarm();
  assert.equal(await kv.get(KV_DIRECTORY_KEY), published);
  assert.equal(await getAlarm(), null);

  // Across multiple scheduled ticks and transactions, the DO instance verifies and caches Signing once.
  const signingReads = await (await authority.fetch('https://authority/__test/signing-reads')).json();
  assert.deepEqual(signingReads, { certificate: 1, seed: 1 });
});
