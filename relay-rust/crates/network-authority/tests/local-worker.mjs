import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash, createPublicKey, generateKeyPairSync, pbkdf2Sync, sign } from 'node:crypto';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const crate = fileURLToPath(new URL('../', import.meta.url));
export const DEFAULT_TEST_BUILD = 'build/test-release';

export function resolveTestBuild() {
  return resolve(crate, process.env.AUTHORITY_TEST_BUILD || DEFAULT_TEST_BUILD);
}

const { Miniflare, Log, LogLevel, convertV4MiniflareOptions } =
  createRequire(new URL('../../relay-cloudflare/package.json', import.meta.url))('miniflare');

const PASSWORD_DOMAINS = {
  account: 'windshare-account-password-v1:',
  admin: 'windshare-admin-password-v1:',
};

export const TEST_PASSWORD_PEPPER = 'v1:' + Buffer.alloc(32, 0x5a).toString('base64url');

export function derivePasswordVerifier(role, username, password) {
  const prefix = PASSWORD_DOMAINS[role];
  if (!prefix) throw new RangeError('Unknown password role: ' + role);
  const canonical = username.trim().toLowerCase();
  return pbkdf2Sync(
    Buffer.from(password, 'utf8'),
    Buffer.from(prefix + canonical, 'utf8'),
    600_000,
    32,
    'sha256',
  ).toString('base64url');
}

