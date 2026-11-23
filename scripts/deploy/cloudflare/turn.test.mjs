import assert from 'node:assert/strict';
import test from 'node:test';
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
  assert.equal(keys[0].uid, 'other');
  client.turnKeys = async () => { throw new Error('private request body'); };
  const warning = await run(true);
  assert.match(warning, /Cloudflare Calls/);
  assert.doesNotMatch(warning, /private/);
});
