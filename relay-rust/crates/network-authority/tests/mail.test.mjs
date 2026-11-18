import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const crate = fileURLToPath(new URL('../', import.meta.url));
const { Miniflare, Log, LogLevel, convertV4MiniflareOptions } =
  createRequire(new URL('../../relay-cloudflare/package.json', import.meta.url))('miniflare');

test('the Worker changes email only after the provider accepts delivery and the account verifies its code', async t => {
  const keys = await mkdtemp(join(tmpdir(), 'windshare-mail-test-'));
  t.after(() => rm(keys, { recursive: true, force: true }));
  execFileSync('cargo', ['run', '--quiet', '--locked', '--bin', 'network-authority-keys', '--', keys, '1'], { cwd: crate });
  const trust = JSON.parse(await readFile(join(keys, 'trust-1.json'), 'utf8'));
  const build = resolve(crate, process.env.AUTHORITY_TEST_BUILD || 'build');
  let delivered;
  let rejectDelivery = false;
  const mf = new Miniflare(convertV4MiniflareOptions({
    log: new Log(LogLevel.ERROR),
    workers: [{
      name: 'authority', compatibilityDate: '2026-09-28', modulesRoot: build,
      modules: [{ type: 'ESModule', path: join(build, 'index.js') },
        { type: 'CompiledWasm', path: join(build, 'index_bg.wasm') }],
      bindings: { AUTHORITY_ROOT: trust.root, AUTHORITY_CERTIFICATE: trust.certificate,
        AUTHORITY_SIGNING_SEED: (await readFile(join(keys, 'online-1.seed'), 'utf8')).trim(),
        EMAIL_WEBHOOK_URL: 'https://mail.test/send', EMAIL_WEBHOOK_TOKEN: 'local-mail-token' },
      durableObjects: { AUTHORITY: { className: 'NetworkAuthority', useSQLite: true } },
      outboundService: async request => {
        assert.equal(request.url, 'https://mail.test/send');
        assert.equal(request.headers.get('Authorization'), 'Bearer local-mail-token');
        if (rejectDelivery) return new Response(null, { status: 302, headers: { Location: 'https://redirect.test' } });
        delivered = await request.json();
        return new Response(null, { status: 204 });
      },
    }],
  }));
  t.after(() => mf.dispose());
  const worker = await mf.getWorker('authority');
  const secret = randomBytes(32).toString('base64url');
  async function call(route, body, status = 200) {
    const response = await worker.fetch('https://authority.test/v1/accounts' + route, {
      method: 'POST', headers: { Authorization: 'Bearer ' + secret, 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const value = await response.json();
    assert.equal(response.status, status, JSON.stringify(value));
    return value;
  }
  await call('', { client_secret: secret });
  const email = 'alice@qq.com';
  const { challenge, address } = await call('/profile/email', { email: 'alice@QQ.COM' });
  assert.equal(address, email);
  assert.equal(delivered.to, email);
  rejectDelivery = true;
  assert.equal((await call('/profile/email', { email: 'alice@gmail.com' }, 503)).kind, 'email_delivery_unavailable');
  assert.equal((await call('/ticket', {})).profile.email, null);
  const code = delivered.text.match(/Verification code: (\S+)/)[1];
  const verified = await call('/profile/email/verify', { email, challenge: challenge.id, code });
  assert.equal(verified.email, email);
});
