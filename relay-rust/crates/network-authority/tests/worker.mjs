import assert from 'node:assert/strict';
import { createPrivateKey, createPublicKey, randomBytes, sign, verify } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';

import test from 'node:test';
import { derivePasswordVerifier, fixture } from './local-worker.mjs';

test('the local Worker preserves account and node policy across signed reports and retries', async t => {
  const { worker, trust, keys, nodeKey: privateKey, node, enrollment: proposed } = await fixture(t);
  const url = new URL('https://localhost');
  const request = (input, options) => worker.fetch(String(input), options);
  const initialization = 'local-initialization-token';
  const enrollment = { ...proposed, zone: 'cn' };
  const adminPassword = 'Local-P2-validation-2026';
  const rotatedAdminPassword = 'Rotated-P2-validation-2026';
  const adminVerifier = derivePasswordVerifier('admin', 'local-operator', adminPassword);
  const wrongAdminVerifier = derivePasswordVerifier('admin', 'local-operator', 'wrong-password-attempt');
  const rotatedAdminVerifier = derivePasswordVerifier('admin', 'local-operator', rotatedAdminPassword);
  const administrator = { username: 'local-operator', password_verifier: adminVerifier };
  let cookie = '';
  let csrf = '';
  async function call(path, body, options = {}) {
    const headers = { Origin: url.origin, ...options.headers };
    if (!Buffer.isBuffer(body)) headers['Content-Type'] = 'application/json';
    if (options.admin) Object.assign(headers, { Cookie: cookie, 'X-CSRF-Token': csrf });
    if (options.secret) headers.Authorization = 'Bearer ' + options.secret;
    Object.assign(headers, options.headers);
    const response = await request(new URL(path, url), {
      method: options.method ?? 'POST', headers,
      body: body === undefined ? undefined : Buffer.isBuffer(body) ? body : JSON.stringify(body),
      signal: AbortSignal.timeout(20_000),
    });
    assert.equal(response.headers.get('cache-control'), 'no-store');
    assert.equal(response.headers.get('access-control-allow-origin'), url.origin);
    const value = await response.json();
    assert.equal(response.status, options.status ?? 200, JSON.stringify(value));
    return { response, value };
  }
  const consolePage = await request(new URL('/console', url), { signal: AbortSignal.timeout(20_000) });
  assert.equal(consolePage.status, 200);
  assert.equal(consolePage.headers.get('cache-control'), 'no-store');
  const consoleHtml = await consolePage.text();
  const assetPath = consoleHtml.match(/src="(\/assets\/[^"]+)"/)?.[1];
  assert(assetPath, 'expected hashed console asset in /console HTML');
  const consoleAsset = await request(new URL(assetPath, url), { signal: AbortSignal.timeout(20_000) });
  assert.equal(consoleAsset.status, 200);
  assert.equal(consoleAsset.headers.get('cache-control'), 'public, max-age=31536000, immutable');
  const initialized = await request(new URL('/v1/admin/initialize', url), {
    method: 'POST', headers: { Origin: url.origin, 'Content-Type': 'application/json' },
    body: JSON.stringify({ ...administrator, token: initialization }),
  });
  assert([200, 403].includes(initialized.status), await initialized.text());
  const login = await call('/v1/admin/login', administrator);
  cookie = login.response.headers.get('set-cookie').split(';')[0];
  csrf = login.value.csrf;
  assert.match(login.response.headers.get('set-cookie'), /HttpOnly; Secure; SameSite=Strict/);
  await call('/v1/admin/initialize', { ...administrator, token: initialization }, { status: 403 });
  const badCsrf = await call('/v1/admin/settings', {}, {
    admin: true, method: 'PATCH', headers: { 'X-CSRF-Token': 'invalid' }, status: 403,
  });
  assert.equal(badCsrf.value.reason, 'csrf');
  const badPasswordChange = await call('/v1/admin/password', {
    current_password_verifier: wrongAdminVerifier, new_password_verifier: rotatedAdminVerifier,
  }, { admin: true, status: 403 });
  assert.equal(badPasswordChange.value.reason, 'credentials');
  const oldCookie = cookie;
  const oldCsrf = csrf;
  const changedPassword = await call('/v1/admin/password', {
    current_password_verifier: adminVerifier, new_password_verifier: rotatedAdminVerifier,
  }, { admin: true });
  cookie = changedPassword.response.headers.get('set-cookie').split(';')[0];
  csrf = changedPassword.value.csrf;
  await call('/v1/admin/console', undefined, {
    method: 'GET', headers: { Cookie: oldCookie, 'X-CSRF-Token': oldCsrf }, status: 403,
  });
  await call('/v1/admin/login', administrator, { status: 403 });
  await call('/v1/admin/login', { username: administrator.username, password_verifier: rotatedAdminVerifier });
  const restoredPassword = await call('/v1/admin/password', {
    current_password_verifier: rotatedAdminVerifier, new_password_verifier: adminVerifier,
  }, { admin: true });
  cookie = restoredPassword.response.headers.get('set-cookie').split(';')[0];
  csrf = restoredPassword.value.csrf;
  const dismissedReviews = await call('/v1/admin/reviews/dismiss', { count: 1000 }, { admin: true });
  assert.deepEqual(dismissedReviews.value, []);
  await call('/v1/admin/reviews/dismiss', { count: 0 }, { admin: true, status: 400 });
  const missingCookie = await call('/v1/admin/logout', {}, {
    admin: true, headers: { Cookie: '' }, status: 403,
  });
  assert.equal(missingCookie.value.reason, 'administrator_session');
  const consoleState = (await call('/v1/admin/console', undefined, { admin: true, method: 'GET' })).value;
  assert.equal(consoleState.username, administrator.username);
  await call('/v1/admin/settings', { registration: 'automatic' }, { admin: true, method: 'PATCH' });
  const secret = randomBytes(32).toString('base64url');
  const first = (await call('/v1/accounts', { client_secret: secret })).value;
  const repeated = (await call('/v1/accounts', { client_secret: secret })).value;
  assert.deepEqual(repeated.account, first.account);
  assert.deepEqual(repeated.credential, first.credential);
  const refreshed = (await call('/v1/accounts/ticket', {}, { secret })).value;
  assert.deepEqual(refreshed.credential, first.credential);
  const username = 'probe-' + randomBytes(6).toString('hex');
  const accountVerifier = derivePasswordVerifier('account', username, adminPassword);
  await call('/v1/accounts/password', { username, password_verifier: accountVerifier }, { secret });
  const second = (await call('/v1/accounts/login', { username, password_verifier: accountVerifier })).value;
  assert.deepEqual(second.account, first.account);
  assert.notDeepEqual(second.credential, first.credential);
  const enrolled = (await call('/v1/admin/nodes', enrollment, { admin: true })).value;
  assert(enrolled.admission === 'accepting' && !enrolled.anonymous && enrolled.version === 1);
  const update = (field, value) => call('/v1/admin/nodes/policy', { key: node, change: { field, value } }, { admin: true, method: 'PUT' });
  await update('anonymous_sharing', true);
  await call('/v1/admin/nodes/policy', { key: node, change: { field: 'admission', value: 'accepting' } }, { admin: true, method: 'PUT', status: 400 });
  const existing = (await call('/v1/admin/nodes', enrollment, { admin: true })).value;
  assert.equal(existing.spec.zone, 'cn');
  assert(existing.admission === 'accepting' && existing.anonymous && existing.version === 2);
  const batch = {
    id: [...randomBytes(16)], node, issuer: [...Buffer.from(trust.root, 'base64url')],
    created_at: Math.floor(Date.now() / 1000), records: [], resident_accounts: [first.account],
    load: { applied_policy_version: 2, resident_shares: 0, active_sessions: 0, wire_bytes: 0, forwarded_bytes: 0, admission_refusals: 0 },
  };
  const onlineSeed = (await readFile(join(keys, 'online-1.seed'), 'utf8')).trim();
  const onlineKey = createPublicKey(createPrivateKey({ key: Buffer.concat([
    Buffer.from('302e020100300506032b657004220420', 'hex'), Buffer.from(onlineSeed, 'base64url'),
  ]), type: 'pkcs8', format: 'der' }));
  async function nodeRequest(path, requestDomain, responseDomain, value) {
    const body = Buffer.from(JSON.stringify(value));
    const signature = sign(null, Buffer.concat([Buffer.from(requestDomain), body]), privateKey);
    const reply = (await call(path, Buffer.concat([body, signature]))).value;
    const envelope = Buffer.from(reply.body, 'base64url');
    assert(verify(null, Buffer.concat([Buffer.from(responseDomain), envelope.subarray(0, -64)]),
      onlineKey, envelope.subarray(-64)));
    const decoded = JSON.parse(envelope.subarray(0, -64));
    assert.deepEqual(decoded.request, value.id);
    return decoded.result;
  }
  const usage = value => nodeRequest('/v1/node/usage',
    'windshare/node-usage/v1\0', 'windshare/authority-response/v2\0', value);
  const status = () => usage({ ...batch, id: [...randomBytes(16)] });
  const initial = await status();
  assert.equal(initial.kind, 'status');
  const adminGet = { admin: true, method: 'GET' };
  const accountsEndpoint = '/v1/admin/accounts';
  const accountId = Buffer.from(first.account).toString('base64url');
  const accountDetail = accountsEndpoint + '/' + accountId;
  await call(accountsEndpoint + '?after=invalid', undefined, { method: 'GET', status: 403 });
  const { value: accounts } = await call(accountsEndpoint, undefined, adminGet);
  const registered = accounts.entries.find(entry => Buffer.from(entry.id).equals(Buffer.from(first.account)));
  assert.equal(registered?.username, username);
  const { value: foundAccount } = await call(accountsEndpoint + '/lookup?q=' + username, undefined, adminGet);
  assert.deepEqual(foundAccount, registered);
  await call(accountsEndpoint + '/lookup?q=', undefined, { ...adminGet, status: 400 });
  await call(accountsEndpoint + '/' + Buffer.alloc(16).toString('base64url'), undefined, { ...adminGet, status: 400 });
  const nodesEndpoint = '/v1/admin/nodes';
  const encodedNodeKey = Buffer.from(node).toString('base64url');
  const nodeDetail = nodesEndpoint + '/' + encodedNodeKey;
  await call(nodeDetail, undefined, { method: 'GET', status: 403 });
  const { value: inspected } = await call(nodeDetail, undefined, adminGet);
  assert.deepEqual(inspected.key, node);
  assert.equal(inspected.version, existing.version);
  assert.deepEqual(inspected.report.load, batch.load);
  const liveLoad = { ...batch.load, active_sessions: 3 };
  await usage({ ...batch, id: [...randomBytes(16)], created_at: batch.created_at + 1, load: liveLoad });
  const reenrolled = (await call('/v1/admin/nodes', enrollment, { admin: true })).value;
  assert.deepEqual(reenrolled.report.load, liveLoad);
  const { value: nextNodes } = await call(nodesEndpoint + '?after=' + encodedNodeKey, undefined, adminGet);
  assert(!nextNodes.entries.some(item => Buffer.from(item.key).equals(Buffer.from(node))));
  await call(nodesEndpoint + '?after=invalid', undefined, { ...adminGet, status: 400 });
  const { value: missingNode } = await call(nodesEndpoint + '/' + Buffer.from(randomBytes(32)).toString('base64url'),
    undefined, { ...adminGet, status: 404 });
  assert.equal(missingNode.kind, 'node_not_found');
  const allowance = initial.entitlements[0].entitlement;
  const day = Math.floor(batch.created_at / 86400);
  const report = {
    ...batch, resident_accounts: [], records: [{ account: first.account, day, bytes: 12345 }],
  };
  assert.deepEqual(await usage(report), { kind: 'usage_received' });
  const remaining = (await status()).entitlements[0].entitlement.remaining_bytes;
  assert.equal(remaining, allowance.remaining_bytes - 12345);
  assert.deepEqual(await usage(report), { kind: 'usage_received' });
  assert.deepEqual(await usage({ ...report, id: [...randomBytes(16)], records: [{ account: first.account, day: day - 1, bytes: 6789 }] }),
    { kind: 'usage_received' });
  assert.equal((await status()).entitlements[0].entitlement.remaining_bytes, remaining);
  await call('/v1/accounts/credentials/revoke', { credential: first.credential }, { secret: second.credential_secret });
  await call('/v1/accounts/ticket', {}, { secret, status: 403 });
  await call('/v1/accounts/ticket', {}, { secret: second.credential_secret });
  const share = [...randomBytes(12)];
  const reason = 'Local authority integration probe';
  const { value: { id } } = await call('/v1/reports', { share, reason });
  const reportEndpoint = '/v1/admin/reports';
  const detail = `${reportEndpoint}/${Buffer.from(id).toString('base64url')}`;
  assert.equal(consoleState.reports, undefined);
  await call(reportEndpoint, undefined, { method: 'GET', status: 403 });
  const { value: reports } = await call(reportEndpoint, undefined, adminGet);
  const filed = reports.reports.find(item => Buffer.from(item.id).equals(Buffer.from(id)));
  assert.deepEqual(filed?.share, share);
  assert.equal(filed.reason, reason);
  assert.deepEqual((await call(detail, undefined, adminGet)).value, filed);
  const continued = await call(`${reportEndpoint}?after=${filed.created_at}.${Buffer.from(id).toString('base64url')}`, undefined, adminGet);
  assert(!continued.value.reports.some(item => Buffer.from(item.id).equals(Buffer.from(id))));
  await call(reportEndpoint + '?after=invalid', undefined, { ...adminGet, status: 400 });
  await call(`${reportEndpoint}/${Buffer.from(randomBytes(16)).toString('base64url')}`, undefined, { ...adminGet, status: 404 });
  await call('/v1/admin/blocks', { kind: 'account', id: first.account, blocked: true }, { admin: true });
  await call('/v1/accounts/ticket', {}, { secret: second.credential_secret, status: 403 });
  const blocked = await status();
  const { value: account } = await call(accountDetail, undefined, adminGet);
  assert(account.restricted && account.has_password);
  assert.equal(account.restriction_version, blocked.restriction_version);
  assert.deepEqual(account.recent_usage.find(entry => entry.day === day), { day, bytes: 12345 });
  const requestPage = version => nodeRequest('/v1/node/restrictions',
    'windshare/node-restrictions/v1\0', 'windshare/restriction-page/v1\0',
    { id: [...randomBytes(16)], node, issuer: batch.issuer, version, after: null });
  const page = await requestPage(blocked.restriction_version);
  assert.equal(page.kind, 'page');
  assert(page.entries.some(entry => entry.kind === 'account' && Buffer.from(entry.id).equals(Buffer.from(first.account))));
  assert(page.entries.some(entry => entry.kind === 'credential' && Buffer.from(entry.id).equals(Buffer.from(first.credential))));
  await call('/v1/admin/blocks', { kind: 'account', id: first.account, blocked: false }, { admin: true });
  assert.deepEqual(await requestPage(blocked.restriction_version),
    { kind: 'superseded', version: blocked.restriction_version + 1 });
  await call('/v1/accounts/ticket', {}, { secret: second.credential_secret });
  await call('/v1/accounts/ticket', {}, { secret, status: 403 });
  const shareBlock = blocked => call('/v1/admin/blocks', { kind: 'share', id: share, blocked }, { admin: true });
  for (const restricted of [true, false]) {
    const { value } = await shareBlock(restricted);
    const { entries } = await requestPage(value.version);
    assert.equal(entries.some(entry => entry.kind === 'share' && Buffer.from(entry.id).equals(Buffer.from(share))), restricted);
  }
  assert.equal((await request(new URL('/v1/blocks', url))).status, 404);
  const logout = await call('/v1/admin/logout', {}, { admin: true });
  assert.match(logout.response.headers.get('set-cookie'), /Max-Age=0/);
  const revokedSession = await call('/v1/admin/console', undefined, { admin: true, method: 'GET', status: 403 });
  assert.equal(revokedSession.value.reason, 'administrator_session');
  console.log(JSON.stringify({
    event: 'authority_worker_probe_passed', account: Buffer.from(first.account).toString('base64url'),
    node: Buffer.from(node).toString('base64url'), checks: [
      'one-time initialization', 'session cookies and CORS', 'idempotent registration', 'stable refresh identity',
      'password login', 'concurrent node policy updates', 'idempotent enrollment', 'signed node reports', 'idempotent UTC usage', 'independent revocation',
      'operator account, node and report reads', 'account block and restoration', 'node share restrictions',
    ],
  }));
});
