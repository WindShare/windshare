import assert from 'node:assert/strict';
import { test } from 'node:test';
import { identifier, turnFixture } from './turn-fixture.mjs';

const LIFETIME_MS = 3_600_000;
const HISTORY_COUNT = 1001;
const DEPLOYMENT_LIMIT = 1000;
const DEVICE_LIMIT = 32;
const REVOCATION_BATCH = 16;
const requestFor = service_id => ({
  service_id, lease_id: identifier(), session_id: identifier(), path_id: identifier(),
});
const grant = username => Response.json({ iceServers: [{
  urls: ['turn:127.0.0.1:3478?transport=udp'], username, credential: 'temporary-password',
}] }, { status: 201 });
function latch() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}
async function records(f, leases) {
  const response = await (await f.authority()).fetch('https://internal/__test/turn-leases', {
    method: leases ? 'POST' : 'GET',
    body: leases ? JSON.stringify(leases) : undefined,
  });
  assert.equal(response.status, 200);
  return response.json();
}
function storedLease(serviceId, device, id, state) {
  return {
    id, service_id: serviceId, key_id: 'localkey0',
    subject: { device, session: identifier(), path: identifier() },
    usable_until: Date.now() + LIFETIME_MS,
    expires_at: Date.now() + LIFETIME_MS,
    state,
  };
}
const active = username => ({
  state: 'active',
  grant: { urls: ['turn:127.0.0.1:3478?transport=udp'], username, password: 'temporary-password' },
});
const history = (serviceId, device) => Array.from({ length: HISTORY_COUNT }, (_, index) =>
  storedLease(serviceId, device, `history-${String(index).padStart(4, '0')}`, { state: 'revoked' }));

test('revocation during another provider call survives cleanup and is retried', { timeout: 60_000 }, async () => {
  const started = latch(), release = latch();
  const revoked = [];
  let issued = 0;
  const f = await turnFixture({ provider: async request => {
    if (!request.url.endsWith('/revoke')) return grant('credential-' + ++issued);
    const username = decodeURIComponent(new URL(request.url).pathname.split('/').at(-2));
    revoked.push(username);
    if (revoked.length === 1) {
      started.resolve();
      await release.promise;
    }
    return new Response(null, { status: 204 });
  } });
  try {
    const ready = await f.provision();
    const owner = await f.admin('credentials', { name: 'concurrent revocations' });
    const requests = [requestFor(ready.serviceId), requestFor(ready.serviceId)]
      .sort((a, b) => a.lease_id < b.lease_id ? -1 : 1);
    for (const request of requests) assert.equal((await f.device(owner.access_key, 'issue', request)).status, 201);
    assert.equal((await f.device(owner.access_key, 'revoke', requests[0])).status, 202);
    const cleanup = f.advance('alarm');
    await Promise.race([started.promise, cleanup.then(() => { throw new Error('cleanup skipped provider'); })]);
    assert.equal((await f.device(owner.access_key, 'revoke', requests[1])).status, 202);
    release.resolve();
    await cleanup;
    const after = await records(f);
    assert.ok(after.every(lease => lease.state.state !== 'active'), JSON.stringify(after));
    await f.advance('alarm');
    assert.deepEqual(revoked.sort(), ['credential-1', 'credential-2']);
    assert.equal((await f.admin('turn/status')).leases.revoked, 2);
  } finally { release.resolve(); await f.dispose(); }
});

test('retained history does not consume capacity or hide live leases and cleanup', { timeout: 60_000 }, async () => {
  const revoked = [];
  const f = await turnFixture({ provider: async request => {
    if (!request.url.endsWith('/revoke')) return grant(identifier());
    revoked.push(decodeURIComponent(new URL(request.url).pathname.split('/').at(-2)));
    return new Response(null, { status: 204 });
  } });
  try {
    const ready = await f.provision();
    const owner = await f.admin('credentials', { name: 'history capacity' });
    const device = owner.credential.credential_id;
    await records(f, history(ready.serviceId, device));
    const request = requestFor(ready.serviceId);
    assert.equal((await f.device(owner.access_key, 'issue', request)).status, 201);
    assert.equal((await f.admin('turn/status')).leases.revoked, HISTORY_COUNT);
    await f.device(owner.access_key, 'revoke', request);
    await f.advance('alarm');
    assert.equal((await f.device(owner.access_key, 'issue', request)).status, 409);

    // Sort after every retained record to expose truncated-list admission and cleanup.
    const live = Array.from({ length: DEVICE_LIMIT }, (_, index) =>
      storedLease(ready.serviceId, device, `zz-live-${index}`, active('live-' + index)));
    await records(f, live);
    assert.equal((await f.device(owner.access_key, 'issue', requestFor(ready.serviceId))).status, 429);
    await f.admin('turn/settings', { enabled: false });
    assert.equal((await f.admin('turn/status')).leases.revocation_pending, DEVICE_LIMIT);
    for (let batch = 0; batch < DEVICE_LIMIT / REVOCATION_BATCH; batch++) await f.advance('alarm');
    assert.equal((await f.admin('turn/status')).leases.revoked, HISTORY_COUNT + DEVICE_LIMIT + 1);
    assert.equal(revoked.length, DEVICE_LIMIT + 1);
  } finally { await f.dispose(); }
});

