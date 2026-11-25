import assert from 'node:assert/strict';
import test from 'node:test';
import { CloudflareRequestError } from './cloudflare.mjs';
import { STATE_BINDING } from './deployment.mjs';
import { CLEANUP_LOOKUP_RETRY_DELAYS_MS, cleanupSmokeResources } from './smoke.mjs';
import { turnKeyName } from './turn.mjs';

const WORKER_NAME = 'windshare-actions-test-0123456789ab';

function resolveOutcome(outcome) {
  if (outcome instanceof Error) throw outcome;
  return outcome;
}

function makeHarness({
  workerSettings = { bindings: [] },
  consoleSettings = { bindings: [] },
  workerSettingsOutcomes,
  consoleSettingsOutcomes,
  keys = [],
  workerSettingsError,
  consoleSettingsError,
  turnKeysError,
  deleteKeyError,
} = {}) {
  const operations = [];
  const waits = [];
  const workerQueue = workerSettingsOutcomes ? [...workerSettingsOutcomes] : null;
  const consoleQueue = consoleSettingsOutcomes ? [...consoleSettingsOutcomes] : null;
  const nextOutcome = (queue, fallbackError, fallbackValue) => {
    if (queue) return resolveOutcome(queue.length > 1 ? queue.shift() : queue[0]);
    if (fallbackError) throw fallbackError;
    return fallbackValue;
  };
  const client = name => ({
    settings: async () => {
      operations.push(`settings:${name}`);
      if (name === WORKER_NAME) {
        return nextOutcome(workerQueue, workerSettingsError, workerSettings);
      }
      return nextOutcome(consoleQueue, consoleSettingsError, consoleSettings);
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
  const wait = async ms => { waits.push(ms); };
  return { client, deleteWorker, wait, operations, waits };
}

test('smoke cleanup deletes the recorded TURN key and both Workers directly from the deployment receipt', async () => {
  const deploymentId = 'abcdef01-2345-6789-abcd-ef0123456789';
  const ownedKeyName = turnKeyName(WORKER_NAME, deploymentId);
  const { client, deleteWorker, wait, operations, waits } = makeHarness({
    workerSettingsError: new CloudflareRequestError(503, '10000'),
    consoleSettingsError: new CloudflareRequestError(503, '10000'),
    keys: [
      { uid: 'owned', name: ownedKeyName },
      { uid: 'other', name: 'windshare-production-abcdef01' },
    ],
  });
  await cleanupSmokeResources({
    workerName: WORKER_NAME,
    deploymentReceipt: { deploymentId },
    client,
    deleteWorker,
    wait,
  });
  assert.deepEqual(waits, []);
  assert.deepEqual(operations, [
    'turnKeys',
    'deleteKey:owned',
    `deleteWorker:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}`,
  ]);
});

test('smoke cleanup still deletes both Workers from a deployment receipt missing deploymentId even if settings lookup fails', async () => {
  const settingsError = new CloudflareRequestError(503, '10000');
  const { client, deleteWorker, wait, operations, waits } = makeHarness({
    workerSettingsError: settingsError,
    keys: [{ uid: 'owned', name: turnKeyName(WORKER_NAME, 'cafe1234-0000') }],
  });
  await assert.rejects(
    cleanupSmokeResources({
      workerName: WORKER_NAME,
      deploymentReceipt: { deploymentId: undefined },
      client,
      deleteWorker,
      wait,
    }),
    error => {
      assert.ok(error instanceof AggregateError);
      assert.deepEqual(error.errors, [settingsError]);
      return true;
    },
  );
  assert.deepEqual(waits, [...CLEANUP_LOOKUP_RETRY_DELAYS_MS]);
  assert.deepEqual(operations, [
    `settings:${WORKER_NAME}`,
    `settings:${WORKER_NAME}`,
    `settings:${WORKER_NAME}`,
    `settings:${WORKER_NAME}`,
    'turnKeys',
    'deleteKey:owned',
    `deleteWorker:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}`,
  ]);
});

test('smoke cleanup recovers exact TURN key name from Worker settings when deployment report was not written', async () => {
  const deploymentId = 'deadbeef-1111-2222-3333-444444444444';
  const expectedKey = turnKeyName(WORKER_NAME, deploymentId);
  const { client, deleteWorker, wait, operations } = makeHarness({
    workerSettings: {
      bindings: [{ name: STATE_BINDING, type: 'plain_text', text: JSON.stringify({ deploymentId }) }],
    },
    keys: [
      { uid: 'owned', name: expectedKey },
      { uid: 'different-id', name: turnKeyName(WORKER_NAME, '99999999-aaaa') },
      { uid: 'other-worker', name: 'windshare-other-deadbeef' },
    ],
  });
  await cleanupSmokeResources({ workerName: WORKER_NAME, deploymentReceipt: null, client, deleteWorker, wait });
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
  const { client, deleteWorker, wait, operations } = makeHarness({
    workerSettings: { bindings: [{ name: STATE_BINDING, type: 'plain_text', text: '{invalid-json' }] },
    keys: [
      { uid: 'owned', name: turnKeyName(WORKER_NAME, 'feedface-0000') },
      { uid: 'other-smoke', name: 'windshare-windshare-actions-test-ffffffffffff-feedface' },
    ],
  });
  await cleanupSmokeResources({ workerName: WORKER_NAME, deploymentReceipt: null, client, deleteWorker, wait });
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
  const { client, deleteWorker, wait, operations } = makeHarness({
    workerSettings: null,
    consoleSettings: null,
  });
  await cleanupSmokeResources({ workerName: WORKER_NAME, deploymentReceipt: null, client, deleteWorker, wait });
  assert.deepEqual(operations, [
    `settings:${WORKER_NAME}`,
    `settings:${WORKER_NAME}-console`,
  ]);
});

test('smoke cleanup ignores HTTP 403 when listing TURN keys on accounts without Calls access', async () => {
  const deploymentId = '12345678-aaaa-bbbb-cccc-dddddddddddd';
  const { client, deleteWorker, wait, operations } = makeHarness({
    turnKeysError: new CloudflareRequestError(403, '10000'),
  });
  await cleanupSmokeResources({
    workerName: WORKER_NAME,
    deploymentReceipt: { deploymentId },
    client,
    deleteWorker,
    wait,
  });
  assert.deepEqual(operations, [
    'turnKeys',
    `deleteWorker:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}`,
  ]);
});

test('smoke cleanup retries transient settings lookup failures before deleting unconfirmed Workers', async () => {
  const deploymentId = 'feed1234-1111-2222-3333-444444444444';
  const { client, deleteWorker, wait, operations, waits } = makeHarness({
    workerSettingsOutcomes: [
      new CloudflareRequestError(503, '10000'),
      new Error('connection reset'),
      { bindings: [{ name: STATE_BINDING, type: 'plain_text', text: JSON.stringify({ deploymentId }) }] },
    ],
    consoleSettingsOutcomes: [
      new CloudflareRequestError(429, '10000'),
      { bindings: [] },
    ],
    keys: [{ uid: 'owned', name: turnKeyName(WORKER_NAME, deploymentId) }],
  });
  await cleanupSmokeResources({ workerName: WORKER_NAME, deploymentReceipt: null, client, deleteWorker, wait });
  assert.deepEqual(waits, [
    CLEANUP_LOOKUP_RETRY_DELAYS_MS[0],
    CLEANUP_LOOKUP_RETRY_DELAYS_MS[1],
    CLEANUP_LOOKUP_RETRY_DELAYS_MS[0],
  ]);
  assert.deepEqual(operations, [
    `settings:${WORKER_NAME}`,
    `settings:${WORKER_NAME}`,
    `settings:${WORKER_NAME}`,
    'turnKeys',
    'deleteKey:owned',
    `settings:${WORKER_NAME}-console`,
    `settings:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}`,
  ]);
});

test('smoke cleanup fails fast on non-retryable settings lookup errors without waiting', async () => {
  const authError = new CloudflareRequestError(403, '10000');
  const { client, deleteWorker, wait, operations, waits } = makeHarness({
    workerSettingsError: authError,
    keys: [{ uid: 'owned', name: turnKeyName(WORKER_NAME, 'beef0000-0000') }],
  });
  await assert.rejects(
    cleanupSmokeResources({ workerName: WORKER_NAME, deploymentReceipt: null, client, deleteWorker, wait }),
    error => {
      assert.ok(error instanceof AggregateError);
      assert.deepEqual(error.errors, [authError]);
      return true;
    },
  );
  assert.deepEqual(waits, []);
  assert.deepEqual(operations, [
    `settings:${WORKER_NAME}`,
    'turnKeys',
    'deleteKey:owned',
    `settings:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}-console`,
  ]);
});

test('smoke cleanup reports deletion errors and still cleans remaining resources when settings retries are exhausted', async () => {
  const settingsError = new CloudflareRequestError(503, '10000');
  const deleteError = new CloudflareRequestError(403, '10000');
  const { client, deleteWorker, wait, operations, waits } = makeHarness({
    workerSettingsError: settingsError,
    keys: [{ uid: 'owned', name: turnKeyName(WORKER_NAME, 'cafe1234-0000') }],
    deleteKeyError: deleteError,
  });
  await assert.rejects(
    cleanupSmokeResources({ workerName: WORKER_NAME, deploymentReceipt: null, client, deleteWorker, wait }),
    error => {
      assert.ok(error instanceof AggregateError);
      assert.deepEqual(error.errors, [settingsError, deleteError]);
      return true;
    },
  );
  assert.deepEqual(waits, [...CLEANUP_LOOKUP_RETRY_DELAYS_MS]);
  assert.deepEqual(operations, [
    `settings:${WORKER_NAME}`,
    `settings:${WORKER_NAME}`,
    `settings:${WORKER_NAME}`,
    `settings:${WORKER_NAME}`,
    'turnKeys',
    'deleteKey:owned',
    `settings:${WORKER_NAME}-console`,
    `deleteWorker:${WORKER_NAME}-console`,
  ]);
});
