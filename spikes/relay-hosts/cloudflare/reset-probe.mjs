import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { setTimeout as delay } from 'node:timers/promises';
import WebSocket from 'ws';

const base = process.argv[2]?.replace(/\/$/, '');
const token = process.env.PROBE_TOKEN
  ?? (await readFile(new URL('./.evidence/token', import.meta.url), 'utf8')).trim();
const mode = process.argv[3] ?? 'paused';
if (!base || !['paused', 'empty'].includes(mode)) {
  throw new Error('Usage: node reset-probe.mjs <Preview URL> [paused|empty]');
}
const run = 'reset-' + randomUUID();
const endpoint = base + '/probe/' + run;
const headers = { Authorization: 'Bearer ' + token };
const sockets = [];
const evidence = { run, endpoint, mode, started_at: new Date().toISOString() };
async function request(action, body, timeout = 20_000) {
  return fetch(endpoint + '/' + action, { headers: { ...headers, 'Content-Type': 'application/json' },
    method: body ? 'POST' : 'GET', body: body ? JSON.stringify(body) : undefined,
    signal: AbortSignal.timeout(timeout) });
}
async function connect(role) {
  const socket = new WebSocket(endpoint.replace(/^http/, 'ws') + '/ws?role=' + role,
    { headers, handshakeTimeout: 20_000, maxPayload: 65_536 });
  sockets.push(socket);
  await new Promise((resolve, reject) => { socket.once('open', resolve); socket.once('error', reject); });
  socket.on('error', () => {});
  return socket;
}
try {
  let healthyClosed = null;
  if (mode === 'paused') {
    const slow = await connect('slow');
    slow.pause();
    const healthy = await connect('healthy');
    healthyClosed = false;
    healthy.once('close', () => { healthyClosed = true; });
    const burst = await request('burst', { role: 'slow', frames: 128, close: true });
    assert.equal(burst.status, 200);
    await delay(6_000);
  }
  evidence.before = await (await request('status')).json();
  const beforeBoot = evidence.before.boot;
  const resetStart = performance.now();
  try {
    const response = await request('reset', {});
    evidence.reset_http_status = response.status;
  } catch (error) {
    evidence.reset_error = String(error);
  }
  // Probe both sides of client teardown: a reset must not be credited with cleanup
  // that actually required the paused peer to destroy its socket.
  try {
    evidence.while_paused = await (await request('status', undefined, 5_000)).json();
    evidence.reset_observed_before_client_teardown = evidence.while_paused.boot !== beforeBoot;
    evidence.surviving_socket_count = evidence.while_paused.sockets.length;
  } catch (error) {
    evidence.status_error_while_paused = String(error);
    evidence.reset_observed_before_client_teardown = false;
  }
  evidence.reset_probe_ms = performance.now() - resetStart;
  evidence.healthy_peer_closed_before_client_teardown = healthyClosed;
  for (const socket of sockets) socket.terminate();
  evidence.client_teardown_at = new Date().toISOString();
  for (let attempt = 0; attempt < 3; attempt++) {
    await delay(250);
    try {
      evidence.after_client_teardown = await (await request('status', undefined, 5_000)).json();
      break;
    } catch (error) {
      evidence.status_error_after_client_teardown = String(error);
    }
  }
  // An independent object distinguishes object-local failure from an unavailable Preview.
  try {
    const response = await fetch(base + '/probe/control-' + randomUUID() + '/status',
      { headers, signal: AbortSignal.timeout(5_000) });
    evidence.fresh_object_control = { status: response.status, body: await response.json() };
  } catch (error) {
    evidence.fresh_object_control = { error: String(error) };
  }
  evidence.result = 'observed';
  evidence.physical_release_proven = false;
} catch (error) {
  evidence.result = 'failed';
  evidence.error = String(error.stack ?? error);
  process.exitCode = 1;
} finally {
  for (const socket of sockets) socket.terminate();
  evidence.finished_at = new Date().toISOString();
  await mkdir(new URL('./.evidence/', import.meta.url), { recursive: true });
  await writeFile(new URL('./.evidence/' + run + '.json', import.meta.url),
    JSON.stringify(evidence, null, 2) + '\n');
  console.log(JSON.stringify({ result: evidence.result, run, mode,
    fresh_object_control: evidence.fresh_object_control,
    reset_http_status: evidence.reset_http_status, reset_probe_ms: evidence.reset_probe_ms,
    reset_observed_before_client_teardown: evidence.reset_observed_before_client_teardown,
    surviving_socket_count: evidence.surviving_socket_count,
    healthy_peer_closed_before_client_teardown: evidence.healthy_peer_closed_before_client_teardown,
    status_error_while_paused: evidence.status_error_while_paused, error: evidence.error }, null, 2));
}
