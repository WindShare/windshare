import assert from 'node:assert/strict';
import { randomBytes } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { Miniflare, Log, LogLevel, convertV4MiniflareOptions } from 'miniflare';
import { provisionTURN } from '../../../../scripts/deploy/cloudflare/turn.mjs';

const root = fileURLToPath(new URL('../', import.meta.url));
const consoleOrigin = 'https://localhost';
const password = 'local-turn-owner-password';
export const identifier = () => randomBytes(16).toString('base64url');

export async function turnFixture({ provider, lifetimeMs, analytics } = {}) {
  const keys = [];
  let turnSecret, address, cookie;
  let issued = 0;
  const options = () => {
    const shared = { compatibilityDate: '2026-10-17',
      compatibilityFlags: ['web_socket_auto_reply_to_close'], modulesRoot: root };
    const module = (path, type = 'ESModule') => ({ type, path: root + path });
    return convertV4MiniflareOptions({ log: new Log(LogLevel.ERROR),
      ...(address ? { port: Number(address.port) } : {}), workers: [
      { ...shared, name: 'relay', modules: [module('tests/access-host.mjs'), module('entry.mjs'),
        module('build/index.js'), module('build/index_bg.wasm', 'CompiledWasm')],
        bindings: { OWNER_PASSWORD: password, CONSOLE_ORIGIN: consoleOrigin,
          RELAY_URL: address?.origin ?? 'http://127.0.0.1', FRONTEND_URL: address?.origin ?? 'http://127.0.0.1',
          WINDSHARE_DEPLOYMENT: JSON.stringify({ deploymentId: 'local-managed-turn' }),
          ...(turnSecret ? { TURN_KEY: turnSecret } : {}),
          ...(lifetimeMs ? { TURN_TEST_LIFETIME_MS: lifetimeMs } : {}),
          ...(analytics ? { TURN_ANALYTICS: JSON.stringify({ account_id: 'a'.repeat(32), api_token: 'readonly-analytics' }) } : {}) },
        outboundService: async request => {
          if (new URL(request.url).hostname === 'api.cloudflare.com' && analytics) return analytics(request);
          assert.equal(new URL(request.url).hostname, 'rtc.live.cloudflare.com');
          assert.equal(request.headers.get('Authorization'), 'Bearer local-provider-secret');
          if (provider) return provider(request);
          if (new URL(request.url).pathname.endsWith('/revoke')) return new Response(null, { status: 204 });
          issued++;
          return Response.json({ iceServers: [{ urls: ['stun:127.0.0.1:3478'] },
            { urls: ['turn:127.0.0.1:3478?transport=udp'], username: 'temporary-' + issued, credential: 'local-password' }] },
            { status: 201 });
        },
        durableObjects: { SHARES: { className: 'ShareRelayObject', useSQLite: true },
          ACCESS: { className: 'AccessObject', useSQLite: true } } },
      { ...shared, name: 'console', modules: [module('console-entry.mjs')],
        bindings: { CONSOLE_ORIGIN: consoleOrigin },
        durableObjects: { ACCESS: { className: 'AccessObject', scriptName: 'relay' } } },
    ] });
  };
  const mf = new Miniflare(options());
  try {
    address = await mf.ready;
    await mf.setOptions(options());
    async function admin(path, body) {
      const worker = await mf.getWorker('console');
      if (!cookie) {
        const response = await worker.fetch(consoleOrigin + '/api/login', { method: 'POST',
          headers: { Origin: consoleOrigin, 'Content-Type': 'application/json' },
          body: JSON.stringify({ password }) });
        assert.equal(response.status, 200, await response.clone().text());
        cookie = response.headers.get('set-cookie').split(';')[0];
      }
      const response = await worker.fetch(consoleOrigin + '/api/' + path, {
        method: body === undefined ? 'GET' : 'POST', headers: {
          Origin: consoleOrigin, Cookie: cookie, 'Content-Type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      const result = await response.json();
      if (!response.ok) throw new Error(path + ': ' + response.status + ' ' + JSON.stringify(result));
      return result;
    }
    const cloudflare = {
      turnKeys: async () => keys.map(({ uid, name }) => ({ uid, name })),
      createTURNKey: async name => {
        const key = { uid: 'localkey' + keys.length, name, secret: 'local-provider-secret' };
        keys.push(key);
        return key;
      },
      deleteTURNKey: async id => { keys.splice(keys.findIndex(key => key.uid === id), 1); },
      putSecret: async (name, value) => { assert.equal(name, 'TURN_KEY'); turnSecret = value; await mf.setOptions(options()); },
      deleteSecret: async name => { assert.equal(name, 'TURN_KEY'); turnSecret = undefined; await mf.setOptions(options()); },
    };
    const provision = () => provisionTURN({ cloudflare, consoleClient: { call: admin } });
    async function device(key, path, body, headers = {}) {
      const response = await (await mf.getWorker('relay')).fetch(address.origin + '/api/turn/' + path, {
        method: body === undefined ? 'GET' : 'POST',
        headers: { 'Content-Type': 'application/json', ...(key ? { Authorization: 'Bearer ' + key } : {}), ...headers },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      return { status: response.status, body: await response.json() };
    }
    const authority = async () => (await mf.getBindings('relay')).ACCESS.getByName('authority');
    const advance = async path => {
      const response = await (await authority()).fetch('https://internal/__test/' + path);
      assert.equal(response.status, 200, await response.text());
    };
    return { mf, admin, device, provision, keys, origin: address.origin, authority, advance,
      restart: () => mf.setOptions(options()), dispose: () => mf.dispose() };
  } catch (error) { await mf.dispose(); throw error; }
}
