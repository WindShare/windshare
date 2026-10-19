// Opt-in live check: owns a randomly named Worker and removes only that Worker.
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { mkdir, mkdtemp, readFile, rm, rmdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { cloudflareClient } from './cloudflare.mjs';
import { FULL_DEPLOYMENT, RELAY_ONLY_DEPLOYMENT } from './configuration.mjs';

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const wrangler = join(repositoryRoot, 'relay-rust/crates/relay-cloudflare/node_modules/wrangler/bin/wrangler.js');
const workerName = 'windshare-actions-test-' + randomUUID().replaceAll('-', '').slice(0, 12);
const directory = await mkdtemp(join(tmpdir(), 'windshare-deploy-smoke-'));
const keyFile = join(directory, 'identity.key');
const stateFile = join(directory, 'share.json');
const savedConfig = join(directory, 'saved-config.json');
const outputDirectory = join(directory, 'export');
const HOSTING_REQUEST_TIMEOUT_MS = 30_000;
const env = { ...process.env, CLOUDFLARE_WORKER_NAME: workerName, WINDSHARE_DEPLOYMENT_OUTPUT: outputDirectory,
  WINDSHARE_CONFIG: savedConfig, CI: 'true', WRANGLER_SEND_METRICS: 'false',
  CLOUDFLARE_DEPLOYMENT_MODE: FULL_DEPLOYMENT,
  CLOUDFLARE_RELAY_DOMAIN: '', CLOUDFLARE_ZONE_ID: '' };
const paths = [keyFile, stateFile, savedConfig, join(outputDirectory, 'relay.json.age'), join(outputDirectory, 'deployment.json')];

function command(program, args, input, capture = false) {
  const result = spawnSync(program, args, {
    cwd: repositoryRoot, env, windowsHide: true, timeout: 20 * 60 * 1000,
    stdio: [input === undefined ? 'ignore' : 'pipe', capture ? 'pipe' : 'inherit', 'inherit'],
    input, encoding: 'utf8',
  });
  assert.ok(!result.error && result.status === 0, `${program} failed (exit ${result.status})`);
  return result.stdout;
}

async function verifyHosting(deployment) {
  const request = path => fetch(new URL(path, deployment.endpoint), {
    headers: { 'Sec-Fetch-Mode': 'navigate' }, signal: AbortSignal.timeout(HOSTING_REQUEST_TIMEOUT_MS), redirect: 'error',
  });
  const response = await request('/');
  if (deployment.mode === FULL_DEPLOYMENT) {
    assert.equal(deployment.frontendUrl, deployment.endpoint);
    assert.equal(response.status, 200);
    assert.match(response.headers.get('Content-Type'), /text\/html/);
    const html = await response.text();
    const modulePath = html.match(/<script\b[^>]*\bsrc="([^"]+)"/)?.[1];
    assert.ok(modulePath, 'frontend must load its JavaScript bundle');
    const moduleResponse = await request(modulePath);
    assert.equal(moduleResponse.status, 200);
    assert.match(moduleResponse.headers.get('Content-Type'), /javascript/);
  } else {
    assert.equal(deployment.frontendUrl, null);
    assert.equal(response.status, 400);
  }
  const relay = await request('/v2/ws');
  assert.equal(relay.status, 400, 'relay paths must not fall through to the SPA');
  assert.doesNotMatch(relay.headers.get('Content-Type') ?? '', /text\/html/);
}

let attempted = false;
try {
  const cloudflare = cloudflareClient({
    accountId: env.CLOUDFLARE_ACCOUNT_ID, workerName, apiToken: env.CLOUDFLARE_API_TOKEN,
  });
  assert.equal(await cloudflare.settings(), null, 'smoke Worker name must be unused');
  command('age-keygen', ['-o', keyFile]);
  env.WINDSHARE_CONFIG_RECIPIENT = command('age-keygen', ['-y', keyFile], undefined, true).trim();
  await mkdir(outputDirectory);
  console.log(JSON.stringify({ event: 'smoke_started', workerName }));
  attempted = true;
  const run = () => command(process.execPath, ['scripts/deploy/cloudflare/run.mjs']);
  run();
  const before = JSON.parse(await readFile(join(outputDirectory, 'deployment.json'), 'utf8'));
  await verifyHosting(before);
  const firstExport = await readFile(join(outputDirectory, 'relay.json.age'), 'utf8');
  const plaintext = command('age', ['--decrypt', '-i', keyFile, join(outputDirectory, 'relay.json.age')], undefined, true);
  command('go', ['run', './cmd/wind', 'config', 'import', '-', '--front-url', before.frontendUrl], plaintext);
  const imported = JSON.parse(await readFile(savedConfig, 'utf8'));
  assert.deepEqual(imported, { ...JSON.parse(plaintext), front_url: before.frontendUrl });
  assert.equal(imported.relays[0].url, before.endpoint);
  env.WINDSHARE_RELAY_REGISTRATION_SECRET = imported.relays[0].registration_token;
  const probe = phase => command('go', ['run', './relay-rust/tests/_goclient',
    '-endpoint', before.endpoint + '/v2/ws', '-deployment-phase', phase, '-deployment-state', stateFile]);
  probe('prepare');
  for (const mode of [RELAY_ONLY_DEPLOYMENT, FULL_DEPLOYMENT]) {
    env.CLOUDFLARE_DEPLOYMENT_MODE = mode;
    run();
    const after = JSON.parse(await readFile(join(outputDirectory, 'deployment.json'), 'utf8'));
    assert.deepEqual(after, { ...before, mode, frontendUrl: mode === FULL_DEPLOYMENT ? before.endpoint : null });
    assert.equal(await readFile(join(outputDirectory, 'relay.json.age'), 'utf8'), firstExport);
    await verifyHosting(after);
    probe('verify');
  }
  console.log(JSON.stringify({ event: 'smoke_passed', workerName, ...before }));
} finally {
  try {
    if (attempted) command(process.execPath, [wrangler, 'delete', workerName, '--config',
      'relay-rust/crates/relay-cloudflare/wrangler.toml']);
  } finally {
    // All paths are named files in this run's mkdtemp directory; no recursive
    // deletion can traverse user configuration or another run's artifacts.
    for (const path of paths) await rm(path, { force: true });
    await rmdir(outputDirectory).catch(error => { if (error.code !== 'ENOENT') throw error; });
    await rmdir(directory);
    console.log(JSON.stringify({ event: 'smoke_cleanup_finished', workerName }));
  }
}
