import assert from 'node:assert/strict';
import { test } from 'node:test';
import { identifier, turnFixture } from './turn-fixture.mjs';

test('analytics uses independent read-only authority only on explicit console demand', { timeout: 30_000 }, async () => {
  let calls = 0, fail = false;
  const f = await turnFixture({ analytics: async request => {
    calls++;
    assert.equal(request.headers.get('Authorization'), 'Bearer readonly-analytics');
    const { query } = await request.json();
    assert.match(query, /callsTurnUsageAdaptiveGroups/);
    assert.match(query, /keyId:"localkey0"/);
    return fail ? Response.json({ errors: [{ message: 'permission unavailable' }] }) :
      Response.json({ data: { viewer: { accounts: [{ callsTurnUsageAdaptiveGroups: [
        { sum: { ingressBytes: 321, egressBytes: 654 } },
      ] }] } } });
  } });
  try {
    assert.equal((await f.provision()).status, 'ready');
    assert.equal((await f.admin('turn/status')).analytics_available, true);
    const credential = await f.admin('credentials', { name: 'usage sender' });
    const cap = (await f.device(credential.access_key, 'capability')).body.turn;
    assert.equal((await f.device(credential.access_key, 'issue', { service_id: cap.service_id,
      lease_id: identifier(), session_id: identifier(), path_id: identifier() })).status, 201);
    assert.equal(calls, 0);
    const usage = await f.admin('turn/usage');
    assert.equal(usage.ingress_bytes, 321);
    assert.equal(usage.egress_bytes, 654);
    assert.equal(usage.sampled, true);
    assert.equal(Date.parse(usage.last_date) - Date.parse(usage.first_date), 6 * 86_400_000);
    fail = true;
    await assert.rejects(f.admin('turn/usage'), /503/);
    assert.equal((await f.admin('turn/status')).leases.active, 1);
    assert.equal((await f.device(credential.access_key, 'capability')).body.turn.enabled, true);
  } finally { await f.dispose(); }
});

test('issuance is rate-bounded per device and rejects malformed authority', { timeout: 30_000 }, async () => {
  const f = await turnFixture();
  try {
    const ready = await f.provision();
    const credential = await f.admin('credentials', { name: 'rate bounded sender' });
    const request = () => ({ service_id: ready.serviceId, lease_id: identifier(), session_id: identifier(), path_id: identifier() });
    assert.equal((await f.device(credential.access_key, 'issue', { ...request(), service_id: 'another-service' })).status, 400);
    assert.equal((await f.device(credential.access_key, 'issue', { ...request(), path_id: 'malformed' })).status, 400);
    for (let index = 0; index < 30; index++) {
      assert.equal((await f.device(credential.access_key, 'issue', request())).status, 201);
    }
    assert.equal((await f.device(credential.access_key, 'issue', request())).status, 429);
    assert.equal((await f.admin('turn/status')).leases.active, 30);
    assert.equal((await f.admin('turn/status')).analytics_available, false);
  } finally { await f.dispose(); }
});
