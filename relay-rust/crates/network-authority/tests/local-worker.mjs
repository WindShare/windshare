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

export async function fixture(t, extra = {}) {
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
  const mf = new Miniflare(convertV4MiniflareOptions({
    log: new Log(LogLevel.ERROR),
    workers: [{
      name: 'authority', compatibilityDate: '2026-09-28', compatibilityFlags: ['global_fetch_strictly_public'], modulesRoot: build,
      modules: [{ type: 'ESModule', path: join(build, 'index.js') },
        { type: 'CompiledWasm', path: join(build, 'index_bg.wasm') }],
      outboundService: async request => {
        requests.push(request.url);
        assert.equal(request.headers.get('Cookie'), null);
        assert.equal(request.headers.get('Authorization'), null);
        assert.equal(request.headers.get('X-CSRF-Token'), null);
        const url = new URL(request.url);
        if (mirrors.has(url.href)) return Response.json(mirrors.get(url.href));
        if (url.hostname === 'cloudflare-dns.com') {
          assert.equal(url.searchParams.get('name'), host);
          return Response.json({ Status: 0, TC: false, Answer: url.searchParams.get('type') === '1' ? [{ type: 1, data: '1.1.1.1' }] : [] });
        }
        if (url.href !== base + '/.well-known/windshare') throw Error('Unexpected outbound request ' + url.href);
        const now = Math.floor(Date.now() / 1000);
        const payload = Buffer.from(JSON.stringify({ version: 2, node_key: node.toString('base64url'),
          node_id: createHash('sha256').update('windshare/node-identity/v1\0').update(node).digest('base64url'),
          relay_endpoint: 'wss://' + host + '/v2/ws', zone: 'global', region: 'edge-test',
          services: { relay: true, stun: false }, stun: [], issued_at: now, expires_at: now + 300 }));
        return Response.json({ version: 2, descriptor: payload.toString('base64url'),
          signature: sign(null, Buffer.concat([Buffer.from('windshare/node-descriptor/v2\0'), payload]), nodeKey).toString('base64url') });
      },
      kvNamespaces: ['DIRECTORY'],
      bindings: {
        ...extra,
        API_ORIGIN: 'https://localhost',
        AUTHORITY_ROOT: trust.root,
        AUTHORITY_CERTIFICATE: trust.certificate,
        AUTHORITY_SIGNING_SEED: (await readFile(join(keys, 'online-1.seed'), 'utf8')).trim(),
        INITIALIZATION_TOKEN: 'local-initialization-token',
      },
      durableObjects: { AUTHORITY: { className: 'NetworkAuthority', useSQLite: true } },
    }],
  }));
  t.after(() => mf.dispose());
  const worker = await mf.getWorker('authority');
  const enrollment = { url: base, node_key: node.toString('base64url'), alias: 'test-node', zone: 'global',
    region: 'edge-test', provider: 'test', roles: ['relay'], capacity_mbps: 5 };
  return { worker, mf, trust, keys, nodeKey, node: [...node], requests, mirrors, enrollment };
}
