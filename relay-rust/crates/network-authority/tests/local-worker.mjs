import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash, createPublicKey, generateKeyPairSync, sign } from 'node:crypto';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const crate = fileURLToPath(new URL('../', import.meta.url));
const { Miniflare, Log, LogLevel, convertV4MiniflareOptions } =
  createRequire(new URL('../../relay-cloudflare/package.json', import.meta.url))('miniflare');

export function signedDescriptor(host, nodeKey, node, overrides = {}) {
  const rawNode = Buffer.isBuffer(node) ? node : Buffer.from(node);
  const now = Math.floor(Date.now() / 1000);
  const payload = Buffer.from(JSON.stringify({
    version: 2,
    node_key: rawNode.toString('base64url'),
    node_id: createHash('sha256').update('windshare/node-identity/v1\0').update(rawNode).digest('base64url'),
    relay_endpoint: 'wss://' + host + '/v2/ws',
    zone: 'global',
    region: 'edge-test',
    services: { relay: true, stun: false },
    stun: [],
    issued_at: now,
    expires_at: now + 300,
    ...overrides,
  }));
  return {
    version: 2,
    descriptor: payload.toString('base64url'),
    signature: sign(null, Buffer.concat([Buffer.from('windshare/node-descriptor/v2\0'), payload]), nodeKey).toString('base64url'),
  };
}

