// Opt-in live check: owns two randomly named Workers and their TURN key, deleting only those names.
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { randomUUID, randomBytes } from 'node:crypto';
import { realpathSync } from 'node:fs';
import { mkdir, mkdtemp, readFile, rm, rmdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';
import { CloudflareRequestError, cloudflareClient } from './cloudflare.mjs';
import { FULL_DEPLOYMENT, RELAY_ONLY_DEPLOYMENT } from './configuration.mjs';
import { STATE_BINDING } from './deployment.mjs';
import { turnKeyName, turnKeyPrefix } from './turn.mjs';

const REQUEST_TIMEOUT_MS = 30_000;
export const CLEANUP_LOOKUP_RETRY_DELAYS_MS = Object.freeze([500, 1000, 2000]);

function deploymentIdFromSettings(settings) {
  const binding = settings?.bindings?.find(item => item.name === STATE_BINDING && item.type === 'plain_text');
  if (typeof binding?.text !== 'string') return undefined;
  try {
    const state = JSON.parse(binding.text);
    return typeof state?.deploymentId === 'string' && state.deploymentId.length > 0 ? state.deploymentId : undefined;
  } catch {
    return undefined;
  }
}

function isRetryableLookupError(error) {
  if (!(error instanceof CloudflareRequestError)) return true;
  return error.status === 408 || error.status === 429 || error.status >= 500;
}

async function lookupWorkerSettings(workerClient, { wait, retryDelaysMs }) {
  for (let attempt = 0; ; attempt++) {
    try {
      const settings = await workerClient.settings();
      return settings === null ? { status: 'absent' } : { status: 'present', settings };
    } catch (error) {
      if (!isRetryableLookupError(error) || attempt >= retryDelaysMs.length) {
        return { status: 'failed', error };
      }
      await wait(retryDelaysMs[attempt]);
    }
  }
}

export async function cleanupSmokeResources({
  workerName,
  deploymentReceipt = null,
  client,
  deleteWorker,
  wait = delay,
  lookupRetryDelaysMs = CLEANUP_LOOKUP_RETRY_DELAYS_MS,
}) {
  const failures = [];
  const consoleWorkerName = workerName + '-console';
  const receiptConfirmed = deploymentReceipt !== null;
  const receiptKeyName = typeof deploymentReceipt?.deploymentId === 'string' && deploymentReceipt.deploymentId.length > 0
    ? turnKeyName(workerName, deploymentReceipt.deploymentId)
    : undefined;
  const lookupOptions = { wait, retryDelaysMs: lookupRetryDelaysMs };

  // When deployment.json was read with a deploymentId, both Workers and the exact TURN key are
  // already proven owned; skip settings lookups so transient control-plane errors cannot block cleanup.
  const workerLookup = receiptConfirmed && receiptKeyName
    ? null
    : await lookupWorkerSettings(client(workerName), lookupOptions);
  if (workerLookup?.status === 'failed') failures.push(workerLookup.error);

  const settingsDeploymentId = receiptKeyName || workerLookup?.status !== 'present'
    ? undefined
    : deploymentIdFromSettings(workerLookup.settings);
  const exactKeyName = receiptKeyName ?? (settingsDeploymentId ? turnKeyName(workerName, settingsDeploymentId) : undefined);

  // Only inspect account-level TURN keys when this run may have reached provisionTURN
  // (deployment receipt read, relay Worker confirmed present, or its settings lookup failed).
  if (receiptConfirmed || workerLookup?.status === 'present' || workerLookup?.status === 'failed') {
    // Prefix fallback is safe only because smoke.mjs allocates a fresh per-run random workerName.
    const prefix = turnKeyPrefix(workerName);
    const isOwned = key => (exactKeyName ? key.name === exactKeyName : key.name.startsWith(prefix));
    let keys = [];
    try {
      keys = await client(workerName).turnKeys();
    } catch (error) {
      if (!(error instanceof CloudflareRequestError && error.status === 403)) failures.push(error);
    }
    for (const key of keys.filter(isOwned)) {
      try {
        await client(workerName).deleteTURNKey(key.uid);
      } catch (error) {
        failures.push(error);
      }
    }
  }

  let shouldDeleteConsole = receiptConfirmed;
  if (!shouldDeleteConsole) {
    const consoleLookup = await lookupWorkerSettings(client(consoleWorkerName), lookupOptions);
    if (consoleLookup.status === 'failed') failures.push(consoleLookup.error);
    shouldDeleteConsole = consoleLookup.status === 'present';
  }
  if (shouldDeleteConsole) {
    try {
      deleteWorker(consoleWorkerName);
    } catch (error) {
      failures.push(error);
    }
  }

  if (receiptConfirmed || workerLookup?.status === 'present') {
    try {
      deleteWorker(workerName);
    } catch (error) {
      failures.push(error);
    }
  }
  if (failures.length) throw new AggregateError(failures, 'Owned smoke resource cleanup failed');
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

async function createCredential(deployment, ownerPassword) {
  const post = (path, body, cookie = '') => fetch(deployment.consoleUrl + '/api/' + path, {
    method: 'POST', headers: { Origin: deployment.consoleUrl, 'Content-Type': 'application/json', Cookie: cookie },
    body: JSON.stringify(body), signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS), redirect: 'error',
  });
  const login = await post('login', { password: ownerPassword });
  assert.equal(login.status, 200);
  const created = await post('credentials', { name: 'Deployment smoke' }, login.headers.get('Set-Cookie').split(';')[0]);
  assert.equal(created.status, 201);
  return { credential: await created.json(), cookie: login.headers.get('Set-Cookie').split(';')[0] };
}

async function verifyTURN(deployment, credential, cookie, bindingFixture) {
  const discover = async () => (await (await fetch(deployment.endpoint + '/.well-known/windshare')).json()).managed_turn === true;
  if (!await discover()) return false;
  const toggle = enabled => fetch(deployment.consoleUrl + '/api/turn/toggle', { method: 'POST',
    headers: { Origin: deployment.consoleUrl, Cookie: cookie, 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled }) });
  const issue = () => fetch(deployment.endpoint + '/v2/turn/credentials', { method: 'POST', redirect: 'error',
    headers: { Authorization: 'Bearer ' + credential.configuration.access_key }, signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
  // The companion's service binding and public receptor can roll out at different times.
  for (let i = 0; i < 20; i++) {
    const status = await fetch(deployment.consoleUrl + '/api/session', { headers: { Cookie: cookie } });
    if ((await status.json()).capabilities?.managed_turn === true) break;
    await new Promise(resolve => setTimeout(resolve, 1000));
  }
  assert.equal((await toggle(false)).status, 200);
  assert.equal(await discover(), true);
  const paused = await issue();
  assert.equal(paused.status, 403);
  assert.equal((await paused.json()).error, 'turn_disabled');
  assert.equal((await toggle(true)).status, 200);
  const issued = await issue();
  if (bindingFixture) {
    // This intentionally unissuable key exercises Wrangler secret preservation
    // and real console authorization when the account cannot create Calls keys.
    assert.equal(issued.status, 503);
    return false;
  }
  assert.equal(issued.status, 200);
  assert.equal(issued.headers.get('cache-control'), 'no-store');
  assert.equal((await issued.json()).ttl_seconds, 1800);
  return true;
}

async function runSmoke() {
  const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
  const wrangler = join(repositoryRoot, 'relay-rust/crates/relay-cloudflare/node_modules/wrangler/bin/wrangler.js');
  const workerName = 'windshare-actions-test-' + randomUUID().replaceAll('-', '').slice(0, 12);
  const directory = await mkdtemp(join(tmpdir(), 'windshare-deploy-smoke-'));
  const stateFile = join(directory, 'share.json');
  const savedConfig = join(directory, 'saved-config.json');
  const outputDirectory = join(directory, 'addresses');
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

  let attempted = false;
  let deploymentReceipt = null;
  let ownedKeyName;
  const client = name => cloudflareClient({ accountId: env.CLOUDFLARE_ACCOUNT_ID, workerName: name, apiToken: env.CLOUDFLARE_API_TOKEN });
  try {
    assert.equal(await client(workerName).settings(), null);
    assert.equal(await client(workerName + '-console').settings(), null);
    await mkdir(outputDirectory);
    attempted = true;
    const run = () => command(process.execPath, ['scripts/deploy/cloudflare/run.mjs']);
    run();
    const before = JSON.parse(await readFile(join(outputDirectory, 'deployment.json'), 'utf8'));
    // Record the deployment receipt before live probing so any probe failure cleans up both Workers
    // and the TURN key directly without depending on extra control-plane settings reads.
    const deploymentId = typeof before?.deploymentId === 'string' && before.deploymentId.length > 0
      ? before.deploymentId
      : undefined;
    if (before && typeof before === 'object') {
      deploymentReceipt = { deploymentId };
    }
    if (deploymentId) {
      ownedKeyName = turnKeyName(workerName, deploymentId);
    }
    await verifyHosting(before);
    assert.ok(ownedKeyName, 'Deployment report is missing deploymentId.');
    const { credential, cookie } = await createCredential(before, env.WINDSHARE_OWNER_PASSWORD);
    const discovery = async () => (await (await fetch(before.endpoint + '/.well-known/windshare')).json()).managed_turn === true;
    const bindingFixture = !await discovery() && process.env.WINDSHARE_SMOKE_TURN_BINDING === 'true';
    if (bindingFixture) {
      await client(workerName).putSecret('TURN_KEY', JSON.stringify({ key_id: 'test/invalid', api_token: randomBytes(24).toString('hex') }));
      for (let attempt = 0; attempt < 12 && !await discovery(); attempt++) await new Promise(resolve => setTimeout(resolve, 1000));
      assert.equal(await discovery(), true);
    }
    const managedTURN = await verifyTURN(before, credential, cookie, bindingFixture);
    const keys = async () => (await client(workerName).turnKeys()).filter(key => key.name === ownedKeyName).map(key => key.uid);
    const originalKeys = managedTURN ? await keys() : [];
    if (managedTURN) assert.equal(originalKeys.length, 1);
    command('go', ['run', './cmd/wind', 'relay', 'add', credential.configuration.url, '--access-key', credential.configuration.access_key]);
    command('go', ['run', './cmd/wind', 'relay', 'frontend', before.frontendUrl]);
    const imported = JSON.parse(await readFile(savedConfig, 'utf8'));
    assert.equal(imported.profiles[0].url, before.endpoint);
    assert.equal(imported.profiles[0].managed_turn === true, managedTURN || bindingFixture);
    if (managedTURN || bindingFixture) {
      env.WINDSHARE_TURN_SMOKE_CONFIG = savedConfig;
      env.WINDSHARE_TURN_SMOKE_SOURCE = managedTURN ? 'managed' : 'local';
      command(process.execPath, ['web/node_modules/@playwright/test/cli.js', 'test', 'v2-parallel-turn.network.spec.ts', '--config', 'web/playwright.network.config.ts',
        '--project', 'chromium', '--grep', 'isolated Cloudflare Worker']);
    }
    env.WINDSHARE_RELAY_ACCESS_KEY = imported.profiles[0].access_key;
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
      if (managedTURN || bindingFixture) {
        if (managedTURN) assert.deepEqual(await keys(), originalKeys);
        assert.equal(await discovery(), true);
        assert((await client(workerName).settings()).bindings.some(b => b.name === 'TURN_KEY' && b.type === 'secret_text'));
      }
      probe('verify');
    }
    console.log(JSON.stringify({ event: 'smoke_passed', workerName, managedTURN, bindingFixture, ...before }));
  } finally {
    try {
      if (attempted) {
        await cleanupSmokeResources({
          workerName,
          deploymentReceipt,
          client,
          deleteWorker: name => command(process.execPath, [wrangler, 'delete', name, '--config', 'relay-rust/crates/relay-cloudflare/wrangler.toml']),
        });
      }
    } finally {
      for (const path of paths) await rm(path, { force: true });
      await rmdir(outputDirectory).catch(error => { if (error.code !== 'ENOENT') throw error; });
      // CLI pairing also creates a lock and a private state directory under this mkdtemp root.
      await rm(directory, { recursive: true, force: true });
    }
  }
}

function isDirectEntrypoint() {
  if (!process.argv[1]) return false;
  try {
    return realpathSync(resolve(process.argv[1])) === realpathSync(fileURLToPath(import.meta.url));
  } catch {
    return false;
  }
}

if (isDirectEntrypoint()) {
  await runSmoke();
}
