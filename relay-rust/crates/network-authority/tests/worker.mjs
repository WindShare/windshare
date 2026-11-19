import assert from 'node:assert/strict';
import { createPrivateKey, createPublicKey, randomBytes, sign, verify } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';

// This probe mutates only a loopback Worker with disposable credentials. It
// exercises the real JS/Wasm/SQLite boundary that native Rust tests cannot cover.
const [base, trustFile, initialization] = process.argv.slice(2);
const url = new URL(base);
assert(['127.0.0.1', 'localhost', '[::1]'].includes(url.hostname), 'Use an isolated local Worker');
const trust = JSON.parse(await readFile(trustFile, 'utf8'));
const administrator = { username: 'local-operator', password: 'Local-P2-validation-2026' };
let cookie = '';
let csrf = '';
async function call(path, body, options = {}) {
  const headers = { Origin: url.origin, ...options.headers };
  if (!Buffer.isBuffer(body)) headers['Content-Type'] = 'application/json';
  if (options.admin) Object.assign(headers, { Cookie: cookie, 'X-CSRF-Token': csrf });
  if (options.secret) headers.Authorization = 'Bearer ' + options.secret;
  Object.assign(headers, options.headers);
  const response = await fetch(new URL(path, url), {
    method: options.method ?? 'POST', headers,
    body: body === undefined ? undefined : Buffer.isBuffer(body) ? body : JSON.stringify(body),
    signal: AbortSignal.timeout(20_000),
  });
  const publicPolicy = path === '/v1/blocks' && [200, 304].includes(response.status);
  assert.equal(response.headers.get('cache-control'), publicPolicy ? 'private, no-cache' : 'no-store');
  assert.equal(response.headers.get('access-control-allow-origin'), url.origin);
  const value = response.status === 304 ? await response.text() : await response.json();
  assert.equal(response.status, options.status ?? 200, JSON.stringify(value));
  return { response, value };
}
const initialized = await fetch(new URL('/v1/admin/initialize', url), {
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
const missingCookie = await call('/v1/admin/logout', {}, {
  admin: true, headers: { Cookie: '' }, status: 403,
});
assert.equal(missingCookie.value.reason, 'administrator_session');
const consoleState = (await call('/v1/admin/console', undefined, { admin: true, method: 'GET' })).value;
await call('/v1/admin/settings', { registration: 'automatic' }, { admin: true, method: 'PATCH' });
const secret = randomBytes(32).toString('base64url');
const first = (await call('/v1/accounts', { client_secret: secret })).value;
const repeated = (await call('/v1/accounts', { client_secret: secret })).value;
assert.deepEqual(repeated.account, first.account);
assert.deepEqual(repeated.credential, first.credential);
const refreshed = (await call('/v1/accounts/ticket', {}, { secret })).value;
assert.deepEqual(refreshed.credential, first.credential);
const username = 'probe-' + randomBytes(6).toString('hex');
await call('/v1/accounts/password', { username, password: administrator.password }, { secret });
const second = (await call('/v1/accounts/login', { username, password: administrator.password })).value;
assert.deepEqual(second.account, first.account);
assert.notDeepEqual(second.credential, first.credential);
const privateKey = createPrivateKey({ key: Buffer.concat([
  Buffer.from('302e020100300506032b657004220420', 'hex'), randomBytes(32),
]), type: 'pkcs8', format: 'der' });
const node = [...createPublicKey(privateKey).export({ type: 'spki', format: 'der' }).subarray(-32)];
const enrollment = { key: node, zone: 'cn', region: 'local-proof' };
const enrolled = (await call('/v1/admin/nodes', enrollment, { admin: true })).value;
assert(enrolled.enabled && !enrolled.anonymous && enrolled.version === 1);
// Independent operator sessions can edit these fields against the same saved snapshot.
const update = (field, value) => call('/v1/admin/nodes/policy', { key: node, change: { field, value } }, { admin: true, method: 'PUT' });
await Promise.all([update('public_relay', false), update('anonymous_sharing', true)]);
const existing = (await call('/v1/admin/nodes', { ...enrollment, zone: 'global' }, { admin: true })).value;
assert.equal(existing.zone, 'cn');
assert(!existing.enabled && existing.anonymous && existing.version === 3);
const batch = {
  id: [...randomBytes(16)], node, issuer: [...Buffer.from(trust.root, 'base64url')],
  created_at: Math.floor(Date.now() / 1000), records: [], resident_accounts: [first.account], shares: [],
  load: { active_sessions: 0, wire_bytes: 0, forwarded_bytes: 0, admission_refusals: 0 },
};
const onlineSeed = (await readFile(join(dirname(trustFile), 'online-1.seed'), 'utf8')).trim();
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
const nodesEndpoint = '/v1/admin/nodes';
const encodedNodeKey = Buffer.from(node).toString('base64url');
const nodeDetail = nodesEndpoint + '/' + encodedNodeKey;
await call(nodeDetail, undefined, { method: 'GET', status: 403 });
const { value: inspected } = await call(nodeDetail, undefined, adminGet);
assert.deepEqual(inspected.key, node);
assert.equal(inspected.version, existing.version);
assert.deepEqual(inspected.report.load, batch.load);
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
await call('/v1/admin/blocks', { kind: 'share', id: share, blocked: true }, { admin: true });
const publicBlocks = await call('/v1/blocks', undefined, { method: 'GET' });
assert(publicBlocks.value.body && publicBlocks.value.certificate);
const blockTag = publicBlocks.response.headers.get('etag');
assert(blockTag);
const revalidation = { method: 'GET', headers: { 'If-None-Match': blockTag } };
const unchanged = await call('/v1/blocks', undefined, { ...revalidation, status: 304 });
assert.equal(unchanged.value, '');
assert.equal(unchanged.response.headers.get('etag'), blockTag);
await call('/v1/admin/blocks', { kind: 'share', id: share, blocked: false }, { admin: true });
const updatedBlocks = await call('/v1/blocks', undefined, revalidation);
assert.notEqual(updatedBlocks.response.headers.get('etag'), blockTag);
assert.notEqual(updatedBlocks.value.body, publicBlocks.value.body);
const logout = await call('/v1/admin/logout', {}, { admin: true });
assert.match(logout.response.headers.get('set-cookie'), /Max-Age=0/);
const revokedSession = await call('/v1/admin/console', undefined, { admin: true, method: 'GET', status: 403 });
assert.equal(revokedSession.value.reason, 'administrator_session');
console.log(JSON.stringify({
  event: 'authority_worker_probe_passed', account: Buffer.from(first.account).toString('base64url'),
  node: Buffer.from(node).toString('base64url'), checks: [
    'one-time initialization', 'session cookies and CORS', 'idempotent registration', 'stable refresh identity',
    'password login', 'concurrent node policy updates', 'idempotent enrollment', 'signed node reports', 'idempotent UTC usage', 'independent revocation',
    'operator node and report reads', 'account block and restoration', 'public share restrictions',
  ],
}));
