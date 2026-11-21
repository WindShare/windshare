import assert from 'node:assert/strict';
import test from 'node:test';
import { deployRelay, readInputs, RELAY_BINDING, SECRET_BINDING, SHARE_BINDING, SHARE_CLASS, STATE_BINDING, ACCESS_BINDING, ACCESS_CLASS } from './deployment.mjs';

const inputs = { accountId: 'a'.repeat(32), workerName: 'private-relay', route: { kind: 'workers-dev' }, ownerPassword: 'owner-password-test-value' };
const namespaceId = 'b'.repeat(32), accessNamespaceId = 'c'.repeat(32);
function settingsFor(state, console = false) {
  return { bindings: [
    { name: STATE_BINDING, type: 'plain_text', text: JSON.stringify(state) },
    ...(console
      ? [{ name: RELAY_BINDING, type: 'service', service: inputs.workerName, environment: 'production' }]
      : [
          { name: SECRET_BINDING, type: 'secret_text' },
          { name: SHARE_BINDING, type: 'durable_object_namespace', class_name: SHARE_CLASS, namespace_id: namespaceId },
          { name: ACCESS_BINDING, type: 'durable_object_namespace', class_name: ACCESS_CLASS, namespace_id: accessNamespaceId },
        ]),
  ] };
}
function harness() {
  let remote = null, consoleRemote = null;
  const plans = [], events = [];
  const options = {
    inputs, newId: () => 'deployment-1',
    cloudflare: { subdomain: async () => 'owner', settings: async () => structuredClone(remote),
      route: async () => ({ enabled: true, previews_enabled: false }) },
    consoleCloudflare: { settings: async () => structuredClone(consoleRemote),
      route: async () => ({ enabled: true, previews_enabled: false }) },
    deploy: async plan => { plans.push(structuredClone(plan)); remote = settingsFor(plan.state); },
    deployConsole: async plan => { consoleRemote = settingsFor(plan.state, true); },
    log: (event, fields) => events.push({ event, ...fields }),
  };
  return { options, plans, events, get remote() { return remote; }, set remote(value) { remote = value; },
    get consoleRemote() { return consoleRemote; }, set consoleRemote(value) { consoleRemote = value; } };
}
test('initial deployment establishes separate origins with one authoritative access namespace', async () => {
  const h = harness(), result = await deployRelay(h.options);
  assert.equal(result.kind, 'create');
  assert.equal(result.state.endpoint, 'https://private-relay.owner.workers.dev');
  assert.equal(result.state.consoleEndpoint, 'https://private-relay-console.owner.workers.dev');
  assert.equal(result.namespaceId, namespaceId);
  assert.equal(result.accessNamespaceId, accessNamespaceId);
  assert.equal(h.consoleRemote.bindings[1].service, inputs.workerName);
  for (const value of [result, h.plans, h.events, h.remote, h.consoleRemote]) assert.ok(!JSON.stringify(value).includes(inputs.ownerPassword));
});
test('password rotation preserves deployment identity and both storage namespaces', async () => {
  const h = harness(), before = await deployRelay(h.options);
  h.options.inputs = { ...inputs, ownerPassword: 'rotated-owner-password' };
  const after = await deployRelay(h.options);
  assert.equal(after.kind, 'update');
  assert.deepEqual(after.state, before.state);
  assert.equal(after.namespaceId, before.namespaceId);
  assert.equal(after.accessNamespaceId, before.accessNamespaceId);
});
for (const stage of ['deploy', 'deployConsole']) {
  test('lost response after ' + stage + ' recovers the same authority on rerun', async () => {
    const h = harness(), publish = h.options[stage];
    h.options[stage] = async plan => { await publish(plan); throw new Error('lost response'); };
    await assert.rejects(deployRelay(h.options), /lost response/);
    const state = h.remote.bindings[0].text;
    h.options[stage] = publish;
    h.options.newId = () => assert.fail('must retain the committed identity');
    assert.equal(JSON.stringify((await deployRelay(h.options)).state), state);
  });
}
for (const [name, mutate] of [
  ['unmanaged Worker', h => { h.remote = { bindings: [] }; }],
  ['missing password', h => { h.remote.bindings.splice(1, 1); }],
  ['foreign share namespace', h => { h.remote.bindings[2].script_name = 'other'; }],
  ['foreign access namespace', h => { h.remote.bindings[3].script_name = 'other'; }],
  ['missing access namespace', h => { h.remote.bindings.pop(); }],
  ['unmanaged companion', h => { h.consoleRemote = { bindings: [] }; }],
  ['foreign companion relay service', h => { h.consoleRemote.bindings[1].service = 'other'; }],
  ['foreign companion relay environment', h => { h.consoleRemote.bindings[1].environment = 'staging'; }],
  ['changed public address', h => { h.options.cloudflare.subdomain = async () => 'other'; }],
]) {
  test(name + ' refuses updates before mutation', async () => {
    const h = harness(); await deployRelay(h.options); mutate(h);
    await assert.rejects(deployRelay(h.options)); assert.equal(h.plans.length, 1);
  });
}
test('companion name collisions are refused before creating the relay', async () => {
  const h = harness(); h.consoleRemote = { bindings: [] };
  await assert.rejects(deployRelay(h.options), /already in use/); assert.equal(h.plans.length, 0);
});
test('uncertain access and namespace drift never become a new deployment', async () => {
  const h = harness();
  h.options.cloudflare.settings = async () => { throw new Error('403'); };
  await assert.rejects(deployRelay(h.options), /403/); assert.equal(h.plans.length, 0);
  const drift = harness(); await deployRelay(drift.options);
  drift.options.deploy = async () => { drift.remote.bindings[3].namespace_id = 'd'.repeat(32); };
  await assert.rejects(deployRelay(drift.options), /namespace changed/);
});
test('input validation requires an owner secret and bounded stable names', () => {
  const env = { CLOUDFLARE_ACCOUNT_ID: inputs.accountId, CLOUDFLARE_WORKER_NAME: inputs.workerName, WINDSHARE_OWNER_PASSWORD: inputs.ownerPassword };
  assert.deepEqual(readInputs(env), inputs);
  for (const key of Object.keys(env)) assert.throws(() => readInputs({ ...env, [key]: '' }));
  assert.throws(() => readInputs({ ...env, CLOUDFLARE_WORKER_NAME: 'name; command' }));
  assert.throws(() => readInputs({ ...env, CLOUDFLARE_WORKER_NAME: 'a'.repeat(56) }));
});

