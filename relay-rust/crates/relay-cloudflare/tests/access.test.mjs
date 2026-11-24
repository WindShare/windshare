import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { setTimeout as delay } from 'node:timers/promises';
import assert from 'node:assert/strict';
import { createHash, randomBytes } from 'node:crypto';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { Miniflare, Log, LogLevel, convertV4MiniflareOptions } from 'miniflare';

const root = fileURLToPath(new URL('../', import.meta.url));
const origin = 'https://localhost';
const password = 'local-access-integration-password';
const encode = bytes => Buffer.from(bytes).toString('base64url');
const digest = value => encode(createHash('sha256').update(value).digest());
const claimant = () => {
  const secret = randomBytes(32);
  return { claim_secret: encode(secret), claim_digest: digest(secret) };
};
function options(ownerPassword = password, consoleOrigin = origin, relayOrigin = 'https://127.0.0.1', faults = false, turn = undefined) {
  const shared = { compatibilityDate: '2026-10-17',
    compatibilityFlags: ['web_socket_auto_reply_to_close'], modulesRoot: root };
  const module = (path, type = 'ESModule') => ({ type, path: root + path });
  return convertV4MiniflareOptions({ log: new Log(LogLevel.ERROR), workers: [
    { ...shared, name: 'relay', modules: [...(faults ? [module('tests/access-host.mjs')] : []), module('entry.mjs'), module('build/index.js'), module('build/index_bg.wasm', 'CompiledWasm')],
      bindings: { OWNER_PASSWORD: ownerPassword, CONSOLE_ORIGIN: consoleOrigin, RELAY_URL: relayOrigin, FRONTEND_URL: relayOrigin,
        ...(turn ? { TURN_KEY: JSON.stringify({key_id:'fixture',api_token:'test-token'}) } : {}) },
      ...(turn ? { outboundService: turn } : {}),
      durableObjects: { SHARES: { className: 'ShareRelayObject', useSQLite: true },
        ACCESS: { className: 'AccessObject', useSQLite: true } } },
    { ...shared, name: 'console', modules: [module('console-entry.mjs')], bindings: { CONSOLE_ORIGIN: consoleOrigin },
      serviceBindings: { RELAY: 'relay' } },
  ] });
}

test('console static assets and non-API routes bypass AccessObject and cache only hashed bundles', { timeout: 30_000 }, async t => {
  const mf = new Miniflare(options(password, origin, 'https://127.0.0.1', true));
  t.after(() => mf.dispose());
  const consoleWorker = await mf.getWorker('console');
  const relayWorker = await mf.getWorker('relay');
  for (const worker of [consoleWorker, relayWorker]) {
    const page = await worker.fetch(origin + '/');
    assert.equal(page.status, 200);
    assert.equal(page.headers.get('Cache-Control'), 'no-store');
    assert.match(page.headers.get('Content-Security-Policy'), /frame-ancestors 'none'/);
    assert.equal(page.headers.get('X-Content-Type-Options'), 'nosniff');
    assert.equal(page.headers.get('Referrer-Policy'), 'no-referrer');
    assert.equal(page.headers.get('Cross-Origin-Opener-Policy'), 'same-origin');
    const html = await page.text();
    const assetPath = html.match(/src="(\/assets\/[^"]+)"/)?.[1];
    assert(assetPath, 'expected hashed asset path in console HTML');
    const bundle = await worker.fetch(origin + assetPath);
    assert.equal(bundle.status, 200);
    assert.equal(bundle.headers.get('Cache-Control'), 'public, max-age=31536000, immutable');
    assert.equal((await worker.fetch(origin + '/missing')).status, 404);
    assert.equal((await worker.fetch(origin + '/', { method: 'POST' })).status, 405);
  }
  const authority = (await mf.getBindings('relay')).ACCESS.getByName('authority');
  assert.deepEqual(await (await authority.fetch('https://internal/__test/fetches')).json(), { fetches: 0 });
  assert.equal((await authority.fetch(origin + '/')).status, 404);
});

