import assert from 'node:assert/strict';
import test from 'node:test';
import { ACCESS_CLASS, SHARE_CLASS, STATE_BINDING, validateHostConfig } from './deployment.mjs';

const config = () => ({
  workers_dev: true, preview_urls: false,
  durable_objects: { bindings: [{ name: 'SHARES', class_name: SHARE_CLASS }, {name:'ACCESS',class_name:ACCESS_CLASS}] },
  migrations: [{ tag: 'v1', new_sqlite_classes: [SHARE_CLASS] },{tag:'v2-access',new_sqlite_classes:[ACCESS_CLASS]}],
});

test('additive migrations retain existing share storage', () => {
  const host = config();
  host.migrations.push({ tag: 'v2', new_sqlite_classes: ['AnotherClass'] });
  validateHostConfig(host);
});

test('destructive migrations are refused before upload, not merely detected afterwards', () => {
  for (const migration of [
    { tag: 'v2', deleted_classes: [SHARE_CLASS] },
    { tag: 'v2', new_classes: [SHARE_CLASS] },
    { tag: 'v2', renamed_classes: [{ from: SHARE_CLASS, to: 'Replacement' }] },
    { tag: 'v2', transferred_classes: [{ from: SHARE_CLASS, from_script: 'other', to: SHARE_CLASS }] },
  ]) {
    const host = config();
    host.migrations.push(migration);
    assert.throws(() => validateHostConfig(host), /cannot delete, rename or transfer/);
  }
});

test('source changes cannot replace deployment authority, routing or storage ownership', () => {
  for (const mutate of [
    host => { host.workers_dev = false; },
    host => { host.preview_urls = true; },
    host => { host.route = 'other.example.com/*'; },
    host => { host.routes = [{ pattern: 'other.example.com', custom_domain: true }]; },
    host => { host.durable_objects.bindings[0].class_name = 'AnotherClass'; },
    host => { host.durable_objects.bindings[0].script_name = 'another-worker'; },
    host => { host.migrations = []; },
    host => { host.migrations.push({ tag: 'v2', new_sqlite_classes: [SHARE_CLASS] }); },
    host => { host.vars = { [STATE_BINDING]: 'replacement' }; },
    host => { host.vars = { OWNER_PASSWORD: 'replacement' }; },
  ]) {
    const host = config();
    mutate(host);
    assert.throws(() => validateHostConfig(host));
  }
});
