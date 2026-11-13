import assert from 'node:assert/strict';
import { test } from 'node:test';
import { frontendConfig } from './configuration.mjs';
import { deployFrontend, readFrontendInputs } from './frontend.mjs';
import { STATE_BINDING } from './deployment.mjs';
import worker from './frontend-entry.mjs';

const inputs = readFrontendInputs({ CLOUDFLARE_ACCOUNT_ID: 'a'.repeat(32), CLOUDFLARE_WORKER_NAME: 'official-web' });
function cloud() {
  let settings = null;
  return { subdomain: async () => 'account', settings: async () => settings,
    route: async () => ({ enabled: true, previews_enabled: false }),
    retain: state => { settings = { bindings: [{ name: STATE_BINDING, type: 'plain_text', text: JSON.stringify(state) }] }; },
  };
}
test('frontend creates and updates without credentials, namespaces or TURN', async () => {
  const cloudflare = cloud();
  const deploy = async plan => cloudflare.retain(plan.state);
  const first = await deployFrontend({ inputs, cloudflare, deploy });
  const next = await deployFrontend({ inputs, cloudflare, deploy });
  assert.equal(first.kind, 'create');
  assert.equal(next.kind, 'update');
  assert.equal(next.state.deploymentId, first.state.deploymentId);
  const config = frontendConfig(inputs);
  assert.equal(config.durable_objects, undefined);
  assert.equal(config.build, undefined);
  assert.equal(config.assets.binding, 'ASSETS');
});
test('frontend rejects existing relay storage and unrelated Workers before upload', async () => {
  for (const settings of [{ bindings: [{ type: 'durable_object_namespace' }] }, { bindings: [] }]) {
    const cloudflare = { ...cloud(), settings: async () => settings };
    await assert.rejects(deployFrontend({ inputs, cloudflare, deploy: () => assert.fail('must not upload') }));
  }
});
test('validated SaaS can attach to an owned frontend without moving either established domain', async () => {
  const primary = readFrontendInputs({ CLOUDFLARE_ACCOUNT_ID: 'a'.repeat(32), CLOUDFLARE_WORKER_NAME: 'official-web',
    CLOUDFLARE_ZONE_ID: 'b'.repeat(32), CLOUDFLARE_FRONTEND_DOMAIN: 'windshare.cc' });
  const linked = { ...primary, route: { ...primary.route, saasHostname: 'ws.ourworld.top' } };
  let attached = false;
  const cloudflare = { ...cloud(),
    zone: async () => ({ id: primary.route.zoneId, account: { id: primary.accountId }, name: 'windshare.cc', status: 'active' }),
    domains: async () => [{ hostname: primary.route.hostname, zone_id: primary.route.zoneId,
      service: primary.workerName, environment: 'production', enabled: true, previews_enabled: false }],
    route: async () => ({ enabled: false, previews_enabled: false }),
    customHostnames: async (_, hostname) => [{ hostname, status: 'active', ssl: { status: 'active' } }],
    workerRoutes: async () => attached ? [{ pattern: 'ws.ourworld.top/*', script: primary.workerName }] : [],
  };
  const deploy = async plan => { cloudflare.retain(plan.state); attached = !!plan.state.route.saasHostname; };
  const initial = await deployFrontend({ inputs: primary, cloudflare, deploy });
  const upgraded = await deployFrontend({ inputs: linked, cloudflare, deploy });
  assert.equal(upgraded.state.deploymentId, initial.state.deploymentId);
  assert.deepEqual(upgraded.state.route, linked.route);
  await assert.rejects(deployFrontend({ inputs: primary, cloudflare, deploy: () => assert.fail('must retain link host') }), /not this frontend/);
  const moved = { ...linked, route: { ...linked.route, saasHostname: 'other.ourworld.top' } };
  await assert.rejects(deployFrontend({ inputs: moved, cloudflare, deploy: () => assert.fail('must not rebind link host') }), /not this frontend/);
});
test('SaaS link hostname requires validated TLS and refuses another Worker route', async () => {
  const inputs = readFrontendInputs({ CLOUDFLARE_ACCOUNT_ID: 'a'.repeat(32), CLOUDFLARE_WORKER_NAME: 'official-web',
    CLOUDFLARE_ZONE_ID: 'b'.repeat(32), CLOUDFLARE_FRONTEND_DOMAIN: 'windshare.cc', CLOUDFLARE_FRONTEND_SAAS_HOST: 'ws.ourworld.top' });
  let active = false;
  const cloudflare = { ...cloud(),
    zone: async () => ({ id: inputs.route.zoneId, account: { id: inputs.accountId }, name: 'windshare.cc', status: 'active' }),
    domains: async () => [],
    customHostnames: async () => [{ hostname: 'ws.ourworld.top', status: 'active', ssl: { status: active ? 'active' : 'pending_validation' } }],
    workerRoutes: async () => [{ pattern: 'ws.ourworld.top/*', script: 'unrelated-worker' }],
  };
  const deploy = () => assert.fail('must not replace another route');
  await assert.rejects(deployFrontend({ inputs, cloudflare, deploy }), /certificate validation/);
  active = true;
  await assert.rejects(deployFrontend({ inputs, cloudflare, deploy }), /route must belong/);
  assert.deepEqual(frontendConfig(inputs).routes[1], { pattern: 'ws.ourworld.top/*', zone_id: inputs.route.zoneId });
});
test('frontend serves assets, refuses absent directory and redirects only installer paths', async () => {
  const env = { ASSETS: { fetch: async () => new Response('page', { headers: { 'Content-Type': 'text/html' } }) } };
  assert.equal((await worker.fetch(new Request('https://ws.ourworld.top/share'), env)).status, 200);
  assert.equal((await worker.fetch(new Request('https://ws.ourworld.top/.well-known/windshare-network'), env)).status, 503);
  const download = await worker.fetch(new Request('https://ws.ourworld.top/downloads/latest/wind-windows-amd64.zip'), env);
  assert.equal(download.headers.get('Location'), 'https://github.com/WindShare/windshare/releases/latest/download/wind-windows-amd64.zip');
  assert.equal((await worker.fetch(new Request('https://ws.ourworld.top/downloads/latest/https:%2F%2Fevil'), env)).status, 404);
  assert.equal((await worker.fetch(new Request('https://ws.ourworld.top/downloads/latest/a.zip', { method: 'POST' }), env)).status, 405);
});

test('signed directory mirrors are readable from self-hosted frontends', async () => {
  const bytes = '{"payload":"exact-signed-bytes"}';
  const response = await worker.fetch(new Request('https://windshare.cc/.well-known/windshare-network', {
    headers: { Origin: 'https://self-hosted.example' },
  }), { ASSETS: { fetch: async () => new Response(bytes) } });
  assert.equal(response.headers.get('Access-Control-Allow-Origin'), '*');
  assert.equal(response.headers.get('Content-Type'), 'application/json');
  assert.equal(await response.text(), bytes);
});