test('SQLite access authority survives restart, approval retry, cancellation and password rotation', { timeout: 60_000 }, async () => {
  const mf = new Miniflare(options());
  try {
    let consoleWorker = await mf.getWorker('console');
    let cookie = '';
    const request = async (path, body, expected = 200, authenticated = false, headers = {}) => {
      const response = await consoleWorker.fetch(origin + '/api/' + path, {
        method: body === undefined ? 'GET' : 'POST',
        headers: { ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
          ...(authenticated ? { Cookie: cookie, Origin: origin } : {}), ...headers },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      const raw = await response.text();
      assert.match(raw, /^\{/, path + ': ' + raw);
      const data = JSON.parse(raw);
      assert.equal(response.status, expected, JSON.stringify(data));
      return { response, data };
    };
    const discovery = await mf.dispatchFetch('https://127.0.0.1/.well-known/windshare');
    assert.equal(discovery.status, 200, await discovery.clone().text());
    assert.equal((await discovery.json()).console_url, origin);
    assert.equal((await mf.dispatchFetch('https://127.0.0.1/api/credentials')).status, 404);
    assert.equal((await consoleWorker.fetch(origin + '/internal/authorize')).status, 404);
    const page = await consoleWorker.fetch(origin + '/');
    assert.match(page.headers.get('Content-Security-Policy'), /frame-ancestors 'none'/);
    await request('credentials', undefined, 401);
    await request('credentials/revoked', undefined, 401);
    await request('login', { password }, 403);
    const login = await request('login', { password }, 200, false, { Origin: origin });
    cookie = login.response.headers.get('Set-Cookie').split(';')[0];
    assert.match(login.response.headers.get('Set-Cookie'), /HttpOnly; Secure; SameSite=Strict; Max-Age=43200/);
    await request('credentials', { name: 'cross site' }, 403, true, { Origin: 'https://127.0.0.1' });
    await request('credentials', { name: 'missing origin' }, 403, false, { Cookie: cookie });
    await request('enrollments', { device_name: 'invalid request' }, 400);

    const authority = (await mf.getBindings('relay')).ACCESS.getByName('authority');
    const authorize = async (accessKey, expected) => {
      const response = await authority.fetch('https://internal/internal/authorize', {
        method: 'POST', body: JSON.stringify({ digest: digest(accessKey), share_id: encode(randomBytes(12)), operation_id: 'test-operation' }),
      });
      assert.equal(response.status, expected, await response.text());
    };

    const first = claimant();
    const { data: pending } = await request('enrollments', { device_name: 'Laptop', claim_digest: first.claim_digest });
    const { data: retry } = await request('enrollments', { device_name: 'Laptop', claim_digest: first.claim_digest });
    assert.equal(pending.enrollment_id, retry.enrollment_id);
    const claim = { enrollment_id: pending.enrollment_id, claim_secret: first.claim_secret };
    await request('enrollments/claim', { ...claim, claim_secret: claimant().claim_secret }, 403);
    await request('enrollments/approve', { enrollment_id: pending.enrollment_id, user_code: 'wrong' }, 403, true);
    await request('enrollments/approve', { enrollment_id: pending.enrollment_id, user_code: pending.user_code }, 200, true);
    const { data: credential } = await request('enrollments/claim', claim);
    const { data: slow } = await request('enrollments/claim', claim, 429);
    await delay(slow.retry_after_ms);
    const { data: reclaimed } = await request('enrollments/claim', claim);
    assert.deepEqual(reclaimed, credential, 'a lost claim response must recover the same key and identifier');
    await authorize(credential.access_key, 403);
    assert.deepEqual((await request('credentials', undefined, 200, true)).data.credentials, []);
    await request('enrollments/installed', claim);
    await request('enrollments/installed', claim);
    await request('enrollments/cancel', { ...claim, claim_secret: claimant().claim_secret }, 403);

    await authorize(credential.access_key, 200);
    await request('credentials/revoke', { credential_id: credential.credential_id, terminate_shares: false }, 200, true);
    await authorize(credential.access_key, 403);
    await request('enrollments/installed', claim, 403);

    const second = claimant();
    const { data: incomplete } = await request('enrollments', { device_name: 'Cancelled device', claim_digest: second.claim_digest });
    await request('enrollments/approve', { enrollment_id: incomplete.enrollment_id, user_code: incomplete.user_code }, 200, true);
    const cancelClaim = { enrollment_id: incomplete.enrollment_id, claim_secret: second.claim_secret };
    const { data: abandoned } = await request('enrollments/claim', cancelClaim);
    await authorize(abandoned.access_key, 403);
    await request('enrollments/installed', cancelClaim);
    await authorize(abandoned.access_key, 200);
    await request('enrollments/cancel', cancelClaim);
    await request('enrollments/cancel', cancelClaim);
    await request('enrollments/installed', cancelClaim, 403);
    await authorize(abandoned.access_key, 403);

    const third = claimant();
    const { data: retainedEnrollment } = await request('enrollments', { device_name: 'Workstation', claim_digest: third.claim_digest });
    await request('enrollments/approve', { enrollment_id: retainedEnrollment.enrollment_id, user_code: retainedEnrollment.user_code }, 200, true);
    const retainedClaim = { enrollment_id: retainedEnrollment.enrollment_id, claim_secret: third.claim_secret };
    const { data: retained } = await request('enrollments/claim', retainedClaim);
    await authorize(retained.access_key, 403);
    await request('enrollments/installed', retainedClaim);

    const { data: manual } = await request('credentials', { name: 'Automation' }, 201, true);
    assert.equal(manual.configuration.access_key, manual.access_key);

    await mf.setOptions(options('rotated-local-access-password'));
    consoleWorker = await mf.getWorker('console');
    await request('session', undefined, 401, true);
    await request('login', { password }, 401, false, { Origin: origin });
    const rotated = await request('login', { password: 'rotated-local-access-password' }, 200, false, { Origin: origin });
    cookie = rotated.response.headers.get('Set-Cookie').split(';')[0];
    const { data: list } = await request('credentials', undefined, 200, true);
    assert.deepEqual(
      new Set(list.credentials.map(item => item.credential_id)),
      new Set([retained.credential_id, manual.credential.credential_id]),
    );
    assert(list.credentials.every(item => item.revoked_at === null));
    const { data: history } = await request('credentials/revoked', undefined, 200, true);
    assert.equal(history.next_cursor, null);
    const [revoked] = history.credentials;
    assert.equal(revoked.credential_id, credential.credential_id);
    assert(Number.isSafeInteger(revoked.revoked_at));
    const { data: after } = await request(
      `credentials/revoked?after=${revoked.revoked_at}.${revoked.credential_id}`, undefined, 200, true);
    assert.deepEqual(after, { credentials: [], next_cursor: null });
    await request('credentials/revoked?after=invalid', undefined, 400, true);
    const newAuthority = (await mf.getBindings('relay')).ACCESS.getByName('authority');
    const preserved = await newAuthority.fetch('https://internal/internal/authorize', {
      method: 'POST', body: JSON.stringify({ digest: digest(manual.access_key), share_id: encode(randomBytes(12)), operation_id: 'after-rotation' }),
    });
    assert.equal(preserved.status, 200);
    await request('enrollments/installed', retainedClaim);
    await request('enrollments/installed', claim, 403);
  } finally {
    await mf.dispose();
  }
});

test('credential history pages across timestamp ties without loading revoked rows as active', async t => {
  const mf = new Miniflare(options(password, origin, 'https://127.0.0.1', true));
  t.after(() => mf.dispose());
  const consoleWorker = await mf.getWorker('console');
  const login = await consoleWorker.fetch(origin + '/api/login', {
    method: 'POST', headers: { 'Content-Type': 'application/json', Origin: origin },
    body: JSON.stringify({ password }),
  });
  assert.equal(login.status, 200);
  const cookie = login.headers.get('Set-Cookie').split(';')[0];
  const get = async path => {
    const response = await consoleWorker.fetch(origin + path, { headers: { Cookie: cookie } });
    assert.equal(response.status, 200, await response.clone().text());
    return response.json();
  };
  const entries = Array.from({ length: 52 }, (_, offset) => {
    const bytes = Buffer.alloc(16, offset);
    return {
      key: 'credential:' + encode(bytes),
      value: { id: [...bytes], principal: [...bytes], name: 'Revoked', digest: Array(32).fill(0),
        created_at: 1, last_used_at: null, state: { Revoked: { at: offset === 0 ? 2 : 3 } } },
    };
  });
  const authority = (await mf.getBindings('relay')).ACCESS.getByName('authority');
  const seeded = await authority.fetch('https://internal/__test/access-entries', {
    method: 'POST', body: JSON.stringify(entries),
  });
  assert.equal(seeded.status, 200);
  assert.deepEqual((await get('/api/credentials')).credentials, []);
  const first = await get('/api/credentials/revoked');
  assert.equal(first.credentials.length, 50);
  assert(first.credentials.every(entry => entry.revoked_at === 3));
  assert(first.next_cursor);
  const second = await get('/api/credentials/revoked?after=' + encodeURIComponent(first.next_cursor));
  assert.equal(second.next_cursor, null);
  assert.deepEqual(second.credentials.map(entry => entry.revoked_at), [3, 2]);
  const seen = [...first.credentials, ...second.credentials].map(entry => 'credential:' + entry.credential_id);
  assert.deepEqual(new Set(seen), new Set(entries.map(entry => entry.key)));
});

test('pending enrollments remain discoverable past 1001 installed receipts', async t => {
  const mf = new Miniflare(options(password, origin, 'https://127.0.0.1', true));
  t.after(() => mf.dispose());
  const consoleWorker = await mf.getWorker('console');
  const login = await consoleWorker.fetch(origin + '/api/login', {
    method: 'POST', headers: { 'Content-Type': 'application/json', Origin: origin },
    body: JSON.stringify({ password }),
  });
  assert.equal(login.status, 200);
  const cookie = login.headers.get('Set-Cookie').split(';')[0];
  // '!' sorts strictly before every URL-safe base64 character so these durable
  // receipts would fill the entire prefix window if SQL did not filter first.
  const installedReceipts = Array.from({ length: 1050 }, (_, index) => {
    const id = Buffer.alloc(16);
    id.writeUInt32BE(index, 12);
    return {
      key: 'enrollment:!' + String(index).padStart(4, '0'),
      value: {
        id: [...id], user_code: 'AAAA-AAAA', device_name: 'Old device',
        claim_digest: Array(32).fill(0), created_at: 1, expires_at: 2, next_poll_at: 1,
        state: { Installed: { credential: [...id] } },
      },
    };
  });
  const authority = (await mf.getBindings('relay')).ACCESS.getByName('authority');
  const seeded = await authority.fetch('https://internal/__test/access-entries', {
    method: 'POST', body: JSON.stringify(installedReceipts),
  });
  assert.equal(seeded.status, 200);
  const fresh = claimant();
  const created = await consoleWorker.fetch(origin + '/api/enrollments', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ device_name: 'Fresh device', claim_digest: fresh.claim_digest }),
  });
  assert.equal(created.status, 200, await created.clone().text());
  const enrollment = await created.json();
  const listed = await consoleWorker.fetch(origin + '/api/enrollments', { headers: { Cookie: cookie } });
  assert.equal(listed.status, 200, await listed.clone().text());
  const { enrollments } = await listed.json();
  assert.equal(enrollments.length, 1);
  assert.equal(enrollments[0].enrollment_id, enrollment.enrollment_id);
  assert.equal(enrollments[0].user_code, enrollment.user_code);
  const approved = await consoleWorker.fetch(origin + '/api/enrollments/approve', {
    method: 'POST', headers: { 'Content-Type': 'application/json', Origin: origin, Cookie: cookie },
    body: JSON.stringify({ enrollment_id: enrollment.enrollment_id, user_code: enrollment.user_code }),
  });
  assert.equal(approved.status, 200, await approved.clone().text());
  const afterApprove = await consoleWorker.fetch(origin + '/api/enrollments', { headers: { Cookie: cookie } });
  assert.equal(afterApprove.status, 200);
  assert.deepEqual((await afterApprove.json()).enrollments, []);
});

