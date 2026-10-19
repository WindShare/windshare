import assert from 'node:assert/strict';
import test from 'node:test';
import { endpointFor, prepareRoute, readRoute, sameRoute, verifyRoute } from './routing.mjs';

const inputs = {
  accountId: 'a'.repeat(32), workerName: 'private-relay',
  route: { kind: 'custom-domain', hostname: 'relay.example.com', zoneId: 'b'.repeat(32) },
};
const zone = () => ({ id: inputs.route.zoneId, account: { id: inputs.accountId }, name: 'example.com', status: 'active' });
const domain = () => ({ hostname: inputs.route.hostname, zone_id: inputs.route.zoneId,
  service: inputs.workerName, environment: 'production', enabled: true, previews_enabled: false });
const cloudflare = () => ({
  zone: async () => zone(), domains: async () => [domain()],
  subdomain: () => assert.fail('custom domains do not require workers.dev'),
  route: async () => ({ enabled: false, previews_enabled: false }),
});

test('custom domains are explicit complete identities, not URL fragments', () => {
  assert.deepEqual(readRoute({}), { kind: 'workers-dev' });
  const env = { CLOUDFLARE_RELAY_DOMAIN: inputs.route.hostname, CLOUDFLARE_ZONE_ID: inputs.route.zoneId };
  assert.deepEqual(readRoute(env), inputs.route);
  assert.equal(endpointFor(inputs), 'https://relay.example.com');
  for (const hostname of ['', 'https://relay.example.com', 'relay.example.com/path', '*.example.com',
    'UPPER.example.com', '-relay.example.com', 'relay.example.com:443', 'a'.repeat(64) + '.example.com']) {
    assert.throws(() => readRoute({ ...env, CLOUDFLARE_RELAY_DOMAIN: hostname }));
  }
  assert.throws(() => readRoute({ ...env, CLOUDFLARE_ZONE_ID: '' }));
  assert.ok(sameRoute(inputs.route, structuredClone(inputs.route)));
  assert.ok(!sameRoute(inputs.route, { ...inputs.route, zoneId: 'c'.repeat(32) }));
  assert.ok(!sameRoute(inputs.route, { kind: 'workers-dev' }));
});

test('a new or already-owned custom domain can deploy without a workers.dev subdomain', async () => {
  const client = cloudflare();
  assert.equal(await prepareRoute(inputs, client), endpointFor(inputs));
  await verifyRoute(inputs, client);
  client.domains = async () => [];
  assert.equal(await prepareRoute(inputs, client), endpointFor(inputs));
  await assert.rejects(verifyRoute(inputs, client), /not attached/);
});

test('zone and domain ownership failures stop deployment before changing routing', async () => {
  for (const mutate of [
    value => { value.account.id = 'c'.repeat(32); },
    value => { value.id = 'c'.repeat(32); },
    value => { value.status = 'pending'; },
    value => { value.name = 'other.example.com'; },
  ]) {
    const client = cloudflare(), value = zone();
    mutate(value);
    client.zone = async () => value;
    await assert.rejects(prepareRoute(inputs, client), /active zone/);
  }
  for (const mutate of [
    value => { value.service = 'another-worker'; },
    value => { value.environment = 'staging'; },
    value => { value.zone_id = 'c'.repeat(32); },
    value => { value.hostname = 'other.example.com'; },
  ]) {
    const client = cloudflare(), value = domain();
    mutate(value);
    client.domains = async () => [value];
    await assert.rejects(prepareRoute(inputs, client), /another Worker/);
    await assert.rejects(verifyRoute(inputs, client), /not attached/);
  }
});

test('custom-domain verification rejects unintended alternate ingress', async () => {
  const client = cloudflare();
  for (const route of [{ enabled: true, previews_enabled: false }, { enabled: false, previews_enabled: true }]) {
    client.route = async () => route;
    await assert.rejects(verifyRoute(inputs, client), /route or preview/);
  }
});
