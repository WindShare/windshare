import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { test } from 'node:test';
import { identifier, turnFixture } from './turn-fixture.mjs';

const leaseRequest = service_id => ({
  service_id, lease_id: identifier(), session_id: identifier(), path_id: identifier(),
});
function latch() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}
const grant = () => Response.json({ iceServers: [{
  urls: ['turn:127.0.0.1:3478?transport=udp'], username: identifier(), credential: 'temporary-password',
}] }, { status: 201 });

test('deployment provisioning, device authority and lease ownership survive restart', { timeout: 60_000 }, async () => {
  const f = await turnFixture();
  try {
    const first = await f.admin('credentials', { name: 'before setup' });
    assert.equal((await f.device(first.access_key, 'capability')).body.turn, null);
    const ready = await f.provision();
    assert.equal(ready.status, 'ready', JSON.stringify(ready));
    const credential = await f.admin('credentials', { name: 'managed sender' });
    const other = await f.admin('credentials', { name: 'other sender' });
    const capability = (await f.device(credential.access_key, 'capability')).body.turn;
    assert.equal(capability.service_id, ready.serviceId);
    assert.equal(credential.configuration.managed_turn[0].service_id, ready.serviceId);
    assert.ok(!JSON.stringify(credential).includes('local-provider-secret'));
    assert.equal((await f.device(undefined, 'capability')).status, 403);
    const forged = createHash('sha256').update(credential.access_key).digest('base64url');
    assert.equal((await f.device(undefined, 'capability', undefined, { 'x-windshare-credential-digest': forged })).status, 403);
    const request = leaseRequest(ready.serviceId);
    const issued = await f.device(credential.access_key, 'issue', request);
    assert.equal(issued.status, 201, JSON.stringify(issued));
    assert.equal(issued.body.lease_id, request.lease_id);
    assert.ok(issued.body.expires_at > Date.now());
    assert.ok(issued.body.expires_at <= Date.now() + 3_600_000);
    assert.deepEqual(issued.body.urls, ['turn:127.0.0.1:3478?transport=udp']);
    assert.deepEqual((await f.device(credential.access_key, 'issue', request)).body, issued.body);
    assert.equal((await f.device(other.access_key, 'issue', request)).status, 403);
    assert.equal((await f.device(other.access_key, 'revoke', request)).status, 403);
    const renewal = { ...request, lease_id: identifier(), path_id: identifier(), previous_lease_id: request.lease_id };
    assert.equal((await f.device(credential.access_key, 'renew', { ...renewal, session_id: identifier() })).status, 409);
    assert.equal((await f.device(credential.access_key, 'renew', renewal)).status, 201);
    assert.equal((await f.device(credential.access_key, 'revoke', request)).status, 202);
    await f.restart();
    assert.deepEqual(await f.provision(), ready);
    assert.equal(f.keys.length, 1);
    await f.advance('alarm');
    assert.equal((await f.device(credential.access_key, 'revoke', request)).body.state, 'revoked');
    await f.admin('credentials/revoke', { credential_id: credential.credential.credential_id, terminate_shares: false });
    assert.equal((await f.device(credential.access_key, 'capability')).status, 403);
    assert.equal((await f.device(credential.access_key, 'issue', leaseRequest(ready.serviceId))).status, 403);
    assert.equal((await f.device(credential.access_key, 'renew', { ...renewal, lease_id: identifier() })).status, 403);
    await f.advance('alarm');
    const status = await f.admin('turn/status');
    assert.equal(status.leases.revoked, 2);
  } finally { await f.dispose(); }
});

test('disable during issuance never publishes credentials and durable revocation retries', { timeout: 60_000 }, async () => {
  const started = latch(), release = latch();
  let failRevocation = true, revocations = 0;
  const f = await turnFixture({ provider: async request => {
    if (request.url.endsWith('/revoke')) {
      revocations++;
      return new Response(null, { status: failRevocation ? 503 : 204 });
    }
    started.resolve();
    await release.promise;
    return grant();
  } });
  try {
    const ready = await f.provision();
    const credential = await f.admin('credentials', { name: 'racing sender' });
    const issuance = f.device(credential.access_key, 'issue', leaseRequest(ready.serviceId));
    await Promise.race([started.promise, issuance.then(result => { throw new Error('issuance ended before provider: ' + JSON.stringify(result)); })]);
    const disabled = await f.admin('turn/settings', { enabled: false });
    assert.equal(disabled.enabled, false);
    assert.equal(disabled.leases.revocation_pending, 1);
    release.resolve();
    assert.equal((await issuance).status, 403);
    assert.equal((await f.admin('turn/status')).leases.revocation_pending, 1);
    await f.advance('alarm');
    assert.equal(revocations, 1);
    assert.equal((await f.admin('turn/status')).leases.revocation_pending, 1);
    await f.restart();
    assert.equal((await f.admin('turn/status')).enabled, false);
    assert.equal((await f.device(credential.access_key, 'capability')).body.turn.enabled, false);
    failRevocation = false;
    await f.advance('turn-retry');
    await f.advance('alarm');
    assert.equal(revocations, 2);
    assert.equal((await f.admin('turn/status')).leases.revoked, 1);
    assert.equal((await f.device(credential.access_key, 'issue', leaseRequest(ready.serviceId))).status, 403);
  } finally { release.resolve(); await f.dispose(); }
});

test('failed renewal preserves the original lease and never delegates an invalid grant', { timeout: 60_000 }, async () => {
  let fail = false;
  const f = await turnFixture({ provider: async () => fail ? new Response(null, { status: 503 }) : grant() });
  try {
    const ready = await f.provision();
    const credential = await f.admin('credentials', { name: 'renewal sender' });
    const request = leaseRequest(ready.serviceId);
    const issued = await f.device(credential.access_key, 'issue', request);
    assert.equal(issued.status, 201);
    fail = true;
    assert.equal((await f.device(credential.access_key, 'renew', { ...request,
      lease_id: identifier(), path_id: identifier(), previous_lease_id: request.lease_id })).status, 503);
    assert.deepEqual((await f.device(credential.access_key, 'issue', request)).body, issued.body);
    const status = await f.admin('turn/status');
    assert.equal(status.leases.active, 1);
    assert.equal(status.leases.issuance_failed, 1);
    assert.equal(status.leases.expired, undefined);
  } finally { await f.dispose(); }
});