test('production Go protocol clients observe revocation, managed TURN contract and durable administrative termination', { timeout: 60_000 }, async () => {
  let upstream = 0;
  const provider = async request => {
    assert.equal(request.url, 'https://rtc.live.cloudflare.com/v1/turn/keys/fixture/credentials/generate-ice-servers');
    assert.equal(request.headers.get('authorization'), 'Bearer test-token');
    assert.deepEqual(await request.json(), { ttl: 1800 });
    upstream++;
    return Response.json({
      iceServers: [
        { urls: 'stun:localhost' },
        { urls: ['turn:127.0.0.1:3478?transport=udp'], username: 'ephemeral-user', credential: 'ephemeral-pass' },
      ],
    }, { status: 201 });
  };
  const mf = new Miniflare(options(password, origin, 'https://127.0.0.1', false, provider));
  try {
    const address = await mf.ready;
    const relayOrigin = address.origin;
    const consoleAddress = new URL(relayOrigin);
    consoleAddress.hostname = 'localhost';
    await mf.setOptions({ ...options(password, consoleAddress.origin, relayOrigin, false, provider), port: Number(address.port) });
    const { stdout } = await promisify(execFile)('go', ['run', './relay-rust/tests/_goclient',
      '-endpoint', relayOrigin + '/v2/ws', '-access-console', consoleAddress.origin], {
      cwd: fileURLToPath(new URL('../../../../', import.meta.url)), timeout: 45_000, windowsHide: true,
      env: { ...process.env, WINDSHARE_OWNER_PASSWORD: password }, maxBuffer: 128 * 1024,
    });
    assert.match(stdout, /PASS:/);
    assert.equal(upstream, 1, 'only the active fallback escalation may mint upstream TURN credentials');
  } finally { await mf.dispose(); }
});

