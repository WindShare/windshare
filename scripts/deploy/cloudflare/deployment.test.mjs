import assert from 'node:assert/strict';
import test from 'node:test';
import { deploymentConfig, FULL_DEPLOYMENT, RELAY_ONLY_DEPLOYMENT } from './configuration.mjs';
import { deployRelay, readInputs, SECRET_BINDING, SHARE_BINDING, SHARE_CLASS, STATE_BINDING } from './deployment.mjs';
import { endpointFor } from './routing.mjs';

const inputs = {
  accountId: 'a'.repeat(32), workerName: 'private-relay', route: { kind: 'workers-dev' },
  recipient: 'age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p',
};
const credential = 'test-registration-authority';
const encryptedConfig = '-----BEGIN AGE ENCRYPTED FILE-----\nopaque-ciphertext\n-----END AGE ENCRYPTED FILE-----\n';
const namespaceId = 'b'.repeat(32);

function settingsFor(state) {
  return { bindings: [
    { name: STATE_BINDING, type: 'plain_text', text: JSON.stringify(state) },
    { name: SECRET_BINDING, type: 'secret_text' },
    { name: SHARE_BINDING, type: 'durable_object_namespace', class_name: SHARE_CLASS, namespace_id: namespaceId },
  ] };
}

function harness() {
  let remote = null;
  const plans = [], events = [], encrypted = [];
  const cloudflare = {
    subdomain: async () => 'owner',
    settings: async () => structuredClone(remote),
    route: async () => ({ enabled: true, previews_enabled: false }),
  };
  const options = {
    inputs, cloudflare,
    newToken: () => credential, newId: () => 'deployment-1',
    encrypt: async (plaintext, recipient) => {
      encrypted.push({ plaintext, recipient });
      return encryptedConfig;
    },
    deploy: async plan => {
      plans.push(structuredClone(plan));
      if (plan.kind === 'create') remote = settingsFor(plan.state);
    },
    log: (event, fields) => events.push({ event, ...fields }),
  };
  return { options, plans, events, encrypted,
    get remote() { return remote; },
    set remote(value) { remote = value; },
  };
}

test('first deploy seals an importable private-only candidate and commits it with the credential', async () => {
  const h = harness();
  const result = await deployRelay(h.options);
  assert.equal(result.kind, 'create');
  assert.equal(result.namespaceId, namespaceId);
  assert.deepEqual(JSON.parse(h.encrypted[0].plaintext), {
    relays: [{ url: 'https://private-relay.owner.workers.dev', registration_token: credential }],
  });
  assert.equal(h.encrypted[0].recipient, inputs.recipient);
  assert.equal(h.plans[0].credential, credential);
  assert.equal(result.state.encryptedConfig, encryptedConfig);
  assert.ok(!JSON.stringify(result).includes(credential));
  assert.ok(!JSON.stringify(h.events).includes(credential));
  assert.deepEqual(h.events.map(item => item.event), ['deployment_prepared', 'deployment_verified']);
});

test('updates need no readable secret and retain the address, encrypted export and namespace', async () => {
  const h = harness();
  const first = await deployRelay(h.options);
  const before = structuredClone(h.remote);
  h.options.newToken = h.options.encrypt = () => assert.fail('updates must not generate or reseal credentials');
  const second = await deployRelay(h.options);
  assert.equal(second.kind, 'update');
  assert.deepEqual(second.state, first.state);
  assert.equal(second.namespaceId, first.namespaceId);
  assert.deepEqual(h.remote, before);
  assert.ok(!Object.hasOwn(h.plans[1], 'credential'));
});

test('switching frontend hosting in either direction preserves deployment identity and relay authority', async () => {
  const scenario = harness();
  const publish = scenario.options.deploy;
  let mode = FULL_DEPLOYMENT;
  const configurations = [];
  scenario.options.deploy = async plan => {
    configurations.push(deploymentConfig({}, scenario.options.inputs, mode));
    await publish(plan);
  };
  const first = await deployRelay(scenario.options);
  scenario.options.newToken = scenario.options.encrypt = () => assert.fail('hosting changes must retain credentials');
  for (const nextMode of [RELAY_ONLY_DEPLOYMENT, FULL_DEPLOYMENT]) {
    mode = nextMode;
    const next = await deployRelay(scenario.options);
    assert.equal(next.kind, 'update');
    assert.deepEqual(next.state, first.state);
    assert.equal(next.namespaceId, first.namespaceId);
  }
  assert.deepEqual(configurations.map(config => Object.hasOwn(config, 'assets')), [true, false, true]);
  assert.equal(Object.hasOwn(first.state, 'mode'), false);
  assert.equal(scenario.encrypted.length, 1);
});

test('lost first-deployment response recovers the same configuration on rerun', async () => {
  const h = harness();
  const publish = h.options.deploy;
  h.options.deploy = async plan => { await publish(plan); throw new Error('lost upload response'); };
  await assert.rejects(deployRelay(h.options), /lost upload response/);
  const committed = h.remote.bindings[0].text;
  h.options.deploy = publish;
  h.options.newToken = h.options.encrypt = () => assert.fail('committed credential must survive a failed job');
  const recovered = await deployRelay(h.options);
  assert.equal(recovered.kind, 'update');
  assert.equal(JSON.stringify(recovered.state), committed);
});