test('post-deploy verification retries transient control-plane lag and logs retry reasons', async () => {
  const h = harness();
  await deployRelay(h.options);
  const waits = [];
  let postDeploySettingsCalls = 0, routeCalls = 0, consoleSettingsCalls = 0;
  const realSettings = h.options.cloudflare.settings;
  const realConsoleSettings = h.options.consoleCloudflare.settings;
  h.options.wait = async ms => waits.push(ms);
  h.options.verificationDelaysMs = [10, 20, 40];
  h.options.cloudflare.settings = async () => {
    if (h.plans.length === 2) {
      postDeploySettingsCalls++;
      if (postDeploySettingsCalls === 1) return { bindings: h.remote.bindings.slice(1) };
      if (postDeploySettingsCalls === 2) return null;
    }
    return realSettings();
  };
  h.options.cloudflare.route = async () => (++routeCalls === 1
    ? { enabled: false, previews_enabled: false }
    : { enabled: true, previews_enabled: false });
  h.options.consoleCloudflare.settings = async () => {
    if (h.plans.length === 2 && ++consoleSettingsCalls === 1) return { bindings: [] };
    return realConsoleSettings();
  };
  const updated = await deployRelay(h.options);
  assert.equal(updated.kind, 'update');
  assert.deepEqual(waits, [10, 20, 10, 10]);
  const retries = h.events.filter(event => event.event === 'deployment_verification_retry');
  assert.deepEqual(retries.map(({ attempt, reason }) => ({ attempt, reason })), [
    { attempt: 1, reason: 'missing_binding:WINDSHARE_DEPLOYMENT' },
    { attempt: 2, reason: 'worker_settings_absent' },
    { attempt: 1, reason: 'workers_dev_route_pending' },
    { attempt: 1, reason: 'missing_binding:WINDSHARE_DEPLOYMENT' },
  ]);
  assert.ok(!JSON.stringify(h.events).includes(inputs.ownerPassword));
});

test('post-deploy verification stops after bounded retries while failing fast on non-retryable drift', async () => {
  const exhausted = harness(), exhaustedWaits = [];
  await deployRelay(exhausted.options);
  exhausted.options.wait = async ms => exhaustedWaits.push(ms);
  exhausted.options.verificationDelaysMs = [10, 20];
  exhausted.options.deploy = async () => { exhausted.remote = { bindings: [] }; };
  await assert.rejects(deployRelay(exhausted.options), /missing its WINDSHARE_DEPLOYMENT plain_text binding/);
  assert.deepEqual(exhaustedWaits, [10, 20]);

  const drift = harness(), driftWaits = [];
  await deployRelay(drift.options);
  drift.options.wait = async ms => driftWaits.push(ms);
  drift.options.verificationDelaysMs = [10, 20];
  drift.options.deploy = async () => { drift.remote.bindings[3].namespace_id = 'd'.repeat(32); };
  await assert.rejects(deployRelay(drift.options), /namespace changed/);
  assert.deepEqual(driftWaits, []);
});
