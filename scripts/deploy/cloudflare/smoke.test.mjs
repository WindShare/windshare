import assert from 'node:assert/strict';
import test from 'node:test';
import { CloudflareRequestError } from './cloudflare.mjs';
import { STATE_BINDING } from './deployment.mjs';
import { cleanupSmokeResources } from './smoke.mjs';
import { turnKeyName } from './turn.mjs';

const WORKER_NAME = 'windshare-actions-test-0123456789ab';

function makeHarness({
  workerSettings = { bindings: [] },
  consoleSettings = { bindings: [] },
  keys = [],
  workerSettingsError,
  turnKeysError,
  deleteKeyError,
} = {}) {
  const operations = [];
  const client = name => ({
    settings: async () => {
      operations.push(`settings:${name}`);
      if (name === WORKER_NAME) {
        if (workerSettingsError) throw workerSettingsError;
        return workerSettings;
      }
      return consoleSettings;
    },
    turnKeys: async () => {
      operations.push('turnKeys');
      if (turnKeysError) throw turnKeysError;
      return keys;
    },
    deleteTURNKey: async uid => {
      operations.push(`deleteKey:${uid}`);
      if (deleteKeyError) throw deleteKeyError;
    },
  });
  const deleteWorker = name => { operations.push(`deleteWorker:${name}`); };
  return { client, deleteWorker, operations };
}

test('smoke cleanup deletes the recorded TURN key before deleting companion and relay Workers', async () => {
  const ownedKeyName = turnKeyName(WORKER_NAME, 'abcdef01-2345-6789-abcd-ef0123456789');
  const { client, deleteWorker, operations } = makeHarness({
    keys: [
      { uid: 'owned', name: ownedKeyName },
      { uid: 'other', name: 'windshare-production-abcdef01' },
    ],
  });
  await cleanupSmokeResources({ workerName: WORKER_NAME, ownedKeyName, client, deleteWorker });
  assert.deepEqual(operations, [
    `settings:${WORKER_NAME}`,
    'turnKeys',
    'deleteKey:owned',
    `settings:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}`,
  ]);
});

test('smoke cleanup recovers exact TURN key name from Worker settings when deployment report was not written', async () => {
  const deploymentId = 'deadbeef-1111-2222-3333-444444444444';
  const expectedKey = turnKeyName(WORKER_NAME, deploymentId);
  const { client, deleteWorker, operations } = makeHarness({
    workerSettings: {
      bindings: [{ name: STATE_BINDING, type: 'plain_text', text: JSON.stringify({ deploymentId }) }],
    },
    keys: [
      { uid: 'owned', name: expectedKey },
      { uid: 'different-id', name: turnKeyName(WORKER_NAME, '99999999-aaaa') },
      { uid: 'other-worker', name: 'windshare-other-deadbeef' },
    ],
  });
  await cleanupSmokeResources({ workerName: WORKER_NAME, ownedKeyName: undefined, client, deleteWorker });
  assert.deepEqual(operations, [
    `settings:${WORKER_NAME}`,
    'turnKeys',
    'deleteKey:owned',
    `settings:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}`,
  ]);
});

test('smoke cleanup falls back to workerName prefix when Worker exists without readable deployment metadata', async () => {
  const { client, deleteWorker, operations } = makeHarness({
    workerSettings: { bindings: [{ name: STATE_BINDING, type: 'plain_text', text: '{invalid-json' }] },
    keys: [
      { uid: 'owned', name: turnKeyName(WORKER_NAME, 'feedface-0000') },
      { uid: 'other-smoke', name: 'windshare-windshare-actions-test-ffffffffffff-feedface' },
    ],
  });
  await cleanupSmokeResources({ workerName: WORKER_NAME, ownedKeyName: undefined, client, deleteWorker });
  assert.deepEqual(operations, [
    `settings:${WORKER_NAME}`,
    'turnKeys',
    'deleteKey:owned',
    `settings:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}`,
  ]);
});

test('smoke cleanup skips TURN API when deployment failed before creating the relay Worker', async () => {
  const { client, deleteWorker, operations } = makeHarness({
    workerSettings: null,
    consoleSettings: null,
  });
  await cleanupSmokeResources({ workerName: WORKER_NAME, ownedKeyName: undefined, client, deleteWorker });
  assert.deepEqual(operations, [
    `settings:${WORKER_NAME}`,
    `settings:${WORKER_NAME}-console`,
  ]);
});

test('smoke cleanup ignores HTTP 403 when listing TURN keys on accounts without Calls access', async () => {
  const ownedKeyName = turnKeyName(WORKER_NAME, '12345678-aaaa');
  const { client, deleteWorker, operations } = makeHarness({
    turnKeysError: new CloudflareRequestError(403, '10000'),
  });
  await cleanupSmokeResources({ workerName: WORKER_NAME, ownedKeyName, client, deleteWorker });
  assert.deepEqual(operations, [
    `settings:${WORKER_NAME}`,
    'turnKeys',
    `settings:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}`,
  ]);
});

test('smoke cleanup reports deletion errors and still cleans remaining resources when settings lookup fails', async () => {
  const settingsError = new CloudflareRequestError(503, '10000');
  const deleteError = new CloudflareRequestError(403, '10000');
  const { client, deleteWorker, operations } = makeHarness({
    workerSettingsError: settingsError,
    keys: [{ uid: 'owned', name: turnKeyName(WORKER_NAME, 'cafe1234-0000') }],
    deleteKeyError: deleteError,
  });
  await assert.rejects(
    cleanupSmokeResources({ workerName: WORKER_NAME, ownedKeyName: undefined, client, deleteWorker }),
    error => {
      assert.ok(error instanceof AggregateError);
      assert.deepEqual(error.errors, [settingsError, deleteError]);
      return true;
    },
  );
  assert.deepEqual(operations, [
    `settings:${WORKER_NAME}`,
    'turnKeys',
    'deleteKey:owned',
    `settings:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}-console`,
  ]);
});
