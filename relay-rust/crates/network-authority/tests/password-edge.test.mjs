import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createPrivateKey, createPublicKey, randomBytes, sign } from 'node:crypto';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const crate = fileURLToPath(new URL('../', import.meta.url));
const { Miniflare, Log, LogLevel, convertV4MiniflareOptions } =
  createRequire(new URL('../../relay-cloudflare/package.json', import.meta.url))('miniflare');

test('the Worker runs password hashing and rate limits across edge and Durable Object boundaries', async t => {
  const keys = await mkdtemp(join(tmpdir(), 'windshare-password-edge-test-'));
  t.after(() => rm(keys, { recursive: true, force: true }));
  execFileSync('cargo', ['run', '--quiet', '--locked', '--bin', 'network-authority-keys', '--', keys, '1'], { cwd: crate });
  const trust = JSON.parse(await readFile(join(keys, 'trust-1.json'), 'utf8'));
  const build = resolve(crate, process.env.AUTHORITY_TEST_BUILD || 'build');
  const mf = new Miniflare(convertV4MiniflareOptions({
    log: new Log(LogLevel.ERROR),
    workers: [{
      name: 'authority', compatibilityDate: '2026-09-28', modulesRoot: build,
      modules: [{ type: 'ESModule', path: join(build, 'index.js') },
        { type: 'CompiledWasm', path: join(build, 'index_bg.wasm') }],
      bindings: {
        AUTHORITY_ROOT: trust.root,
        AUTHORITY_CERTIFICATE: trust.certificate,
        AUTHORITY_SIGNING_SEED: (await readFile(join(keys, 'online-1.seed'), 'utf8')).trim(),
        INITIALIZATION_TOKEN: 'local-initialization-token',
      },
      durableObjects: { AUTHORITY: { className: 'NetworkAuthority', useSQLite: true } },
    }],
  }));
  t.after(() => mf.dispose());
  const worker = await mf.getWorker('authority');

  // Hashed console bundles are immutable while HTML entrypoints remain uncached.
  const consolePage = await worker.fetch('https://localhost/console');
  assert.equal(consolePage.status, 200);
  assert.equal(consolePage.headers.get('cache-control'), 'no-store');
  const assetPath = (await consolePage.text()).match(/src="(\/assets\/[^"]+)"/)?.[1];
  assert(assetPath, 'expected hashed asset path in /console HTML');
  const consoleAsset = await worker.fetch('https://localhost' + assetPath);
  assert.equal(consoleAsset.status, 200);
  assert.equal(consoleAsset.headers.get('cache-control'), 'public, max-age=31536000, immutable');
  assert.equal(consoleAsset.headers.get('vary'), 'Origin');

  // External callers can never invoke internal DO orchestration routes.
  for (const internalPath of [
    '/internal/accounts/login/complete',
    '/internal/accounts/password/commit',
    '/internal/admin/login/complete',
    '/internal/admin/password/commit',
  ]) {
    const blocked = await worker.fetch('https://localhost' + internalPath, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Origin: 'https://localhost' },
      body: JSON.stringify({}),
    });
    assert.equal(blocked.status, 404, internalPath);
  }

  let adminCookie = '';
  let csrf = '';
  async function call(path, body, { status = 200, secret, admin = false, cookie, source } = {}) {
    const headers = { 'Content-Type': 'application/json', Origin: 'https://localhost' };
    if (source) headers['CF-Connecting-IP'] = source;
    if (secret) headers.Authorization = 'Bearer ' + secret;
    if (admin) Object.assign(headers, { Cookie: adminCookie, 'X-CSRF-Token': csrf });
    if (cookie) headers.Cookie = cookie;
    const response = await worker.fetch('https://localhost' + path, {
      method: 'POST', headers, body: JSON.stringify(body),
    });
    const value = await response.json();
    assert.equal(response.status, status, JSON.stringify(value));
    return { response, value };
  }

  // Two-phase admin initialize and login.
  const operator = { username: 'operator', password: 'Operator-Password-2026!' };
  const initialized = await call('/v1/admin/initialize', { ...operator, token: 'local-initialization-token' });
  adminCookie = initialized.response.headers.get('set-cookie').split(';')[0];
  csrf = initialized.value.csrf;
  await call('/v1/admin/initialize', { ...operator, token: 'local-initialization-token' }, { status: 403 });
  await call('/v1/admin/login', { username: operator.username, password: 'wrong-password-attempt' }, { status: 403 });
  const adminLogin = await call('/v1/admin/login', operator);
  const priorAdminCookie = adminCookie;
  adminCookie = adminLogin.response.headers.get('set-cookie').split(';')[0];
  csrf = adminLogin.value.csrf;
  const rotatedAdmin = 'Operator-Rotated-Password-2026!';
  await call('/v1/admin/password', {
    current_password: 'wrong-password-attempt', new_password: rotatedAdmin,
  }, { admin: true, status: 403 });
  await call('/v1/admin/password', {
    current_password: operator.password, new_password: 'short',
  }, { admin: true, status: 400 });
  const passwordChanged = await call('/v1/admin/password', {
    current_password: operator.password, new_password: rotatedAdmin,
  }, { admin: true });
  adminCookie = passwordChanged.response.headers.get('set-cookie').split(';')[0];
  csrf = passwordChanged.value.csrf;
  const staleConsole = await worker.fetch('https://localhost/v1/admin/console', {
    headers: { Cookie: priorAdminCookie },
  });
  assert.equal(staleConsole.status, 403);
  await call('/v1/admin/login', operator, { status: 403 });
  const relogged = await call('/v1/admin/login', { ...operator, password: rotatedAdmin });
  adminCookie = relogged.response.headers.get('set-cookie').split(';')[0];
  csrf = relogged.value.csrf;
  const dismissed = await call('/v1/admin/reviews/dismiss', { count: 1000 }, { admin: true });
  assert.deepEqual(dismissed.value, []);
  await call('/v1/admin/reviews/dismiss', { count: 0 }, { admin: true, status: 400 });

  // Password registration + idempotent retry + login + edge oversized password rejection.
  const secret = randomBytes(32).toString('base64url');
  const firstPassword = 'Correct-Horse-Battery-1';
  const secondPassword = 'Correct-Horse-Battery-2';
  await call('/v1/accounts', { client_secret: secret, username: 'alice' }, { status: 400 });
  const created = (await call('/v1/accounts', {
    client_secret: secret, username: 'alice', password: firstPassword,
  }, { source: '198.51.100.10' })).value;
  const retried = (await call('/v1/accounts', {
    client_secret: secret, username: 'alice', password: firstPassword,
  }, { source: '198.51.100.10' })).value;
  assert.deepEqual(retried.account, created.account);
  assert.deepEqual(retried.credential, created.credential);

  await call('/v1/accounts/login', { username: 'alice', password: 'a'.repeat(300) }, {
    status: 403, source: '198.51.100.11',
  });
  await call('/v1/accounts/login', { username: 'alice', password: 'wrong-password-attempt' }, {
    status: 403, source: '198.51.100.11',
  });
  const loggedIn = (await call('/v1/accounts/login', { username: 'alice', password: firstPassword }, {
    source: '198.51.100.11',
  })).value;
  assert.deepEqual(loggedIn.account, created.account);
  assert.notDeepEqual(loggedIn.credential, created.credential);

  // Blocked account status stays hidden until the password actually matches.
  await call('/v1/admin/blocks', { kind: 'account', id: created.account, blocked: true }, { admin: true });
  const blockedWrong = await call('/v1/accounts/login', { username: 'alice', password: 'wrong-password-attempt' }, {
    status: 403, source: '198.51.100.15',
  });
  assert.equal(blockedWrong.value.reason, 'credentials');
  const blockedRight = await call('/v1/accounts/login', { username: 'alice', password: firstPassword }, {
    status: 403, source: '198.51.100.16',
  });
  assert.equal(blockedRight.value.reason, 'account_blocked');
  await call('/v1/admin/blocks', { kind: 'account', id: created.account, blocked: false }, { admin: true });

  // Device login with password, then password A -> B -> A keeps old cookie invalidated.
  const nodeKey = createPrivateKey({
    key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), randomBytes(32)]),
    type: 'pkcs8', format: 'der',
  });
  const node = [...createPublicKey(nodeKey).export({ type: 'spki', format: 'der' }).subarray(-32)];
  await call('/v1/admin/nodes', { key: node, zone: 'global', region: 'edge-test' }, { admin: true });
  const invitePayload = Buffer.from(JSON.stringify({
    node, issuer: [...Buffer.from(trust.root, 'base64url')],
    enrollment: [...randomBytes(16)], origin: 'https://relay.example',
    device: 'Laptop', code: 'ABCD-EFGH', expires_ms: Date.now() + 60_000,
  }));
  const invitation = Buffer.concat([
    invitePayload,
    sign(null, Buffer.concat([Buffer.from('windshare/device-invitation/v1\0'), invitePayload]), nodeKey),
  ]).toString('base64url');
  const approved = await call('/v1/device/approve', {
    invitation, login: { username: 'alice', password: firstPassword },
  }, { source: '198.51.100.12' });
  const deviceCookie = approved.response.headers.get('set-cookie').split(';')[0];
  assert.match(deviceCookie, /^windshare_service_login=/);
  await call('/v1/device/approve', { invitation, username: 'alice' }, {
    cookie: deviceCookie, source: '198.51.100.12',
  });

  // Short password rejects at the edge without spending rate limits; A -> B -> A invalidates cookie.
  await call('/v1/accounts/password', { username: 'alice', password: 'short' }, {
    secret, source: '198.51.100.20', status: 400,
  });
  await call('/v1/accounts/password', { username: 'alice', password: secondPassword }, {
    secret, source: '198.51.100.20',
  });
  await call('/v1/accounts/password', { username: 'alice', password: firstPassword }, {
    secret, source: '198.51.100.21',
  });
  await call('/v1/device/approve', { invitation, username: 'alice' }, {
    cookie: deviceCookie, status: 403, source: '198.51.100.12',
  });
  for (let i = 0; i < 3; i++) {
    await call('/v1/accounts/password', { username: 'alice', password: secondPassword }, {
      secret, source: `198.51.100.${30 + i}`,
    });
  }
  const limited = await call('/v1/accounts/password', { username: 'alice', password: firstPassword }, {
    secret, source: '198.51.100.40', status: 429,
  });
  assert.equal(limited.value.kind, 'rate_limited');
});
