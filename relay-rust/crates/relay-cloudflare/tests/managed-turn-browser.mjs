// Cross-layer managed TURN verification: runs real Go wind, the compiled
// Cloudflare relay Worker in Miniflare, a simulated provider backed by a local
// Pion TURN server, and Chromium through the existing isolated-Worker scenario.
import assert from 'node:assert/strict';
import { execFile, spawn } from 'node:child_process';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import { Miniflare, Log, LogLevel, convertV4MiniflareOptions } from 'miniflare';

const execFileAsync = promisify(execFile);
const workerRoot = fileURLToPath(new URL('../', import.meta.url));
const repositoryRoot = fileURLToPath(new URL('../../../../', import.meta.url));

const OWNER_PASSWORD = 'local-managed-turn-browser-password';
const PROVIDER_KEY_ID = 'local-fixture';
const PROVIDER_API_TOKEN = 'local-provider-token';
const PROVIDER_ENDPOINT = `https://rtc.live.cloudflare.com/v1/turn/keys/${PROVIDER_KEY_ID}/credentials/generate-ice-servers`;
const EXPECTED_TTL_SECONDS = 1800;
const MINIMUM_TURN_BYTES = 256 * 1024;
const TURN_STARTUP_TIMEOUT_MS = 20_000;
const PROCESS_STOP_TIMEOUT_MS = 5_000;
const ALLOCATION_DRAIN_TIMEOUT_MS = 10_000;
const ALLOCATION_POLL_INTERVAL_MS = 100;
const PLAYWRIGHT_TIMEOUT_MS = 6 * 60 * 1000;

function localGoEnv() {
  return { ...process.env, GOTOOLCHAIN: 'local', GOWORK: 'off' };
}

function workerOptions(consoleOrigin, relayOrigin, provider, port) {
  const shared = {
    compatibilityDate: '2026-10-17',
    compatibilityFlags: ['web_socket_auto_reply_to_close'],
    modulesRoot: workerRoot,
  };
  const module = (path, type = 'ESModule') => ({ type, path: workerRoot + path });
  return convertV4MiniflareOptions({
    log: new Log(LogLevel.ERROR),
    ...(port === undefined ? {} : { port }),
    workers: [
      {
        ...shared,
        name: 'relay',
        modules: [
          module('entry.mjs'),
          module('build/index.js'),
          module('build/index_bg.wasm', 'CompiledWasm'),
        ],
        bindings: {
          OWNER_PASSWORD,
          CONSOLE_ORIGIN: consoleOrigin,
          RELAY_URL: relayOrigin,
          FRONTEND_URL: relayOrigin,
          TURN_KEY: JSON.stringify({ key_id: PROVIDER_KEY_ID, api_token: PROVIDER_API_TOKEN }),
        },
        outboundService: provider,
        durableObjects: {
          SHARES: { className: 'ShareRelayObject', useSQLite: true },
          ACCESS: { className: 'AccessObject', useSQLite: true },
        },
      },
      {
        ...shared,
        name: 'console',
        modules: [module('console-entry.mjs')],
        bindings: { CONSOLE_ORIGIN: consoleOrigin },
        serviceBindings: { RELAY: 'relay' },
      },
    ],
  });
}

async function stopChild(child) {
  if (child.exitCode !== null || child.signalCode !== null) return;
  const exited = new Promise(resolve => child.once('exit', resolve));
  child.kill();
  await Promise.race([exited, delay(PROCESS_STOP_TIMEOUT_MS)]);
  if (child.exitCode === null && child.signalCode === null) {
    child.kill('SIGKILL');
    await Promise.race([exited, delay(1_000)]);
  }
}

async function startLocalTURN(directory) {
  const binary = join(directory, process.platform === 'win32' ? 'parallelturn.exe' : 'parallelturn');
  await execFileAsync('go', ['build', '-o', binary, './transport/webrtc/testdata/parallelturn'], {
    cwd: repositoryRoot,
    env: localGoEnv(),
    windowsHide: true,
  });
  const child = spawn(binary, [], {
    cwd: repositoryRoot,
    env: localGoEnv(),
    stdio: ['ignore', 'pipe', 'pipe'],
    windowsHide: true,
  });
  let stdout = '';
  let stderr = '';
  child.stderr.setEncoding('utf8');
  child.stderr.on('data', chunk => { stderr += chunk; });
  try {
    const ready = await new Promise((resolveReady, rejectReady) => {
      const timer = setTimeout(() => {
        rejectReady(new Error(`local TURN fixture timed out after ${TURN_STARTUP_TIMEOUT_MS}ms: ${stderr}`));
      }, TURN_STARTUP_TIMEOUT_MS);
      const cleanup = () => {
        clearTimeout(timer);
        child.stdout.off('data', onData);
        child.off('exit', onExit);
        child.off('error', onError);
      };
      const onError = error => { cleanup(); rejectReady(error); };
      const onExit = (code, signal) => {
        cleanup();
        rejectReady(new Error(`local TURN fixture exited early (code=${code}, signal=${signal}): ${stderr}`));
      };
      const onData = chunk => {
        stdout += chunk;
        const line = stdout.split(/\r?\n/u).find(entry => entry.startsWith('{') && entry.endsWith('}'));
        if (!line) return;
        cleanup();
        try { resolveReady(JSON.parse(line)); }
        catch (error) { rejectReady(error); }
      };
      child.stdout.setEncoding('utf8');
      child.stdout.on('data', onData);
      child.once('exit', onExit);
      child.once('error', onError);
    });
    assert.ok(Array.isArray(ready.services) && ready.services.length >= 2, 'expected two local TURN services');
    return {
      service: ready.services[0],
      counters: async () => (await (await fetch(ready.stats)).json()),
      stop: () => stopChild(child),
    };
  } catch (error) {
    await stopChild(child);
    throw error;
  }
}