test('failed termination batches preserve retry responsibility without starving later targets', { timeout: 30_000 }, async () => {
  const configuration = options(password, origin, 'https://127.0.0.1', true);
  const mf = new Miniflare(configuration);
  try {
    const bindings = await mf.getBindings('relay');
    let authority = bindings.ACCESS.getByName('authority');
    const post = (path, body, cookie = '') => authority.fetch(origin + path, {
      method: 'POST', headers: { 'Content-Type': 'application/json', Origin: origin, Cookie: cookie },
      body: JSON.stringify(body),
    });
    const login = await post('/api/login', { password });
    assert.equal(login.status, 200);
    const cookie = login.headers.get('Set-Cookie').split(';')[0];
    const created = await post('/api/credentials', { name: 'queue regression' }, cookie);
    assert.equal(created.status, 201);
    const credential = await created.json();
    const shares = Array.from({ length: 32 }, (_, index) => {
      const bytes = Buffer.alloc(12); bytes[11] = index + 1; return encode(bytes);
    });
    const healthy = encode(Buffer.alloc(12, 255));
    for (const share of shares) {
      const response = await bindings.SHARES.getByName(share).fetch('https://internal/__test/fail-termination', { method: 'POST' });
      assert.equal(response.status, 200);
    }
    for (const share of [...shares, healthy]) {
      const response = await post('/internal/authorize', {
        digest: digest(credential.access_key), share_id: share, operation_id: 'queue-regression',
      });
      assert.equal(response.status, 200);
    }
    const revoked = await post('/api/credentials/revoke', {
      credential_id: credential.credential.credential_id, terminate_shares: true,
    }, cookie);
    assert.equal(revoked.status, 200);
    const queue = async () => (await authority.fetch('https://internal/__test/queue')).json();
    assert.equal((await authority.fetch('https://internal/__test/alarm')).status, 200);
    let remaining = await queue();
    assert.equal(remaining.length, 33);
    assert.equal(remaining.find(row => row.share === healthy).next_attempt, 0);
    assert.ok(remaining.filter(row => row.share !== healthy).every(row => row.next_attempt > 0));
    await mf.setOptions(configuration);
    authority = (await mf.getBindings('relay')).ACCESS.getByName('authority');
    assert.equal((await authority.fetch('https://internal/__test/alarm')).status, 200);
    remaining = await queue();
    assert.equal(remaining.length, 32);
    assert.ok(remaining.every(row => row.share !== healthy), 'failed targets blocked a later termination after restart');
    const stopped = await (await mf.getBindings('relay')).SHARES.getByName(healthy).fetch('https://internal/internal/terminate', {
      method: 'POST', body: JSON.stringify({ credential_id: credential.credential.credential_id }),
    });
    assert.equal(stopped.status, 200, 'terminating an empty target must be idempotent');
  } finally { await mf.dispose(); }
});

