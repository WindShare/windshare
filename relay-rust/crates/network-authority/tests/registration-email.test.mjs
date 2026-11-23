import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { TEST_PASSWORD_PEPPER, derivePasswordVerifier, resolveTestBuild } from './local-worker.mjs';

const crate = fileURLToPath(new URL('../', import.meta.url));
const { Miniflare, Log, LogLevel, convertV4MiniflareOptions } =
  createRequire(new URL('../../relay-cloudflare/package.json', import.meta.url))('miniflare');

test('the Worker isolates registration email challenges while tracking retries', async t => {
  const keys = await mkdtemp(join(tmpdir(), 'windshare-registration-test-'));
  t.after(() => rm(keys, { recursive: true, force: true }));
  execFileSync('cargo', ['run', '--quiet', '--locked', '--bin', 'network-authority-keys', '--', keys, '1'], { cwd: crate });
  const trust = JSON.parse(await readFile(join(keys, 'trust-1.json'), 'utf8'));
  const build = resolveTestBuild();
  let delivered;
  const mf = new Miniflare(convertV4MiniflareOptions({
    log: new Log(LogLevel.ERROR),
    workers: [{
      name: 'authority', compatibilityDate: '2026-09-28', modulesRoot: build,
      modules: [{ type: 'ESModule', path: join(build, 'index.js') },
        { type: 'CompiledWasm', path: join(build, 'index_bg.wasm') }],
      bindings: { AUTHORITY_ROOT: trust.root, AUTHORITY_CERTIFICATE: trust.certificate,
        AUTHORITY_SIGNING_SEED: (await readFile(join(keys, 'online-1.seed'), 'utf8')).trim(),
        EMAIL_WEBHOOK_URL: 'https://mail.test/send', EMAIL_WEBHOOK_TOKEN: 'local-mail-token',
        INITIALIZATION_TOKEN: 'local-initialization-token',
        PASSWORD_PEPPER: TEST_PASSWORD_PEPPER },
      durableObjects: { AUTHORITY: { className: 'NetworkAuthority', useSQLite: true } },
      outboundService: async request => {
        assert.equal(request.url, 'https://mail.test/send');
        delivered = await request.json();
        return new Response(null, { status: 204 });
      },
    }],
  }));
  t.after(() => mf.dispose());
  const worker = await mf.getWorker('authority');
  let cookie = '';
  let csrf = '';
  async function call(path, body, { status = 200, method = 'POST', admin = false, source } = {}) {
    const headers = { 'Content-Type': 'application/json', Origin: 'https://localhost' };
    if (source) headers['CF-Connecting-IP'] = source;
    if (admin) Object.assign(headers, { Cookie: cookie, 'X-CSRF-Token': csrf });
    const response = await worker.fetch('https://localhost' + path,
      { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
    const value = await response.json();
    assert.equal(response.status, status, JSON.stringify(value));
    return { response, value };
  }
  const operator = {
    username: 'local-operator',
    password_verifier: derivePasswordVerifier('admin', 'local-operator', 'Local-P2-validation-2026'),
  };
  const initialized = await call('/v1/admin/initialize', { ...operator, token: 'local-initialization-token' });
  cookie = initialized.response.headers.get('set-cookie').split(';')[0];
  const console = (await call('/v1/admin/console', undefined, { method: 'GET', admin: true })).value;
  csrf = console.csrf;
  assert.equal(console.settings.registration, 'by_account_count');
  assert.equal(console.registration_mode, 'automatic');
  const secret = randomBytes(32).toString('base64url');
  const otherSecret = randomBytes(32).toString('base64url');
  const thirdSecret = randomBytes(32).toString('base64url');
  const register = async (challenge, solution, { status, source, clientSecret = secret } = {}) =>
    (await call('/v1/accounts', { client_secret: clientSecret, challenge, solution }, { status, source })).value;
  const email = async (body, options) => {
    const challenge = (await call('/v1/accounts/email', body, options)).value;
    const [local, domain] = body.email.split('@');
    assert.equal(delivered.to, `${local}@${domain.toLowerCase()}`);
    const code = delivered.text.match(/Verification code: (\S+)/)[1];
    assert.match(code, /^\d{6}$/);
    return { challenge, code };
  };

  // Unauthenticated registration mail stays disabled while registration mode is not email.
  const disabled = await call('/v1/accounts/email', { email: 'alice@qq.com', client_secret: secret }, { status: 403 });
  assert.equal(disabled.value.kind, 'denied');
  assert.equal(disabled.value.reason, 'email_registration_disabled');
  assert.equal(delivered, undefined);

  const patchSettings = body => call('/v1/admin/settings', body, { method: 'PATCH', admin: true });
  const override = await patchSettings({ registration: 'email' });
  assert.equal(override.value.registration_mode, 'email');
  const daily_bytes = console.settings.daily_bytes + 1;
  const quota = await patchSettings({ daily_bytes });
  assert.equal(quota.value.registration_mode, 'email');
  assert.deepEqual(quota.value.settings, { ...console.settings, registration: 'email', daily_bytes });

  const verificationRequired = { status: 428 };
  const alice = { email: 'alice@qq.com', client_secret: secret };
  const bob = { email: 'bob@qq.com', client_secret: otherSecret };

  const missingSecret = { email: alice.email };
  for (const body of [missingSecret, { ...missingSecret, client_secret: null }]) {
    const rejected = await call('/v1/accounts/email', body, { status: 400 });
    assert.equal(rejected.value.kind, 'invalid');
  }
  assert.equal(delivered, undefined);

  const first = await email(alice);
  // Both attempts share the Worker's local source and must keep their own codes.
  const other = await email(bob);
  assert.equal((await register(other.challenge.id, other.code, verificationRequired)).challenge.id, '');
  assert.equal((await register('unknown', 'typo', verificationRequired)).challenge.id, '');

  // Each retry is its own Durable Object transaction, so the attempts must persist.
  for (let attempt = 1; attempt < 5; attempt++) {
    assert.deepEqual(await register(first.challenge.id, 'typo', verificationRequired), { kind: 'verification', challenge: first.challenge });
  }
  assert.equal((await register(first.challenge.id, 'typo', verificationRequired)).challenge.id, '');
  assert.equal((await register(first.challenge.id, first.code, verificationRequired)).challenge.id, '');

  const second = await email(alice);
  assert.equal((await call('/v1/accounts/email', bob, { status: 429 })).value.kind, 'rate_limited');
  assert.deepEqual(await register(second.challenge.id, 'typo', verificationRequired), { kind: 'verification', challenge: second.challenge });
  assert.equal((await register(first.challenge.id, 'typo', verificationRequired)).challenge.id, '');
  // The code follows the setup secret across a change of network.
  const completed = await register(second.challenge.id, second.code, { source: '203.0.113.7' });
  assert.equal(completed.profile.email, alice.email);
  assert.equal((await register(other.challenge.id, other.code, { clientSecret: otherSecret })).profile.email, bob.email);

  // Re-verifying Alice's canonical mailbox from a fresh device recovers her account instead of minting a new one.
  const recoveredCode = await email(
    { email: 'Alice+reinstall@QQ.COM', client_secret: thirdSecret },
    { source: '198.51.100.31' },
  );
  const recovered = await register(recoveredCode.challenge.id, recoveredCode.code, {
    clientSecret: thirdSecret, source: '198.51.100.31',
  });
  assert.deepEqual(recovered.account, completed.account);
  assert.notDeepEqual(recovered.credential, completed.credential);
  const afterRecovery = (await call('/v1/admin/console', undefined, { method: 'GET', admin: true })).value;
  assert.equal(afterRecovery.account_count, 2);

  // Rotating IPs targeting the same canonical Gmail mailbox hits the per-recipient limit after burst 3.
  for (const [idx, alias] of ['target@gmail.com', 'T.a.r.g.e.t+1@googlemail.com', 'target+2@gmail.com'].entries()) {
    await email(
      { email: alias, client_secret: randomBytes(32).toString('base64url') },
      { source: `198.51.100.${40 + idx}` },
    );
  }
  const recipientLimited = await call(
    '/v1/accounts/email',
    { email: 't.a.r.g.e.t+3@gmail.com', client_secret: randomBytes(32).toString('base64url') },
    { status: 429, source: '198.51.100.44' },
  );
  assert.equal(recipientLimited.value.kind, 'rate_limited');
});