export async function fixture(
  t,
  extra = {},
  { existingEnrollment = false, outbound = null, testControl = false, manualAlarm = false } = {},
) {
  const keys = await mkdtemp(join(tmpdir(), 'windshare-password-edge-test-'));
  t.after(() => rm(keys, { recursive: true, force: true }));
  execFileSync('cargo', ['run', '--quiet', '--locked', '--bin', 'network-authority-keys', '--', keys, '1'], { cwd: crate });
  const trust = JSON.parse(await readFile(join(keys, 'trust-1.json'), 'utf8'));
  const build = resolve(crate, process.env.AUTHORITY_TEST_BUILD || 'build');
  const nodeKey = generateKeyPairSync('ed25519').privateKey;
  const node = createPublicKey(nodeKey).export({ type: 'spki', format: 'der' }).subarray(-32);
  const host = 'relay.example';
  const base = 'https://' + host;
  const requests = [];
  const mirrors = new Map();
  // Rehydrate the pre-P3 approval for the password flow from the same SQLite row
  // an upgraded deployment would retain. The test module exists only in Miniflare.
  const legacy = { key: [...node], zone: 'global', region: 'edge-test', enabled: true, anonymous: false, version: 1 };
  const useWrapper = existingEnrollment || testControl || manualAlarm;
  const seedLegacySql = existingEnrollment
    ? `const sql = state.storage.sql;
        sql.exec('CREATE TABLE IF NOT EXISTS authority_records(key TEXT PRIMARY KEY,value TEXT NOT NULL) STRICT');
        sql.exec('INSERT OR IGNORE INTO authority_records VALUES (?, ?)', ${JSON.stringify('node/' + node.toString('base64url'))}, ${JSON.stringify(JSON.stringify(legacy))});`
    : '';
  const bootstrap = useWrapper ? [{ type: 'ESModule', path: join(build, 'fixture.mjs'), contents: `
    import worker, { NetworkAuthority } from './index.js';
    export default worker;
    export class AuthorityFixture {
      constructor(state, env) {
        this.state = state;
        this.kvFailCount = 0;
        ${seedLegacySql}
        const self = this;
        const wrappedEnv = new Proxy(env, {
          get(target, prop) {
            if (prop === 'DIRECTORY' && target.DIRECTORY) {
              const kv = target.DIRECTORY;
              return new Proxy(kv, {
                get(kvTarget, kvProp) {
                  if (kvProp === 'put') {
                    return (...args) => {
                      if (self.kvFailCount > 0) {
                        self.kvFailCount -= 1;
                        return Promise.reject(new Error('KV PUT failed: 429 Too Many Requests'));
                      }
                      return kvTarget.put(...args);
                    };
                  }
                  const value = Reflect.get(kvTarget, kvProp, kvTarget);
                  return typeof value === 'function' ? value.bind(kvTarget) : value;
                },
              });
            }
            const value = Reflect.get(target, prop, target);
            return typeof value === 'function' ? value.bind(target) : value;
          },
        });
        this.authority = new NetworkAuthority(state, wrappedEnv);
      }
      async fetch(request) {
        const path = new URL(request.url).pathname;
        if (path === '/__test/age-delivery' && request.method === 'POST') {
          const { ms } = await request.json();
          const row = [...this.state.storage.sql.exec(
            'SELECT value FROM authority_records WHERE key = ?', 'directory/kv-delivery',
          )][0];
          if (!row) return Response.json({ updated: false });
          const record = JSON.parse(row.value);
          if (record.delivered) {
            record.delivered.delivered_at_ms = Math.max(0, record.delivered.delivered_at_ms - ms);
          }
          this.state.storage.sql.exec(
            'UPDATE authority_records SET value = ? WHERE key = ?', JSON.stringify(record), 'directory/kv-delivery',
          );
          return Response.json({ updated: true, record });
        }
        if (path === '/__test/kv-fail' && request.method === 'POST') {
          const { count } = await request.json();
          this.kvFailCount = count;
          return Response.json({ count: this.kvFailCount });
        }
        if (path === '/__test/alarm' && request.method === 'GET') {
          return Response.json({ alarm: await this.state.storage.getAlarm() });
        }
        if (path === '/__test/run-alarm' && request.method === 'POST') {
          await this.state.storage.deleteAlarm();
          return this.authority.alarm();
        }
        return this.authority.fetch(request);
      }
      alarm(...args) {
        if (${JSON.stringify(manualAlarm)}) return;
        return this.authority.alarm(...args);
      }
    }` }] : [];
  const mf = new Miniflare(convertV4MiniflareOptions({
    log: new Log(LogLevel.ERROR),
    workers: [{
      name: 'authority', compatibilityDate: '2026-09-28', compatibilityFlags: ['global_fetch_strictly_public'], modulesRoot: build,
      modules: [...bootstrap, { type: 'ESModule', path: join(build, 'index.js') },
        { type: 'CompiledWasm', path: join(build, 'index_bg.wasm') }],
      outboundService: async request => {
        requests.push(request.url);
        assert.equal(request.headers.get('Cookie'), null);
        assert.equal(request.headers.get('Authorization'), null);
        assert.equal(request.headers.get('X-CSRF-Token'), null);
        const url = new URL(request.url);
        if (outbound) {
          const custom = await outbound(request, url);
          if (custom) return custom;
        }
        if (mirrors.has(url.href)) return Response.json(mirrors.get(url.href));
        if (url.hostname === 'cloudflare-dns.com') {
          assert.equal(url.searchParams.get('name'), host);
          return Response.json({ Status: 0, TC: false, Answer: url.searchParams.get('type') === '1' ? [{ type: 1, data: '1.1.1.1' }] : [] });
        }
        if (url.href !== base + '/.well-known/windshare') throw Error('Unexpected outbound request ' + url.href);
        return Response.json(signedDescriptor(host, nodeKey, node));
      },
      kvNamespaces: ['DIRECTORY'],
      bindings: {
        API_ORIGIN: 'https://localhost',
        AUTHORITY_ROOT: trust.root,
        AUTHORITY_CERTIFICATE: trust.certificate,
        AUTHORITY_SIGNING_SEED: (await readFile(join(keys, 'online-1.seed'), 'utf8')).trim(),
        INITIALIZATION_TOKEN: 'local-initialization-token',
        ...extra,
      },
      durableObjects: { AUTHORITY: { className: useWrapper ? 'AuthorityFixture' : 'NetworkAuthority', useSQLite: true } },
    }],
  }));
  t.after(() => mf.dispose());
  const worker = await mf.getWorker('authority');
  const enrollment = { url: base, node_key: node.toString('base64url'), alias: 'test-node', zone: 'global',
    region: 'edge-test', provider: 'test', roles: ['relay'], capacity_mbps: 5 };
  return { worker, mf, trust, keys, nodeKey, node: [...node], requests, mirrors, enrollment };
}
