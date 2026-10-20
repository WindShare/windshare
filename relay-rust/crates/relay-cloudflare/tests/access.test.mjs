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
function options(ownerPassword = password, consoleOrigin = origin, relayOrigin = 'https://127.0.0.1', faults = false) {
  const shared = { compatibilityDate: '2026-10-17',
    compatibilityFlags: ['web_socket_auto_reply_to_close'], modulesRoot: root };
  const module = (path, type = 'ESModule') => ({ type, path: root + path });
  return convertV4MiniflareOptions({ log: new Log(LogLevel.ERROR), workers: [
    { ...shared, name: 'relay', modules: [...(faults ? [module('tests/access-host.mjs')] : []), module('entry.mjs'), module('build/index.js'), module('build/index_bg.wasm', 'CompiledWasm')],
      bindings: { OWNER_PASSWORD: ownerPassword, CONSOLE_ORIGIN: consoleOrigin, RELAY_URL: relayOrigin, FRONTEND_URL: relayOrigin },
      durableObjects: { SHARES: { className: 'ShareRelayObject', useSQLite: true },
        ACCESS: { className: 'AccessObject', useSQLite: true } } },
    { ...shared, name: 'console', modules: [module('console-entry.mjs')], bindings: { CONSOLE_ORIGIN: consoleOrigin },
      durableObjects: { ACCESS: { className: 'AccessObject', scriptName: 'relay' } } },
  ] });
}

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
    await request('login', { password }, 403);
    const login = await request('login', { password }, 200, false, { Origin: origin });
    cookie = login.response.headers.get('Set-Cookie').split(';')[0];
    assert.match(login.response.headers.get('Set-Cookie'), /HttpOnly; Secure; SameSite=Strict/);
    await request('credentials', { name: 'cross site' }, 403, true, { Origin: 'https://127.0.0.1' });
    await request('credentials', { name: 'missing origin' }, 403, false, { Cookie: cookie });
    await request('enrollments', { device_name: 'invalid request' }, 400);

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
    await request('enrollments/installed', claim);
    await request('enrollments/installed', claim);
    await request('enrollments/cancel', { ...claim, claim_secret: claimant().claim_secret }, 403);

    const authority = (await mf.getBindings('relay')).ACCESS.getByName('authority');
    const authorize = async (accessKey, expected) => {
      const response = await authority.fetch('https://internal/internal/authorize', {
        method: 'POST', body: JSON.stringify({ digest: digest(accessKey), share_id: encode(randomBytes(12)), operation_id: 'test-operation' }),
      });
      assert.equal(response.status, expected, await response.text());
    };
    await authorize(credential.access_key, 200);
    await request('credentials/revoke', { credential_id: credential.credential_id, terminate_shares: false }, 200, true);
    await authorize(credential.access_key, 403);

    const second = claimant();
    const { data: incomplete } = await request('enrollments', { device_name: 'Cancelled device', claim_digest: second.claim_digest });
    await request('enrollments/approve', { enrollment_id: incomplete.enrollment_id, user_code: incomplete.user_code }, 200, true);
    const cancelClaim = { enrollment_id: incomplete.enrollment_id, claim_secret: second.claim_secret };
    const { data: abandoned } = await request('enrollments/claim', cancelClaim);
    await request('enrollments/installed', cancelClaim);
    await request('enrollments/cancel', cancelClaim);
    await request('enrollments/cancel', cancelClaim);
    await authorize(abandoned.access_key, 403);
    const { data: manual } = await request('credentials', { name: 'Automation' }, 201, true);
    assert.equal(manual.configuration.profiles[0].access_key, manual.access_key);

    await mf.setOptions(options('rotated-local-access-password'));
    consoleWorker = await mf.getWorker('console');
    await request('session', undefined, 401, true);
    await request('login', { password }, 401, false, { Origin: origin });
    const rotated = await request('login', { password: 'rotated-local-access-password' }, 200, false, { Origin: origin });
    cookie = rotated.response.headers.get('Set-Cookie').split(';')[0];
    const { data: list } = await request('credentials', undefined, 200, true);
    assert.equal(list.credentials.length, 2);
    assert.equal(list.credentials.find(item => item.credential_id === credential.credential_id).revoked, true);
    const newAuthority = (await mf.getBindings('relay')).ACCESS.getByName('authority');
    const preserved = await newAuthority.fetch('https://internal/internal/authorize', {
      method: 'POST', body: JSON.stringify({ digest: digest(manual.access_key), share_id: encode(randomBytes(12)), operation_id: 'after-rotation' }),
    });
    assert.equal(preserved.status, 200);
    await request('enrollments/installed', claim);
  } finally {
    await mf.dispose();
  }
});

test('production Go protocol clients observe revocation and durable administrative termination', { timeout: 60_000 }, async () => {
  const mf = new Miniflare(options());
  try {
    const address = await mf.ready;
    const relayOrigin = address.origin;
    const consoleAddress = new URL(relayOrigin);
    consoleAddress.hostname = 'localhost';
    await mf.setOptions({ ...options(password, consoleAddress.origin, relayOrigin), port: Number(address.port) });
    const { stdout } = await promisify(execFile)('go', ['run', './relay-rust/tests/_goclient',
      '-endpoint', relayOrigin + '/v2/ws', '-access-console', consoleAddress.origin], {
      cwd: fileURLToPath(new URL('../../../../', import.meta.url)), timeout: 45_000, windowsHide: true,
      env: { ...process.env, WINDSHARE_OWNER_PASSWORD: password }, maxBuffer: 128 * 1024,
    });
    assert.match(stdout, /PASS:/);
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
