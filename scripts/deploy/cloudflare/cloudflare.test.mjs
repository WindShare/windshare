import assert from 'node:assert/strict';
import test from 'node:test';
import { cloudflareClient } from './cloudflare.mjs';

const config = { accountId: 'a'.repeat(32), workerName: 'private-relay', apiToken: 'private-deployment-token' };
const reply = (status, body) => new Response(JSON.stringify(body), { status });

test('account and Worker reads send bounded, nonredirecting authenticated requests', async () => {
  const calls = [];
  const client = cloudflareClient({
    ...config, fetchImpl: async (url, options) => {
      calls.push({ url, options });
      return reply(200, { success: true, result: { subdomain: 'owner', bindings: [], enabled: true, previews_enabled: false } });
    },
  });
  assert.equal(await client.subdomain(), 'owner');
  assert.deepEqual((await client.settings()).bindings, []);
  assert.equal((await client.route()).enabled, true);
  assert.deepEqual(calls.map(call => new URL(call.url).pathname), [
    `/client/v4/accounts/${config.accountId}/workers/subdomain`,
    `/client/v4/accounts/${config.accountId}/workers/scripts/private-relay/settings`,
    `/client/v4/accounts/${config.accountId}/workers/scripts/private-relay/subdomain`,
  ]);
  for (const { options } of calls) {
    assert.equal(options.headers.Authorization, `Bearer ${config.apiToken}`);
    assert.equal(options.redirect, 'error');
    assert.ok(options.signal instanceof AbortSignal);
  }
});

test('custom-domain lookups use the exact zone and hostname', async () => {
  const calls = [];
  const client = cloudflareClient({ ...config, fetchImpl: async url => {
    calls.push(url);
    return reply(200, { success: true, result: [] });
  } });
  await client.zone('b'.repeat(32));
  await client.domains('relay.example.com');
  assert.equal(new URL(calls[0]).pathname, '/client/v4/zones/' + 'b'.repeat(32));
  assert.equal(new URL(calls[1]).pathname, `/client/v4/accounts/${config.accountId}/workers/domains`);
  assert.equal(new URL(calls[1]).searchParams.get('hostname'), 'relay.example.com');
});

test('only a confirmed missing Worker permits initial deployment', async () => {
  const client = cloudflareClient({
    ...config, fetchImpl: async () => reply(404, { success: false, errors: [{ code: 10007 }] }),
  });
  assert.equal(await client.settings(), null);
  await assert.rejects(client.subdomain());
  await assert.rejects(client.route());
});

for (const [status, body] of [
  [401, { success: false, errors: [{ code: 10000 }] }],
  [403, { success: false, errors: [{ code: 10007 }] }],
  [404, { success: false, errors: [{ code: 7003 }] }],
  [404, { success: false, errors: [{ code: 10007 }, { code: 10000 }] }],
  [500, { success: false, errors: [{ code: 10007 }] }],
  [200, { success: false, errors: [{ code: 10007 }] }],
  [200, { success: true, result: null }],
]) {
  test(`HTTP ${status} with ${JSON.stringify(body)} cannot bootstrap or expose an API response body`, async () => {
    const client = cloudflareClient({
      ...config, fetchImpl: async () => reply(status, { ...body, messages: [config.apiToken] }),
    });
    await assert.rejects(client.settings(), error => {
      assert.ok(!error.message.includes(config.apiToken));
      return /Cloudflare request failed/.test(error.message);
    });
  });
}

test('malformed and unavailable API responses fail without mutation', async () => {
  const malformed = cloudflareClient({ ...config, fetchImpl: async () => new Response('<html>error</html>', { status: 502 }) });
  await assert.rejects(malformed.settings(), /invalid JSON/);
  const offline = cloudflareClient({ ...config, fetchImpl: async () => { throw new Error('connection lost'); } });
  await assert.rejects(offline.settings(), /connection lost/);
  assert.throws(() => cloudflareClient({ ...config, apiToken: '' }), /CLOUDFLARE_API_TOKEN/);
});