test('cleanup cannot overwrite issuance completed while another revocation is in flight', { timeout: 60_000 }, async () => {
  const issuing = latch(), completeIssue = latch(), revoking = latch(), completeRevoke = latch();
  let issued = 0;
  const f = await turnFixture({ provider: async request => {
    if (request.url.endsWith('/revoke')) {
      revoking.resolve();
      await completeRevoke.promise;
      return new Response(null, { status: 204 });
    }
    if (++issued === 2) {
      issuing.resolve();
      await completeIssue.promise;
    }
    return grant('issued-' + issued);
  } });
  try {
    const ready = await f.provision();
    const owner = await f.admin('credentials', { name: 'issuance during cleanup' });
    const requests = [requestFor(ready.serviceId), requestFor(ready.serviceId)]
      .sort((a, b) => a.lease_id < b.lease_id ? -1 : 1);
    assert.equal((await f.device(owner.access_key, 'issue', requests[0])).status, 201);
    const issuance = f.device(owner.access_key, 'issue', requests[1]);
    await Promise.race([issuing.promise, issuance.then(() => { throw new Error('issuance skipped provider'); })]);
    await f.device(owner.access_key, 'revoke', requests[0]);
    const cleanup = f.advance('alarm');
    await Promise.race([revoking.promise, cleanup.then(() => { throw new Error('cleanup skipped provider'); })]);
    completeIssue.resolve();
    const result = await issuance;
    assert.equal(result.status, 201);
    completeRevoke.resolve();
    await cleanup;
    const replay = await f.device(owner.access_key, 'issue', requests[1]);
    assert.equal(replay.status, 200);
    assert.deepEqual(replay.body, result.body);
    assert.equal(issued, 2);
  } finally { completeIssue.resolve(); completeRevoke.resolve(); await f.dispose(); }
});

test('capacity follows every lease state and its deadline without waiting for cleanup', { timeout: 60_000 }, async () => {
  const f = await turnFixture();
  try {
    const ready = await f.provision();
    const owner = await f.admin('credentials', { name: 'capacity semantics' });
    const device = owner.credential.credential_id;
    const now = Date.now();
    const cases = [
      [active('active'), false, 429],
      [{ ...active('pending'), state: 'revocation_pending', next_attempt: now }, false, 429],
      [{ state: 'issuing', deadline: now + LIFETIME_MS }, false, 429],
      [{ state: 'withdrawing', deadline: now + LIFETIME_MS }, false, 429],
      [{ state: 'revoked' }, false, 201],
      [{ state: 'issuance_failed' }, false, 201],
      [{ state: 'expired' }, false, 201],
      [active('expired-active'), true, 201],
      [{ ...active('expired-pending'), state: 'revocation_pending', next_attempt: now }, true, 201],
      [{ state: 'issuing', deadline: now - 1 }, false, 201],
      [{ state: 'withdrawing', deadline: now - 1 }, false, 201],
    ];
    for (const [state, expired, status] of cases) {
      const leases = Array.from({ length: DEVICE_LIMIT }, (_, index) => ({
        ...storedLease(ready.serviceId, device, 'capacity-' + index, state),
        ...(expired ? { expires_at: now - 1, usable_until: now - 1 } : {}),
      }));
      await records(f, leases);
      const response = await f.device(owner.access_key, 'issue', requestFor(ready.serviceId));
      assert.equal(response.status, status, JSON.stringify({ state, expired, response }));
    }
  } finally { await f.dispose(); }
});

test('deployment admission counts reservations and pending revocations beyond history', { timeout: 60_000 }, async () => {
  const f = await turnFixture();
  try {
    const ready = await f.provision();
    const owner = await f.admin('credentials', { name: 'deployment capacity' });
    const device = owner.credential.credential_id;
    const pending = { ...active('pending'), state: 'revocation_pending', next_attempt: Date.now() };
    const liveStates = [
      active('active'), pending,
      { state: 'issuing', deadline: Date.now() + 30_000 },
      { state: 'withdrawing', deadline: Date.now() + 30_000 },
    ];
    const live = Array.from({ length: DEPLOYMENT_LIMIT }, (_, index) =>
      storedLease(ready.serviceId, 'other-device', `zz-live-${index}`, liveStates[index % liveStates.length]));
    await records(f, [...history(ready.serviceId, device), ...live]);
    assert.equal((await f.device(owner.access_key, 'issue', requestFor(ready.serviceId))).status, 429);
    await records(f, [{ ...live[0], state: { state: 'revoked' } }]);
    assert.equal((await f.device(owner.access_key, 'issue', requestFor(ready.serviceId))).status, 201);
  } finally { await f.dispose(); }
});
