import assert from 'node:assert/strict';
import { resolve } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { deploymentConfig, FULL_DEPLOYMENT, readDeploymentMode, RELAY_ONLY_DEPLOYMENT } from './configuration.mjs';

const inputs = {
  accountId: 'a'.repeat(32), workerName: 'private-relay',
  route: { kind: 'custom-domain', hostname: 'relay.example.com', zoneId: 'b'.repeat(32) },
};
const host = {
  main: 'build/index.js', build: { command: 'worker-build --release --locked' },
  durable_objects: { bindings: [{ name: 'SHARES', class_name: 'ShareRelayObject' }] },
  migrations: [{ tag: 'v1', new_sqlite_classes: ['ShareRelayObject'] }],
  workers_dev: true, preview_urls: false,
};

test('deployments include the frontend by default and require an explicit valid mode to omit it', () => {
  assert.equal(readDeploymentMode({}), FULL_DEPLOYMENT);
  assert.equal(readDeploymentMode({ CLOUDFLARE_DEPLOYMENT_MODE: '' }), FULL_DEPLOYMENT);
  for (const mode of [FULL_DEPLOYMENT, RELAY_ONLY_DEPLOYMENT]) {
    assert.equal(readDeploymentMode({ CLOUDFLARE_DEPLOYMENT_MODE: mode }), mode);
  }
  for (const mode of ['false', 'relay', 'FULL', ' full ', 'full; echo unexpected']) {
    assert.throws(() => readDeploymentMode({ CLOUDFLARE_DEPLOYMENT_MODE: mode }), /CLOUDFLARE_DEPLOYMENT_MODE/);
    assert.throws(() => deploymentConfig(host, inputs, mode), /CLOUDFLARE_DEPLOYMENT_MODE/);
  }
});

test('generated deployment config preserves build/storage and uses only the chosen route in both modes', () => {
  const original = structuredClone(host);
  for (const mode of [FULL_DEPLOYMENT, RELAY_ONLY_DEPLOYMENT]) {
    const custom = deploymentConfig(host, inputs, mode);
    assert.equal(custom.name, inputs.workerName);
    assert.equal(custom.account_id, inputs.accountId);
    assert.deepEqual(custom.routes, [{ pattern: inputs.route.hostname, zone_id: inputs.route.zoneId, custom_domain: true }]);
    assert.equal(custom.workers_dev, false);
    assert.equal(custom.preview_urls, false);
    assert.deepEqual(custom.build, host.build);
    assert.deepEqual(custom.durable_objects, host.durable_objects);
    assert.deepEqual(custom.migrations, host.migrations);
    assert.equal(custom.main, host.main);
    const defaultRoute = deploymentConfig(host, { ...inputs, route: { kind: 'workers-dev' } }, mode);
    assert.deepEqual(defaultRoute.routes, []);
    assert.equal(defaultRoute.workers_dev, true);
  }
  assert.deepEqual(host, original);
});

test('full deployments serve the built SPA while reserving relay paths for the Worker', () => {
  const config = deploymentConfig(host, inputs);
  const hostDirectory = fileURLToPath(new URL('../../../relay-rust/crates/relay-cloudflare/', import.meta.url));
  const frontendDirectory = fileURLToPath(new URL('../../../web/dist', import.meta.url));
  assert.equal(resolve(hostDirectory, config.assets.directory), frontendDirectory);
  assert.equal(config.assets.not_found_handling, 'single-page-application');
  assert.deepEqual(config.assets.run_worker_first, ['/v2/*']);
});

test('relay-only deployments remove frontend assets even when the previous config included them', () => {
  const full = deploymentConfig(host, inputs);
  const relayOnly = deploymentConfig(full, inputs, RELAY_ONLY_DEPLOYMENT);
  assert.equal(Object.hasOwn(relayOnly, 'assets'), false);
  assert.deepEqual(relayOnly.durable_objects, full.durable_objects);
  assert.deepEqual(relayOnly.migrations, full.migrations);
  assert.ok(full.assets);
  assert.deepEqual(deploymentConfig(relayOnly, inputs), full);
});
