import assert from 'node:assert/strict';
import test from 'node:test';
import { provisionTURN, TURN_SECRET, turnConsoleClient } from './turn.mjs';

function harness(fault) {
  let state, secret, sequence = 0, keySequence = 0;
  const keys = [], deleted = [], events = [];
  const snapshot = () => structuredClone({ service_id: 'cf-deployment', provisioning: state,
    key_name: state ? 'windshare:cf-deployment:' + state.attempt : null, secret_key_id: secret?.key_id ?? null });
  const interrupt = stage => { if (fault === stage) { fault = undefined; throw new Error('interrupted ' + stage); } };
  const options = {
    log: (event, fields) => events.push({ event, ...fields }),
    consoleClient: { async call(path, body) {
      assert.equal(path, 'turn/provision');
      if (body.action === 'begin' && !state) state = { phase: 'intent', attempt: 'attempt' + ++sequence };
      else if (body.action === 'created') {
        assert.equal(body.attempt, state.attempt);
        assert.ok(state.phase === 'intent' || state.phase === 'created');
        state = { ...state, phase: 'created', key_id: body.key_id };
      } else if (body.action === 'publish') {
        assert.equal(state.key_id, secret?.key_id);
        assert.ok(state.phase === 'created' || state.phase === 'published');
        state.phase = 'published';
      } else if (body.action === 'abandon') {
        assert.equal(secret, undefined);
        assert.notEqual(state.phase, 'published');
        state.phase = 'abandoning';
      } else if (body.action === 'replace') {
        assert.equal(state.phase, 'abandoning');
        assert.equal(secret, undefined);
        state = { phase: 'intent', attempt: 'attempt' + ++sequence };
      }
      interrupt(body.action);
      return snapshot();
    } },
    cloudflare: {
      async turnKeys() { interrupt('list'); return structuredClone(keys); },
      async createTURNKey(name) {
        assert.equal(state.phase, 'intent', 'intent must precede creation');
        const key = { name, uid: 'key' + ++keySequence };
        keys.push(key); interrupt('create');
        return { ...key, secret: 'secret-value-never-log' };
      },
      async putSecret(name, text) {
        assert.equal(name, TURN_SECRET);
        assert.equal(state.phase, 'created');
        secret = JSON.parse(text); interrupt('secret');
      },
      async deleteTURNKey(id) {
        assert.equal(state.phase, 'abandoning');
        assert.equal(id, state.key_id);
        keys.splice(keys.findIndex(key => key.uid === id), 1); deleted.push(id); interrupt('delete');
      },
      async deleteSecret() { secret = undefined; },
    },
  };
  return { options, keys, deleted, events, snapshot,
    loseSecret() { secret = undefined; },
    setFault(value) { fault = value; } };
}
for (const fault of ['begin', 'list', 'create', 'created', 'secret', 'publish']) {
  test('interrupted TURN ' + fault + ' recovers without replacing a published key', async () => {
    const h = harness(fault);
    assert.equal((await provisionTURN(h.options)).status, 'incomplete');
    const result = await provisionTURN(h.options);
    assert.equal(result.status, 'ready', result.reason);
    assert.equal(h.keys.length, 1);
    assert.equal(h.snapshot().secret_key_id, result.keyId);
    const again = await provisionTURN(h.options);
    assert.deepEqual(again, result);
    assert.ok(!JSON.stringify([h.events, result]).includes('secret-value-never-log'));
  });
}
for (const phase of ['abandon', 'delete', 'replace']) {
  test('recovery resumes after interrupted ' + phase, async () => {
    const h = harness('create');
    await provisionTURN(h.options);
    h.setFault(phase);
    assert.equal((await provisionTURN(h.options)).status, 'incomplete');
    assert.equal((await provisionTURN(h.options)).status, 'ready');
    assert.equal(h.keys.length, 1);
  });
}
test('published secret loss and ambiguous ownership never delete working or foreign keys', async () => {
  const h = harness();
  await provisionTURN(h.options);
  h.loseSecret();
  assert.equal((await provisionTURN(h.options)).status, 'incomplete');
  assert.equal(h.deleted.length, 0);
  const duplicate = harness('create');
  await provisionTURN(duplicate.options);
  duplicate.keys.push({ ...duplicate.keys[0], uid: 'foreign' });
  assert.equal((await provisionTURN(duplicate.options)).status, 'incomplete');
  assert.equal(duplicate.deleted.length, 0);
});
test('missing Calls permission reports incomplete without throwing through deployment', async () => {
  const h = harness();
  h.options.cloudflare.createTURNKey = async () => { throw new Error('Calls Write required'); };
  const result = await provisionTURN(h.options);
  assert.equal(result.status, 'incomplete');
  assert.match(result.reason, /Calls Write/);
  assert.equal(h.snapshot().provisioning.phase, 'intent');
});
test('console provisioning uses an origin-bound owner session and refuses redirects', async () => {
  const calls = [];
  const client = turnConsoleClient({ origin: 'https://console.example', password: 'owner-secret',
    fetchImpl: async (url, request) => {
      calls.push([url, request]);
      return Response.json({}, { headers: url.endsWith('/login') ? { 'set-cookie': 'session=value; Secure' } : {} });
    } });
  await client.call('turn/provision', { action: 'begin' });
  assert.equal(calls.length, 2);
  assert.equal(calls[1][1].headers.Cookie, 'session=value');
  assert.equal(calls[1][1].headers.Origin, 'https://console.example');
  assert.equal(calls[1][1].redirect, 'error');
  assert.ok(!calls[1][1].body.includes('owner-secret'));
});