test('managed TURN authorizes read-only, pauses new grants and bounds issuance per credential', { timeout: 30_000 }, async t => {
  const relayOrigin = 'https://127.0.0.1';
  const urls = ['turn:localhost:3478', 'turns:localhost:443?transport=tcp'];
  const grant = { urls, username: 'guest', password: 'ephemeral', ttl_seconds: 1800 };
  const iceServers = [
    { urls: 'stun:localhost' },
    { urls: urls[0], username: grant.username, credential: grant.password },
    { urls: 'turn:other.example', username: grant.username, credential: 'other' },
    { urls: [...urls, 'turn:bad@example.org'], username: grant.username, credential: grant.password },
  ];
  let upstream = 0;
  const mf = new Miniflare(options(password, origin, relayOrigin, true, async request => {
    assert.equal(request.url, 'https://rtc.live.cloudflare.com/v1/turn/keys/fixture/credentials/generate-ice-servers');
    assert.equal(request.headers.get('authorization'), 'Bearer test-token');
    assert.deepEqual(await request.json(), { ttl: 1800 });
    upstream++;
    if (upstream <= 2) {
      const limit = 32 * 1024;
      const size = upstream === 1 ? limit + 1 : limit;
      const bytes = new TextEncoder().encode(JSON.stringify({ iceServers }).padEnd(size));
      return new Response(new ReadableStream({
        start(controller) {
          controller.enqueue(bytes.subarray(0, limit / 2));
          controller.enqueue(bytes.subarray(limit / 2));
          controller.close();
        },
      }), { status: 201 });
    }
    return Response.json({ iceServers }, { status: 201 });
  }));
  t.after(() => mf.dispose());
  const edge = await mf.getWorker('relay');
  const ui = await mf.getWorker('console');
  let cookie = '';
  const admin = (path, body) => ui.fetch(origin + '/api/' + path, {
    method: body ? 'POST' : 'GET',
    headers: { Origin: origin, Cookie: cookie, 'Content-Type': 'application/json' },
    ...(body ? { body: JSON.stringify(body) } : {}),
  });
  const issue = key => edge.fetch(relayOrigin + '/v2/turn/credentials', {
    method: 'POST', headers: { Authorization: 'Bearer ' + key },
  });
  const refusal = async (key, status, error) => {
    const response = await issue(key);
    assert.equal(response.status, status);
    assert.deepEqual(await response.json(), { error });
  };
  const discovery = async () => (await (await edge.fetch(relayOrigin + '/.well-known/windshare')).json()).managed_turn;
  const authority = (await mf.getBindings('relay')).ACCESS.getByName('authority');
  const changes = async () => (await authority.fetch('https://internal/__test/changes')).json();
  assert.equal(await discovery(), true);
  const empty = await changes();
  await refusal('invalid', 403, 'access_denied');
  assert.deepEqual(await changes(), empty);

  const login = await admin('login', { password });
  assert.equal(login.status, 200);
  cookie = login.headers.get('set-cookie').split(';')[0];
  assert.equal((await (await admin('session')).json()).capabilities.managed_turn, true);
  const created = await (await admin('credentials', { name: 'local TURN test' })).json();
  const key = created.configuration.access_key;
  assert.equal((await admin('turn/toggle', { enabled: false })).status, 200);
  const paused = await changes();
  await refusal(key, 403, 'turn_disabled');
  await refusal('invalid', 403, 'access_denied');
  assert.equal(await discovery(), true);
  assert.deepEqual(await changes(), paused);
  assert.equal(upstream, 0);

  assert.equal((await admin('turn/toggle', { enabled: true })).status, 200);
  const before = await changes();
  await refusal(key, 503, 'turn_unavailable');
  for (let i = 1; i < 20; i++) {
    const response = await issue(key);
    assert.equal(response.status, 200);
    assert.equal(response.headers.get('cache-control'), 'no-store');
    assert.deepEqual(await response.json(), grant);
  }
  const limited = await issue(key);
  assert.equal(limited.status, 429);
  assert.equal(limited.headers.get('retry-after'), '60');
  assert.deepEqual(await changes(), before);
  assert.equal(upstream, 20);

  assert.equal((await admin('credentials/revoke', { credential_id: created.credential.credential_id, terminate_shares: false })).status, 200);
  await refusal(key, 403, 'access_denied');
  assert.equal((await admin('turn/toggle', { enabled: false })).status, 200);
  await refusal(key, 403, 'access_denied');
});

