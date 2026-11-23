import assert from 'node:assert/strict';
import test from 'node:test';
import { CloudflareRequestError, cloudflareClient } from './cloudflare.mjs';
import { managedTURNEnabled } from './configuration.mjs';
import { provisionTURN } from './turn.mjs';

test('managed TURN preserves installed keys and reconciles only deployment-owned orphans', async () => {
  const bindings = [], keys = [{ uid: 'other', name: 'another-worker' }], calls = [];
  const client = {
    settings: async () => ({ bindings }),
    turnKeys: async () => { calls.push('list'); return keys; },
    createTURNKey: async name => { const key = { uid: 'id', key: 'generated', name }; keys.push(key); calls.push('create'); return key; },
    deleteTURNKey: async id => { keys.splice(keys.findIndex(k => k.uid === id), 1); calls.push('delete'); },
    putSecret: async (name, text) => { assert.equal(JSON.parse(text).api_token, 'generated'); bindings.push({ name, type: 'secret_text' }); calls.push('put'); },
    deleteSecret: async () => { bindings.length = 0; calls.push('remove'); },
  };
  const run = enabled => provisionTURN({ client, enabled, workerName: 'test', deploymentId: '0123456789', log() {} });
  keys.push({ uid: 'orphan', name: 'windshare-test-01234567' });
  await run(managedTURNEnabled({}));
  assert.deepEqual(calls.splice(0), ['list','delete','create','put']);
  await run(true);
  assert.deepEqual(calls, []);
  await run(managedTURNEnabled({ CLOUDFLARE_TURN_ENABLED: 'false' }));
  assert.deepEqual(calls.splice(0), ['remove','list','delete']);
  keys.push({ uid: 'unbound', name: 'windshare-test-01234567' });
  await run(false);
  assert.deepEqual(calls.splice(0), ['list','delete']);
  assert.deepEqual(keys.map(key => key.uid), ['other']);
  client.turnKeys = async () => { throw new Error('private request body'); };
  const warning = await run(true);
  assert.match(warning, /Cloudflare Calls/);
  assert.doesNotMatch(warning, /private/);
});

test('an opted-out Worker without a TURN secret can deploy with no Calls permission', async () => {
  const calls = [], events = [];
  const client = cloudflareClient({
    accountId: 'account', workerName: 'worker', apiToken: 'deployment-token',
    fetchImpl: async (url, { method }) => {
      const path = new URL(url).pathname;
      calls.push([method, path]);
      if (path.endsWith('/settings')) return Response.json({ success: true, result: { bindings: [] } });
      return Response.json({ success: false, errors: [{ code: 10000 }] }, { status: 403 });
    },
  });
  const warning = await provisionTURN({ client, enabled: false, deploymentId: '0123456789', workerName: 'worker',
    log: (event, fields) => events.push([event, fields]) });
  assert.equal(warning, undefined);
  assert.deepEqual(events, [['turn_provisioning_skipped', { reason: 'disabled' }]]);
  assert.deepEqual(calls, [
    ['GET', '/client/v4/accounts/account/workers/scripts/worker/settings'],
    ['GET', '/client/v4/accounts/account/calls/turn_keys'],
  ]);
});

test('a known orphan that cannot be deleted reports incomplete cleanup', async () => {
  const deletes = [], events = [];
  const client = {
    settings: async () => ({ bindings: [] }),
    turnKeys: async () => [{ uid: 'owned', name: 'windshare-worker-01234567' }],
    deleteTURNKey: async id => {
      deletes.push(id);
      throw new CloudflareRequestError(403, '10000');
    },
  };
  const warning = await provisionTURN({ client, enabled: false, deploymentId: '0123456789', workerName: 'worker',
    log: (event, fields) => events.push([event, fields]) });
  assert.deepEqual(deletes, ['owned']);
  assert.match(warning, /cleanup is incomplete/);
  assert.match(warning, /HTTP 403/);
  assert.doesNotMatch(warning, /CLOUDFLARE_TURN_ENABLED/);
  assert.deepEqual(events, [['turn_provisioning_skipped', { reason: warning }]]);
});

test('unverified Worker bindings leave TURN keys untouched', async () => {
  for (const settings of [null, {}]) {
    for (const enabled of [true, false]) {
      let listed = false;
      const client = { settings: async () => settings, turnKeys: async () => { listed = true; return []; } };
      const warning = await provisionTURN({ client, enabled, deploymentId: '0123456789', workerName: 'worker', log() {} });
      assert.equal(listed, false);
      assert.match(warning, /bindings/);
    }
  }
});
