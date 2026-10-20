// Opt-in live check: owns two randomly named Workers and deletes only those names.
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { randomUUID, randomBytes } from 'node:crypto';
import { mkdir, mkdtemp, readFile, rm, rmdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { cloudflareClient } from './cloudflare.mjs';
import { turnConsoleClient } from './turn.mjs';
import { FULL_DEPLOYMENT, RELAY_ONLY_DEPLOYMENT } from './configuration.mjs';

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const wrangler = join(repositoryRoot, 'relay-rust/crates/relay-cloudflare/node_modules/wrangler/bin/wrangler.js');
const workerName = 'windshare-actions-test-' + randomUUID().replaceAll('-', '').slice(0, 12);
const directory = await mkdtemp(join(tmpdir(), 'windshare-deploy-smoke-'));
const stateFile = join(directory, 'share.json');
const savedConfig = join(directory, 'saved-config.json');
const outputDirectory = join(directory, 'addresses');
const REQUEST_TIMEOUT_MS = 30_000;
const env = { ...process.env, CLOUDFLARE_WORKER_NAME: workerName, WINDSHARE_DEPLOYMENT_OUTPUT: outputDirectory,
  WINDSHARE_CONFIG: savedConfig, WINDSHARE_OWNER_PASSWORD: randomBytes(32).toString('base64url'),
  CI: 'true', WRANGLER_SEND_METRICS: 'false', CLOUDFLARE_DEPLOYMENT_MODE: FULL_DEPLOYMENT,
  CLOUDFLARE_RELAY_DOMAIN: '', CLOUDFLARE_CONSOLE_DOMAIN: '', CLOUDFLARE_ZONE_ID: '' };
const paths = [stateFile, savedConfig, join(outputDirectory, 'deployment.json')];

function command(program, args, input, capture = false) {
  const result = spawnSync(program, args, {
    cwd: repositoryRoot, env, windowsHide: true, timeout: 20 * 60 * 1000,
    stdio: [input === undefined ? 'ignore' : 'pipe', capture ? 'pipe' : 'inherit', 'inherit'],
    input, encoding: 'utf8',
  });
  assert.ok(!result.error && result.status === 0, program + ' failed (exit ' + result.status + ')');
  return result.stdout;
}
async function verifyHosting(deployment) {
  const request = path => fetch(new URL(path, deployment.endpoint), {
    headers: { 'Sec-Fetch-Mode': 'navigate' }, signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS), redirect: 'error',
  });
  const response = await request('/');
  if (deployment.mode === FULL_DEPLOYMENT) {
    assert.equal(response.status, 200);
    const modulePath = (await response.text()).match(/<script\b[^>]*\bsrc="([^"]+)"/)?.[1];
    assert.ok(modulePath);
    assert.match((await request(modulePath)).headers.get('Content-Type'), /javascript/);
  } else assert.equal(response.status, 404);
  assert.equal((await request('/v2/ws')).status, 400);
  assert.equal((await request('/api/credentials')).status, 404);
  assert.equal((await (await request('/.well-known/windshare')).json()).console_url, deployment.consoleUrl);
  const consolePage = await fetch(deployment.consoleUrl, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS), redirect: 'error' });
  assert.equal(consolePage.status, 200);
  assert.match(consolePage.headers.get('Content-Security-Policy'), /frame-ancestors 'none'/);
}
async function createCredential(deployment) {
  const post = (path, body, cookie = '') => fetch(deployment.consoleUrl + '/api/' + path, {
    method: 'POST', headers: { Origin: deployment.consoleUrl, 'Content-Type': 'application/json', Cookie: cookie },
    body: JSON.stringify(body), signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS), redirect: 'error',
  });
  const login = await post('login', { password: env.WINDSHARE_OWNER_PASSWORD });
  assert.equal(login.status, 200);
  const created = await post('credentials', { name: 'Deployment smoke' }, login.headers.get('Set-Cookie').split(';')[0]);
  assert.equal(created.status, 201);
  return created.json();
}