test('AccessObject provisions partial indexes once per instance and runs zero DDL on online requests', { timeout: 30_000 }, async t => {
  const mf = new Miniflare(options(password, origin, 'https://127.0.0.1', true));
  t.after(() => mf.dispose());
  const consoleWorker = await mf.getWorker('console');
  const authority = (await mf.getBindings('relay')).ACCESS.getByName('authority');
  const audit = async () => (await authority.fetch('https://internal/__test/schema-audit')).json();
  const expectedIndexes = [
    'access_active_credentials',
    'access_entry_expiry',
    'access_pending_enrollments',
    'access_revoked_credentials',
  ];

  assert.deepEqual(await audit(), { indexes: [], ddlCount: 0 });
  const uninitializedTurn = await authority.fetch('https://internal/internal/authorize-turn', {
    method: 'POST', body: JSON.stringify({ digest: digest('unknown') }),
  });
  assert.equal(uninitializedTurn.status, 403);
  assert.deepEqual(await audit(), { indexes: [], ddlCount: 0 });

  const login = await consoleWorker.fetch(origin + '/api/login', {
    method: 'POST', headers: { 'Content-Type': 'application/json', Origin: origin },
    body: JSON.stringify({ password }),
  });
  assert.equal(login.status, 200);
  const cookie = login.headers.get('Set-Cookie').split(';')[0];
  assert.deepEqual(await audit(), { indexes: expectedIndexes, ddlCount: 8 });

  const admin = (path, body) => consoleWorker.fetch(origin + '/api/' + path, {
    method: body === undefined ? 'GET' : 'POST',
    headers: { Cookie: cookie, ...(body === undefined ? {} : { 'Content-Type': 'application/json', Origin: origin }) },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  assert.equal((await admin('credentials')).status, 200);
  assert.equal((await admin('credentials/revoked')).status, 200);
  assert.equal((await admin('enrollments')).status, 200);
  const createdRes = await admin('credentials', { name: 'schema-audit-key' });
  assert.equal(createdRes.status, 201);
  const created = await createdRes.json();
  const authorized = await authority.fetch('https://internal/internal/authorize', {
    method: 'POST',
    body: JSON.stringify({
      digest: digest(created.access_key),
      share_id: encode(randomBytes(12)),
      operation_id: 'schema-audit',
    }),
  });
  assert.equal(authorized.status, 200);
  const turnAuthorized = await authority.fetch('https://internal/internal/authorize-turn', {
    method: 'POST', body: JSON.stringify({ digest: digest(created.access_key) }),
  });
  assert.equal(turnAuthorized.status, 200);
  assert.equal((await admin('credentials/revoke', {
    credential_id: created.credential.credential_id,
    terminate_shares: false,
  })).status, 200);
  assert.equal((await admin('credentials/revoked')).status, 200);
  assert.equal((await authority.fetch('https://internal/__test/alarm')).status, 200);

  assert.deepEqual(await audit(), { indexes: expectedIndexes, ddlCount: 8 });
});