async function createDeviceCredential(mf, consoleOrigin) {
  const consoleWorker = await mf.getWorker('console');
  const login = await consoleWorker.fetch(consoleOrigin + '/api/login', {
    method: 'POST',
    headers: { Origin: consoleOrigin, 'Content-Type': 'application/json' },
    body: JSON.stringify({ password: OWNER_PASSWORD }),
  });
  assert.equal(login.status, 200, await login.clone().text());
  const cookie = login.headers.get('set-cookie')?.split(';')[0];
  assert.ok(cookie, 'console login did not issue a session cookie');
  const created = await consoleWorker.fetch(consoleOrigin + '/api/credentials', {
    method: 'POST',
    headers: { Origin: consoleOrigin, Cookie: cookie, 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: 'managed-turn-browser' }),
  });
  assert.equal(created.status, 201, await created.clone().text());
  return (await created.json()).configuration;
}

// Spawn asynchronously so the Node event loop remains free to serve Miniflare's
// outboundService callback while Playwright, Go wind, and Chromium execute.
async function runPlaywrightScenario(configPath) {
  const cli = join(repositoryRoot, 'web/node_modules/@playwright/test/cli.js');
  const child = spawn(
    process.execPath,
    [
      cli,
      'test',
      'v2-parallel-turn.network.spec.ts',
      '--config',
      'web/playwright.network.config.ts',
      '--project',
      'chromium',
      '--grep',
      'isolated Cloudflare Worker',
    ],
    {
      cwd: repositoryRoot,
      stdio: 'inherit',
      windowsHide: true,
      env: {
        ...localGoEnv(),
        WINDSHARE_TURN_SMOKE_CONFIG: configPath,
        WINDSHARE_TURN_SMOKE_SOURCE: 'managed',
      },
    },
  );
  const timeout = setTimeout(() => { void stopChild(child); }, PLAYWRIGHT_TIMEOUT_MS);
  try {
    const [code, signal] = await new Promise((resolveExit, rejectExit) => {
      child.once('error', rejectExit);
      child.once('exit', (exitCode, exitSignal) => resolveExit([exitCode, exitSignal]));
    });
    assert.equal(signal, null, `Playwright terminated by signal ${signal}`);
    assert.equal(code, 0, `Playwright exited with status ${code}`);
  } finally {
    clearTimeout(timeout);
  }
}

async function waitForZeroAllocations(counters) {
  const deadline = Date.now() + ALLOCATION_DRAIN_TIMEOUT_MS;
  while (true) {
    const stats = await counters();
    if (stats.every(server => server.allocations === 0)) return stats;
    if (Date.now() >= deadline) {
      throw new Error(`local TURN allocations did not drain: ${JSON.stringify(stats)}`);
    }
    await delay(ALLOCATION_POLL_INTERVAL_MS);
  }
}

const directory = await mkdtemp(join(tmpdir(), 'windshare-managed-turn-browser-'));
let turn;
let mf;
try {
  turn = await startLocalTURN(directory);
  let upstreamIssues = 0;
  const provider = async request => {
    assert.equal(request.url, PROVIDER_ENDPOINT);
    assert.equal(request.headers.get('authorization'), `Bearer ${PROVIDER_API_TOKEN}`);
    assert.deepEqual(await request.json(), { ttl: EXPECTED_TTL_SECONDS });
    upstreamIssues += 1;
    return Response.json(
      {
        iceServers: [
          { urls: 'stun:localhost' },
          { urls: turn.service.urls, username: turn.service.username, credential: turn.service.password },
        ],
      },
      { status: 201 },
    );
  };

  mf = new Miniflare(workerOptions('http://localhost', 'http://127.0.0.1', provider));
  const address = await mf.ready;
  const relayOrigin = address.origin;
  const consoleAddress = new URL(relayOrigin);
  consoleAddress.hostname = 'localhost';
  await mf.setOptions(workerOptions(consoleAddress.origin, relayOrigin, provider, Number(address.port)));

  const discovery = await (await fetch(relayOrigin + '/.well-known/windshare')).json();
  assert.equal(discovery.managed_turn, true);
  assert.equal(discovery.relay_url, relayOrigin);

  const configuration = await createDeviceCredential(mf, consoleAddress.origin);
  assert.equal(configuration.url, relayOrigin);

  // Leave managed_turn false in saved config so wind share performs live
  // /.well-known/windshare discovery against the Worker before escalation.
  const configPath = join(directory, 'saved-config.json');
  await writeFile(
    configPath,
    JSON.stringify({
      profiles: [{ url: relayOrigin, access_key: configuration.access_key, managed_turn: false }],
    }),
    { mode: 0o600 },
  );

  await runPlaywrightScenario(configPath);

  assert.ok(upstreamIssues >= 1, 'Worker did not mint managed TURN credentials through simulated provider');
  const stats = await waitForZeroAllocations(turn.counters);
  assert.ok(
    stats[0].bytes > MINIMUM_TURN_BYTES,
    `expected issued local TURN server to carry > ${MINIMUM_TURN_BYTES} bytes, got ${JSON.stringify(stats)}`,
  );
  assert.equal(stats[1].bytes, 0, `unissued local TURN server carried unexpected bytes: ${JSON.stringify(stats)}`);
  console.log(
    JSON.stringify({
      event: 'managed_turn_browser_passed',
      relayOrigin,
      upstreamIssues,
      turnBytes: stats[0].bytes,
    }),
  );
} finally {
  await Promise.allSettled([turn?.stop(), mf?.dispose()]);
  await rm(directory, { recursive: true, force: true });
}
