import assert from 'node:assert/strict';
import { randomBytes, sign } from 'node:crypto';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import { derivePasswordVerifier, fixture } from './local-worker.mjs';

test('the Worker verifies client-derived password verifiers atomically inside the Durable Object', async t => {
  const { worker, mf, trust, nodeKey, node, activeNodeKey, activeNode, requests, mirrors, enrollment } = await fixture(t, {}, { activeNode: true });
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
    '/internal/fleet/stage',
    '/internal/fleet/tick',
    '/internal/accounts/login/complete',
    '/internal/admin/login/complete',
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

  // The first committed SQLite cursor can be delivered to KV with no node or frontend traffic.
  const namespace = await mf.getDurableObjectNamespace('AUTHORITY', 'authority');
  const object = namespace.get(namespace.idFromName('network-authority-v1'));
  const publicURL = 'https://localhost/.well-known/windshare-network';
  const preflight = await worker.fetch(publicURL, { method: 'OPTIONS', headers: { Origin: 'http://127.0.0.1:9090' } });
  assert.equal(preflight.status, 204, await preflight.text());
  assert.equal(preflight.headers.get('access-control-allow-origin'), '*');
  const cold = await worker.fetch(publicURL, { method: 'HEAD' });
  assert.equal(cold.headers.get('x-content-type-options'), 'nosniff');
  assert.equal(await cold.text(), '');
  const release = JSON.parse(await readFile(new URL('../../../../deploy/network/release.json', import.meta.url)));
  const decode = raw => JSON.parse(Buffer.from(raw, 'base64url'));
  const payload = decode(release.snapshot.payload), grant = decode(release.snapshot.certificate.payload);
  const usable = Date.now() + 300_000 >= Date.parse(payload.issued_at) && Date.now() < Math.min(Date.parse(grant.expires_at), Date.parse(payload.expires_at) + 7 * 86400_000);
  assert.equal(cold.status, usable ? 200 : 503);
  for (const unknownPath of ['/v1/vantage/unknown', '/v1/maintenance/unknown']) {
    const unknown = await worker.fetch('https://localhost' + unknownPath, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
    assert.equal(unknown.status, 404, unknownPath);
  }
  const unknownInternal = await object.fetch('https://authority/internal/fleet/unknown', {
    method: 'POST',
    body: '{}',
  });
  assert.equal(unknownInternal.status, 404);
  const wrongMethod = await worker.fetch('https://localhost/v1/admin/nodes/manage', {
    method: 'GET',
  });
  assert.equal(wrongMethod.status, 404);
  const tick = await object.fetch('https://authority/internal/fleet/tick', { method: 'POST', body: '{}' });
  assert.equal(tick.status, 200);
  assert.equal(tick.headers.get('x-directory-mirror-defer'), 'kv_just_published');
  const tickBody = await tick.json();
  const settlingTick = await object.fetch('https://authority/internal/fleet/tick', { method: 'POST', body: '{}' });
  assert.equal(settlingTick.status, 200);
  assert.equal(settlingTick.headers.get('x-directory-mirror-defer'), 'kv_settling');
  await object.fetch('https://authority/__test/age-delivery', {
    method: 'POST',
    body: JSON.stringify({ ms: 120_000 }),
  });
  const steadyTick = await object.fetch('https://authority/internal/fleet/tick', { method: 'POST', body: '{}' });
  assert.equal(steadyTick.status, 200);
  assert.equal(steadyTick.headers.get('x-directory-mirror-defer'), null);
  const directory = await worker.fetch('https://localhost/.well-known/windshare-network');
  assert.equal(directory.status, usable ? 200 : 503);
  if (usable) assert.deepEqual(await directory.json(), tickBody);
  else assert.equal(directory.headers.get('cache-control'), 'no-store');

  // Single-transaction admin initialize, login, console username, and password rotation.
  const operatorVerifier = derivePasswordVerifier('admin', 'operator', 'Operator-Password-2026!');
  const wrongAdminVerifier = derivePasswordVerifier('admin', 'operator', 'wrong-password-attempt');
  const rotatedAdminVerifier = derivePasswordVerifier('admin', 'operator', 'Operator-Rotated-Password-2026!');
  const operator = { username: 'operator', password_verifier: operatorVerifier };
  await call('/v1/admin/initialize', {
    username: 'operator', password: 'Operator-Password-2026!', token: 'local-initialization-token',
  }, { status: 400 });
  const initialized = await call('/v1/admin/initialize', { ...operator, token: 'local-initialization-token' });
  adminCookie = initialized.response.headers.get('set-cookie').split(';')[0];
  csrf = initialized.value.csrf;
  const consoleRes = await worker.fetch('https://localhost/v1/admin/console', {
    headers: { Cookie: adminCookie },
  });
  assert.equal(consoleRes.status, 200);
  assert.equal((await consoleRes.json()).username, 'operator');

  await call('/v1/admin/initialize', { ...operator, token: 'local-initialization-token' }, { status: 403 });
  await call('/v1/admin/login', { username: operator.username, password_verifier: wrongAdminVerifier }, { status: 403 });
  const adminLogin = await call('/v1/admin/login', operator);
  const priorAdminCookie = adminCookie;
  adminCookie = adminLogin.response.headers.get('set-cookie').split(';')[0];
  csrf = adminLogin.value.csrf;
  await call('/v1/admin/password', {
    current_password_verifier: wrongAdminVerifier, new_password_verifier: rotatedAdminVerifier,
  }, { admin: true, status: 403 });
  await call('/v1/admin/password', {
    current_password_verifier: operatorVerifier, new_password_verifier: 'malformed-verifier',
  }, { admin: true, status: 400 });
  const passwordChanged = await call('/v1/admin/password', {
    current_password_verifier: operatorVerifier, new_password_verifier: rotatedAdminVerifier,
  }, { admin: true });
  adminCookie = passwordChanged.response.headers.get('set-cookie').split(';')[0];
  csrf = passwordChanged.value.csrf;
  const staleConsole = await worker.fetch('https://localhost/v1/admin/console', {
    headers: { Cookie: priorAdminCookie },
  });
  assert.equal(staleConsole.status, 403);
  await call('/v1/admin/login', operator, { status: 403 });
  const relogged = await call('/v1/admin/login', { username: operator.username, password_verifier: rotatedAdminVerifier });
  adminCookie = relogged.response.headers.get('set-cookie').split(';')[0];
  csrf = relogged.value.csrf;
  const dismissed = await call('/v1/admin/reviews/dismiss', { count: 1000 }, { admin: true });
  assert.deepEqual(dismissed.value, []);
  await call('/v1/admin/reviews/dismiss', { count: 0 }, { admin: true, status: 400 });

  // Password registration + idempotent retry + login + malformed verifier rejection.
  const secret = randomBytes(32).toString('base64url');
  const firstVerifier = derivePasswordVerifier('account', 'alice', 'Correct-Horse-Battery-1');
  const secondVerifier = derivePasswordVerifier('account', 'alice', 'Correct-Horse-Battery-2');
  const wrongAccountVerifier = derivePasswordVerifier('account', 'alice', 'wrong-password-attempt');
  await call('/v1/accounts', { client_secret: secret, username: 'alice' }, { status: 400 });
  await call('/v1/accounts', { client_secret: secret, username: 'alice', password: 'plaintext-rejected' }, { status: 400 });
  const created = (await call('/v1/accounts', {
    client_secret: secret, username: 'alice', password_verifier: firstVerifier,
  }, { source: '198.51.100.10' })).value;
  const retried = (await call('/v1/accounts', {
    client_secret: secret, username: 'alice', password_verifier: firstVerifier,
  }, { source: '198.51.100.10' })).value;
  assert.deepEqual(retried.account, created.account);
  assert.deepEqual(retried.credential, created.credential);

  await call('/v1/accounts/login', { username: 'alice', password_verifier: 'not-a-32-byte-verifier' }, {
    status: 400, source: '198.51.100.11',
  });
  await call('/v1/accounts/login', { username: 'alice', password_verifier: wrongAccountVerifier }, {
    status: 403, source: '198.51.100.11',
  });
  const loggedIn = (await call('/v1/accounts/login', { username: 'alice', password_verifier: firstVerifier }, {
    source: '198.51.100.11',
  })).value;
  assert.deepEqual(loggedIn.account, created.account);
  assert.notDeepEqual(loggedIn.credential, created.credential);

  // Blocked account status stays hidden until the password verifier actually matches.
  await call('/v1/admin/blocks', { kind: 'account', id: created.account, blocked: true }, { admin: true });
  const blockedWrong = await call('/v1/accounts/login', { username: 'alice', password_verifier: wrongAccountVerifier }, {
    status: 403, source: '198.51.100.15',
  });
  assert.equal(blockedWrong.value.reason, 'credentials');
  const blockedRight = await call('/v1/accounts/login', { username: 'alice', password_verifier: firstVerifier }, {
    status: 403, source: '198.51.100.16',
  });
  assert.equal(blockedRight.value.reason, 'account_blocked');
  await call('/v1/admin/blocks', { kind: 'account', id: created.account, blocked: false }, { admin: true });

  // Device login with verifier, then password A -> B -> A keeps old cookie invalidated.
  assert.equal(requests.length, 0);
  await call('/v1/admin/nodes/discover', { url: enrollment.url }, { status: 403 });
  assert.equal(requests.length, 0);
  const discovered = (await call('/v1/admin/nodes/discover', { url: enrollment.url }, { admin: true })).value;
  assert.equal(discovered.node_key, enrollment.node_key);
  const staged = (await call('/v1/admin/nodes', enrollment, { admin: true })).value;
  assert.equal(staged.state.check.kind, 'queued');
  await call('/v1/admin/nodes/cancel', { node_key: enrollment.node_key, operation_id: staged.operation_id }, { admin: true });
  const replacement = (await call('/v1/admin/nodes', enrollment, { admin: true })).value;
  assert.notEqual(replacement.operation_id, staged.operation_id);
  await call('/v1/admin/nodes/cancel', { node_key: enrollment.node_key, operation_id: staged.operation_id }, { admin: true, status: 409 });
  const mirrorURLs = [release.link_host, release.backup_entry].map(origin => origin + '/.well-known/windshare-network');
  for (const url of mirrorURLs) mirrors.set(url, release.snapshot);
  const promotion = () => worker.fetch('https://localhost/v1/admin/nodes/promote', {
    method: 'POST', headers: { 'Content-Type': 'application/json', Origin: 'https://localhost', Cookie: adminCookie, 'X-CSRF-Token': csrf },
    body: JSON.stringify({ node_key: enrollment.node_key, operation_id: replacement.operation_id, status: 'canary' }),
  });
  const matched = await promotion();
  assert.equal(matched.status, 503);
  assert.equal(await matched.text(), 'fleet temporarily unavailable');
  assert.deepEqual(new Set(requests.slice(-2)), new Set(mirrorURLs));
  mirrors.set(mirrorURLs[0], {});
  const lagging = await promotion();
  assert.equal(lagging.status, 403);
  assert.equal((await lagging.json()).reason, 'mirror_lag');
  const signInvitation = (targetNode, targetKey, origin) => {
    const invitePayload = Buffer.from(JSON.stringify({
      node: targetNode, issuer: [...Buffer.from(trust.root, 'base64url')],
      enrollment: [...randomBytes(16)], origin,
      device: 'Laptop', code: 'ABCD-EFGH', expires_ms: Date.now() + 60_000,
    }));
    return Buffer.concat([
      invitePayload,
      sign(null, Buffer.concat([Buffer.from('windshare/device-invitation/v1\0'), invitePayload]), targetKey),
    ]).toString('base64url');
  };
  const stagedInvitation = signInvitation(node, nodeKey, 'https://relay.example');
  const blocked = await call('/v1/device/approve', {
    invitation: stagedInvitation, login: { username: 'alice', password_verifier: firstVerifier },
  }, { source: '198.51.100.12', status: 403 });
  assert.equal(blocked.value.reason, 'node_unavailable');
  await call('/v1/admin/nodes/cancel', { node_key: enrollment.node_key, operation_id: replacement.operation_id }, { admin: true });
  const cancelled = await call('/v1/device/approve', {
    invitation: stagedInvitation, login: { username: 'alice', password_verifier: firstVerifier },
  }, { source: '198.51.100.12', status: 403 });
  assert.equal(cancelled.value.reason, 'node_unavailable');
  const invitation = signInvitation(activeNode, activeNodeKey, 'https://active.example');
  const approved = await call('/v1/device/approve', {
    invitation, login: { username: 'alice', password_verifier: firstVerifier },
  }, { source: '198.51.100.12' });
  const deviceCookie = approved.response.headers.get('set-cookie').split(';')[0];
  assert.match(deviceCookie, /^windshare_service_login=/);
  await call('/v1/device/approve', { invitation, username: 'alice' }, {
    cookie: deviceCookie, source: '198.51.100.12',
  });

  // Malformed verifier rejects without spending rate limits; A -> B -> A invalidates cookie.
  await call('/v1/accounts/password', { username: 'alice', password_verifier: 'short' }, {
    secret, source: '198.51.100.20', status: 400,
  });
  await call('/v1/accounts/password', { username: 'alice', password_verifier: secondVerifier }, {
    secret, source: '198.51.100.20',
  });
  await call('/v1/accounts/password', { username: 'alice', password_verifier: firstVerifier }, {
    secret, source: '198.51.100.21',
  });
  await call('/v1/device/approve', { invitation, username: 'alice' }, {
    cookie: deviceCookie, status: 403, source: '198.51.100.12',
  });
  for (let i = 0; i < 3; i++) {
    await call('/v1/accounts/password', { username: 'alice', password_verifier: secondVerifier }, {
      secret, source: `198.51.100.${30 + i}`,
    });
  }
  const limited = await call('/v1/accounts/password', { username: 'alice', password_verifier: firstVerifier }, {
    secret, source: '198.51.100.40', status: 429,
  });
  assert.equal(limited.value.kind, 'rate_limited');
});

test('the Worker fails closed when PASSWORD_PEPPER is missing or malformed', async t => {
  const { worker } = await fixture(t, { PASSWORD_PEPPER: '' });
  const response = await worker.fetch('https://localhost/v1/admin/initialize', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Origin: 'https://localhost' },
    body: JSON.stringify({
      username: 'operator',
      password_verifier: Buffer.alloc(32, 1).toString('base64url'),
      token: 'local-initialization-token',
    }),
  });
  assert.equal(response.status, 503);
});