export function signedDescriptor(host, nodeKey, node, authority, overrides = {}) {
  const rawNode = Buffer.isBuffer(node) ? node : Buffer.from(node);
  const now = Math.floor(Date.now() / 1000);
  const payload = Buffer.from(JSON.stringify({
    version: 2,
    node_key: rawNode.toString('base64url'),
    node_id: createHash('sha256').update('windshare/node-identity/v1\0').update(rawNode).digest('base64url'),
    relay_endpoint: 'wss://' + host + '/v2/ws',
    zone: 'global',
    region: 'edge-test',
    authority,
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

function directoryRelease(nodes) {
  const keys = () => generateKeyPairSync('ed25519');
  const encodeKey = key => key.export({ format: 'der', type: 'spki' }).subarray(-32).toString('base64url');
  const signed = (key, domain, value) => {
    const bytes = Buffer.from(JSON.stringify(value));
    return { payload: bytes.toString('base64url'),
      signature: sign(null, Buffer.concat([Buffer.from(domain), bytes]), key).toString('base64url') };
  };
  const root = keys();
  const online = keys();
  const epoch = Math.floor(Date.now() / 1000) * 1000;
  const date = days => new Date(epoch + days * 86400_000).toISOString().replace('.000Z', 'Z');
  const network = 'test-network';
  const certificate = { root: 'test-root', ...signed(root.privateKey, 'windshare/network-certificate/v1\0',
    { format: 1, network, generation: 1, key: encodeKey(online.publicKey), not_before: date(-1), expires_at: date(120) }) };
  const snapshot = { certificate, ...signed(online.privateKey, 'windshare/network-directory/v1\0',
    { format: 1, network, generation: 1, serial: 1, issued_at: date(0), expires_at: date(90), nodes, public_stun: [] }) };
  const release = { network, roots: [{ id: 'test-root', key: encodeKey(root.publicKey) }], snapshot,
    link_host: 'https://front.example', backup_entry: 'https://backup.example' };
  return { release, bindings: { TEST_DIRECTORY_RELEASE: JSON.stringify(release),
    DIRECTORY_CERTIFICATE: JSON.stringify(certificate), DIRECTORY_SIGNING_SEED: online.privateKey.export({ format: 'jwk' }).d } };
}

export async function fixture(
  t,
  extra = {},
  { activeNode = false, outbound = null, testControl = false, manualAlarm = false } = {},
) {
  const keys = await mkdtemp(join(tmpdir(), 'windshare-password-edge-test-'));
  t.after(() => rm(keys, { recursive: true, force: true }));
  execFileSync('cargo', ['run', '--quiet', '--locked', '--bin', 'network-authority-keys', '--', keys, '1'], { cwd: crate });
  const trust = JSON.parse(await readFile(join(keys, 'trust-1.json'), 'utf8'));
  const build = resolveTestBuild();
  const nodeKey = generateKeyPairSync('ed25519').privateKey;
  const node = createPublicKey(nodeKey).export({ type: 'spki', format: 'der' }).subarray(-32);
  const activeNodeKey = generateKeyPairSync('ed25519').privateKey;
  const activeNodeBytes = createPublicKey(activeNodeKey).export({ type: 'spki', format: 'der' }).subarray(-32);
  const host = 'relay.example';
  const base = 'https://' + host;
  const requests = [];
  const authority = { url: extra.API_ORIGIN ?? 'https://localhost', issuer: trust.root };
  const activeSpec = { alias: 'active-edge', node_key: activeNodeBytes.toString('base64url'),
    relay: 'https://active.example', stun: [], roles: ['relay'], zone: 'global', region: 'test',
    provider: 'test', operator: 'official', capacity_mbps: 10, status: 'active' };
  const { release, bindings } = directoryRelease(activeNode ? [activeSpec] : []);
  const useWrapper = testControl || manualAlarm;
  const bootstrap = useWrapper ? [{ type: 'ESModule', path: join(build, 'fixture.mjs'), contents: `
    import worker, { NetworkAuthority } from './index.js';
    export default worker;
    export class AuthorityFixture {
      constructor(state, env) {
        this.state = state;
        this.kvFailCount = 0;
        this.alarmSetFailCount = 0;
        this.alarmDeleteFailCount = 0;
        this.deliverySqlFailCount = 0;
        this.txCommitFailCount = 0;
        this.signingCertReads = 0;
        this.signingSeedReads = 0;
        this.ddlCount = 0;
        this.ddlInTransaction = 0;
        this.transactionDepth = 0;
        const self = this;
        const wrapSql = sql => new Proxy(sql, {
          get(target, prop) {
            if (prop === 'exec') {
              return (query, ...bindings) => {
                if (/^\\s*CREATE\\s+(?:TABLE|INDEX)\\b/i.test(String(query))) {
                  self.ddlCount += 1;
                  if (self.transactionDepth > 0) self.ddlInTransaction += 1;
                }
                if (self.deliverySqlFailCount > 0 && bindings.includes('directory/kv-delivery')) {
                  self.deliverySqlFailCount -= 1;
                  throw new Error('DO SQLite delivery ledger unavailable');
                }
                return target.exec(query, ...bindings);
              };
            }
            const value = Reflect.get(target, prop, target);
            return typeof value === 'function' ? value.bind(target) : value;
          },
        });
        const wrapStorage = storage => new Proxy(storage, {
          get(target, prop) {
            if (prop === 'sql') return wrapSql(target.sql);
            if (prop === 'setAlarm') {
              return (...args) => {
                if (self.alarmSetFailCount > 0) {
                  self.alarmSetFailCount -= 1;
                  return Promise.reject(new Error('DO setAlarm unavailable'));
                }
                return target.setAlarm(...args);
              };
            }
            if (prop === 'deleteAlarm') {
              return (...args) => {
                if (self.alarmDeleteFailCount > 0) {
                  self.alarmDeleteFailCount -= 1;
                  return Promise.reject(new Error('DO deleteAlarm unavailable'));
                }
                return target.deleteAlarm(...args);
              };
            }
            if (prop === 'transactionSync') {
              return callback => {
                self.transactionDepth += 1;
                try {
                  return target.transactionSync(() => {
                    const result = callback();
                    if (self.txCommitFailCount > 0) {
                      self.txCommitFailCount -= 1;
                      throw new Error('DO transactionSync commit failed');
                    }
                    return result;
                  });
                } finally {
                  self.transactionDepth -= 1;
                }
              };
            }
            const value = Reflect.get(target, prop, target);
            return typeof value === 'function' ? value.bind(target) : value;
          },
        });
        const wrappedState = new Proxy(state, {
          get(target, prop) {
            if (prop === 'storage') return wrapStorage(target.storage);
            const value = Reflect.get(target, prop, target);
            return typeof value === 'function' ? value.bind(target) : value;
          },
        });
        const wrappedEnv = new Proxy(env, {
          get(target, prop) {
            if (prop === 'AUTHORITY_CERTIFICATE') self.signingCertReads += 1;
            if (prop === 'AUTHORITY_SIGNING_SEED') self.signingSeedReads += 1;
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
        this.authority = new NetworkAuthority(wrappedState, wrappedEnv);
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
        if (path === '/__test/tx-commit-fail' && request.method === 'POST') {
          const { count } = await request.json();
          this.txCommitFailCount = count;
          return Response.json({ count: this.txCommitFailCount });
        }
        if (path === '/__test/alarm-faults' && request.method === 'POST') {
          const { setAlarm = 0, deleteAlarm = 0, deliverySql = 0 } = await request.json();
          this.alarmSetFailCount = setAlarm;
          this.alarmDeleteFailCount = deleteAlarm;
          this.deliverySqlFailCount = deliverySql;
          return Response.json({
            setAlarm: this.alarmSetFailCount,
            deleteAlarm: this.alarmDeleteFailCount,
            deliverySql: this.deliverySqlFailCount,
          });
        }
        if (path === '/__test/alarm' && request.method === 'GET') {
          return Response.json({ alarm: await this.state.storage.getAlarm() });
        }
        if (path === '/__test/alarm' && request.method === 'DELETE') {
          await this.state.storage.deleteAlarm();
          return Response.json({ alarm: null });
        }
        if (path === '/__test/run-alarm' && request.method === 'POST') {
          await this.state.storage.deleteAlarm();
          try {
            return await this.authority.alarm();
          } catch (error) {
            return Response.json({ error: String(error) }, { status: 500 });
          }
        }
        if (path === '/__test/signing-reads' && request.method === 'GET') {
          return Response.json({
            certificate: this.signingCertReads,
            seed: this.signingSeedReads,
          });
        }
        if (path === '/__test/schema-audit' && request.method === 'GET') {
          const indexes = [...this.state.storage.sql.exec(
            "SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'authority_records' AND name NOT LIKE 'sqlite_autoindex_%' ORDER BY name",
          )].map(row => row.name);
          return Response.json({
            indexes,
            ddlCount: this.ddlCount,
            ddlInTransaction: this.ddlInTransaction,
          });
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
        if (url.hostname === 'cloudflare-dns.com') {
          assert.equal(url.searchParams.get('name'), host);
          return Response.json({ Status: 0, TC: false, Answer: url.searchParams.get('type') === '1' ? [{ type: 1, data: '1.1.1.1' }] : [] });
        }
        if (url.href !== base + '/.well-known/windshare') throw Error('Unexpected outbound request ' + url.href);
        return Response.json(signedDescriptor(host, nodeKey, node, authority));
      },
      kvNamespaces: ['DIRECTORY'],
      bindings: {
        API_ORIGIN: 'https://localhost',
        AUTHORITY_ROOT: trust.root,
        AUTHORITY_CERTIFICATE: trust.certificate,
        AUTHORITY_SIGNING_SEED: (await readFile(join(keys, 'online-1.seed'), 'utf8')).trim(),
        INITIALIZATION_TOKEN: 'local-initialization-token',
        PASSWORD_PEPPER: TEST_PASSWORD_PEPPER,
        ...bindings,
        ...extra,
      },
      durableObjects: { AUTHORITY: { className: useWrapper ? 'AuthorityFixture' : 'NetworkAuthority', useSQLite: true } },
    }],
  }));
  t.after(() => mf.dispose());
  const worker = await mf.getWorker('authority');
  const enrollment = { url: base, node_key: node.toString('base64url'), alias: 'test-node', zone: 'global',
    region: 'edge-test', provider: 'test', roles: ['relay'], capacity_mbps: 5 };
  return { worker, mf, trust, keys, nodeKey, node: [...node], activeNodeKey, activeNode: [...activeNodeBytes], requests, authority, release, enrollment };
}