let attempted = false;
const client = name => cloudflareClient({ accountId: env.CLOUDFLARE_ACCOUNT_ID, workerName: name, apiToken: env.CLOUDFLARE_API_TOKEN });
let consoleClient, ownedTURN;
try {
  assert.equal(await client(workerName).settings(), null);
  assert.equal(await client(workerName + '-console').settings(), null);
  await mkdir(outputDirectory);
  attempted = true;
  const run = () => command(process.execPath, ['scripts/deploy/cloudflare/run.mjs']);
  run();
  const before = JSON.parse(await readFile(join(outputDirectory, 'deployment.json'), 'utf8'));
  await verifyHosting(before);
  assert.equal(before.turn.status, 'ready');
  consoleClient = turnConsoleClient({ origin: before.consoleUrl, password: env.WINDSHARE_OWNER_PASSWORD });
  ownedTURN = await consoleClient.call('turn/provision');
  const credential = await createCredential(before);
  command('go', ['run', './cmd/wind', 'config', 'import', '-', '--front-url', before.frontendUrl], JSON.stringify(credential.configuration));
  const imported = JSON.parse(await readFile(savedConfig, 'utf8'));
  assert.equal(imported.profiles[0].url, before.endpoint);
  assert.equal(imported.managed_turn[0].service_id, before.turn.serviceId);
  env.WINDSHARE_RELAY_ACCESS_KEY = imported.profiles[0].access_key;
  env.WINDSHARE_TURN_TRIAL_CONFIGURATION = savedConfig;
  command(process.execPath, ['web/scripts/browser-network-matrix/run-turn-trial.mjs']);
  await consoleClient.call('turn/settings', { enabled: false });
  const probe = phase => command('go', ['run', './relay-rust/tests/_goclient',
    '-endpoint', before.endpoint + '/v2/ws', '-deployment-phase', phase, '-deployment-state', stateFile]);
  probe('prepare');
  for (const mode of [RELAY_ONLY_DEPLOYMENT, FULL_DEPLOYMENT]) {
    env.CLOUDFLARE_DEPLOYMENT_MODE = mode;
    env.WINDSHARE_OWNER_PASSWORD = randomBytes(32).toString('base64url');
    run();
    const after = JSON.parse(await readFile(join(outputDirectory, 'deployment.json'), 'utf8'));
    assert.deepEqual(after, { ...before, mode, frontendUrl: mode === FULL_DEPLOYMENT ? before.endpoint : null });
    await verifyHosting(after);
    consoleClient = turnConsoleClient({ origin: after.consoleUrl, password: env.WINDSHARE_OWNER_PASSWORD });
    assert.equal((await consoleClient.call('turn/status')).enabled, false);
    probe('verify');
  }
  console.log(JSON.stringify({ event: 'smoke_passed', workerName, ...before }));
} finally {
  try {
    if (attempted) {
      const failures = [];
      try {
        // Retain confirmed ownership even if a later password rotation fails.
        if (!ownedTURN) {
          const settings = await client(workerName).settings();
          const binding = settings?.bindings?.find(value => value.name === 'WINDSHARE_DEPLOYMENT');
          if (binding) {
            consoleClient = turnConsoleClient({ origin: JSON.parse(binding.text).consoleEndpoint, password: env.WINDSHARE_OWNER_PASSWORD });
            ownedTURN = await consoleClient.call('turn/provision');
          }
        }
        if (ownedTURN) {
          const keys = await client(workerName).turnKeys();
          for (const key of keys.filter(key => key.name === ownedTURN.key_name)) {
            await client(workerName).deleteTURNKey(key.uid);
          }
        }
      } catch (error) { failures.push(error); }
      for (const name of [workerName + '-console', workerName]) {
        try { command(process.execPath, [wrangler, 'delete', name, '--config', 'relay-rust/crates/relay-cloudflare/wrangler.toml']); }
        catch (error) { failures.push(error); }
      }
      if (failures.length) throw new AggregateError(failures, 'Owned smoke resource cleanup failed');
    }
  } finally {
    for (const path of paths) await rm(path, { force: true });
    await rmdir(outputDirectory).catch(error => { if (error.code !== 'ENOENT') throw error; });
    await rmdir(directory);
  }
}