test('uncertain preflight does not treat failed authorization as an empty account', async () => {
  const h = harness();
  h.options.cloudflare.settings = async () => { throw new Error('403'); };
  await assert.rejects(deployRelay(h.options), /403/);
  assert.equal(h.plans.length, 0);
  assert.equal(h.encrypted.length, 0);
});

for (const [name, mutate, message] of [
  ['unmanaged Worker', h => { h.remote = { bindings: [] }; }, /WINDSHARE_DEPLOYMENT/],
  ['missing secret', h => { h.remote.bindings.splice(1, 1); }, /REGISTRATION_SECRET/],
  ['plaintext secret', h => { h.remote.bindings[1].type = 'plain_text'; }, /REGISTRATION_SECRET/],
  ['duplicate state', h => { h.remote.bindings.push(h.remote.bindings[0]); }, /WINDSHARE_DEPLOYMENT/],
  ['corrupt state', h => { h.remote.bindings[0].text = '{}'; }, /Unsupported deployment/],
  ['malformed state', h => { h.remote.bindings[0].text = '{'; }, /Invalid deployment/],
  ['changed recipient', h => { h.options.inputs = { ...inputs, recipient: 'age1' + 'a'.repeat(58) }; }, /recipient changed/],
  ['renamed workers.dev subdomain', h => { h.options.cloudflare.subdomain = async () => 'different'; }, /address changed/],
  ['different account', h => { h.options.inputs = { ...inputs, accountId: 'c'.repeat(32) }; }, /address changed/],
  ['different Worker', h => { h.options.inputs = { ...inputs, workerName: 'other' }; }, /address changed/],
  ['missing namespace', h => { delete h.remote.bindings[2].namespace_id; }, /share storage/],
  ['replaced class', h => { h.remote.bindings[2].class_name = 'Replacement'; }, /share storage/],
  ['external storage', h => { h.remote.bindings[2].script_name = 'another-worker'; }, /share storage/],
]) {
  test(`${name} refuses updates before any mutation`, async () => {
    const h = harness();
    await deployRelay(h.options);
    mutate(h);
    await assert.rejects(deployRelay(h.options), message);
    assert.equal(h.plans.length, 1);
  });
}

test('namespace replacement is detected in post-deploy verification', async () => {
  const h = harness();
  await deployRelay(h.options);
  h.options.deploy = async () => { h.remote.bindings[2].namespace_id = 'c'.repeat(32); };
  await assert.rejects(deployRelay(h.options), /namespace changed/);
});

test('another deployment cannot silently replace the generated identity', async () => {
  const h = harness();
  const publish = h.options.deploy;
  h.options.deploy = async plan => {
    await publish(plan);
    const state = JSON.parse(h.remote.bindings[0].text);
    state.deploymentId = 'another-deployment';
    h.remote.bindings[0].text = JSON.stringify(state);
  };
  await assert.rejects(deployRelay(h.options), /did not match/);
});

test('failed route activation can be retried without replacing credentials', async () => {
  const h = harness();
  h.options.cloudflare.route = async () => ({ enabled: false, previews_enabled: false });
  await assert.rejects(deployRelay(h.options), /stable workers.dev route/);
  h.options.cloudflare.route = async () => ({ enabled: true, previews_enabled: false });
  h.options.newToken = () => assert.fail('credential already exists');
  assert.equal((await deployRelay(h.options)).kind, 'update');
});

test('custom-domain updates retain their route, credential and share namespace', async () => {
  const h = harness();
  const route = { kind: 'custom-domain', hostname: 'relay.example.com', zoneId: 'c'.repeat(32) };
  h.options.inputs = { ...inputs, route };
  h.options.cloudflare.subdomain = () => assert.fail('custom-domain deployment does not use workers.dev');
  h.options.cloudflare.zone = async () => ({ id: route.zoneId, name: 'example.com', status: 'active', account: { id: inputs.accountId } });
  h.options.cloudflare.domains = async () => h.remote ? [{
    hostname: route.hostname, zone_id: route.zoneId, service: inputs.workerName, environment: 'production',
    enabled: true, previews_enabled: false,
  }] : [];
  h.options.cloudflare.route = async () => ({ enabled: false, previews_enabled: false });
  const first = await deployRelay(h.options);
  assert.equal(JSON.parse(h.encrypted[0].plaintext).relays[0].url, 'https://relay.example.com');
  h.options.newToken = h.options.encrypt = () => assert.fail('updates must retain credentials');
  assert.deepEqual((await deployRelay(h.options)).state, first.state);
  h.options.inputs = { ...inputs, route: { ...route, hostname: 'different.example.com' } };
  h.options.cloudflare.domains = async () => [];
  await assert.rejects(deployRelay(h.options), /address changed/);
  assert.equal(h.plans.length, 2);
});

test('configuration inputs reject missing values, shell syntax and invalid hostnames', () => {
  const env = { CLOUDFLARE_ACCOUNT_ID: inputs.accountId, CLOUDFLARE_WORKER_NAME: inputs.workerName, WINDSHARE_CONFIG_RECIPIENT: inputs.recipient };
  assert.deepEqual(readInputs(env), inputs);
  for (const key of Object.keys(env)) {
    assert.throws(() => readInputs({ ...env, [key]: '' }));
    assert.throws(() => readInputs({ ...env, [key]: 'x; echo malicious' }));
  }
  assert.throws(() => endpointFor(inputs, ''));
  assert.throws(() => endpointFor(inputs, 'https://other'));
});
