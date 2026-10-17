import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { setTimeout as delay } from 'node:timers/promises';
import WebSocket from 'ws';
import { boundedPeer } from './bounded-peer.mjs';

const base = process.argv[2]?.replace(/\/$/, '');
const token = process.env.PROBE_TOKEN
  ?? (await readFile(new URL('./.evidence/token', import.meta.url), 'utf8')).trim();
const mode = process.argv[3] ?? 'paused';
const route = process.argv[4] ?? 'probe';
const closePolicy = process.argv[5] ?? 'close';
if (!base || !['paused', 'bounded', 'empty'].includes(mode)
    || !['probe', 'direct', 'bridge', 'bridge-raw', 'control'].includes(route)
    || !['close', 'open'].includes(closePolicy) || (route === 'control' && mode !== 'empty')) {
  throw new Error('Usage: node reset-probe.mjs <Preview URL> [paused|bounded|empty] [probe|direct|bridge|bridge-raw|control] [close|open]');
}
const run = 'reset-' + randomUUID();
const endpoint = base + '/' + route + '/' + run;
const headers = { Authorization: 'Bearer ' + token };
const sockets = [];
let bounded;
const evidence = { run, endpoint, mode, route, close_policy: closePolicy, started_at: new Date().toISOString() };
async function request(action, body, timeout = 20_000) {
  return fetch(endpoint + '/' + action, { headers: { ...headers, 'Content-Type': 'application/json' },
    method: body ? 'POST' : 'GET', body: body ? JSON.stringify(body) : undefined,
    signal: AbortSignal.timeout(timeout) });
}
async function connect(role, target = endpoint) {
  const socket = new WebSocket(target.replace(/^http/, 'ws') + '/ws?role=' + role,
    { headers, handshakeTimeout: 20_000, maxPayload: 65_536 });
  sockets.push(socket);
  await new Promise((resolve, reject) => { socket.once('open', resolve); socket.once('error', reject); });
  socket.on('error', () => {});
  return socket;
}
async function until(predicate, timeout = 5_000) {
  const deadline = performance.now() + timeout;
  while (!predicate()) {
    if (performance.now() >= deadline) throw new Error('bounded socket wait expired');
    await delay(25);
  }
}
async function ping(socket) {
  let pong = false;
  const listener = data => { if (data.toString() === 'pong') pong = true; };
  socket.on('message', listener);
  try { socket.send('ping'); await until(() => pong); }
  finally { socket.off('message', listener); }
}
try {
  let healthyClosed = null;
  let slow;
  let slowClosed;
  let slowFrames = 0;
  let otherObject;
  if (mode !== 'empty') {
    if (mode === 'bounded') {
      bounded = await boundedPeer(endpoint.replace(/^http/, 'ws') + '/ws?role=slow', token);
      evidence.slow_receive_buffer_bytes = bounded.ready.receive_buffer_bytes;
    } else {
      slow = await connect('slow');
      slow.on('close', (code, reason) => { slowClosed = { code, reason: reason.toString() }; });
      slow.on('message', (bytes, binary) => {
        if (binary) { assert.equal(Number(bytes.readBigUInt64BE()), slowFrames++); }
      });
      slow.pause();
    }
    const healthy = await connect('healthy');
    healthyClosed = false;
    healthy.once('close', () => { healthyClosed = true; });
    if (route === 'bridge') {
      const independent = base + '/bridge/sibling-' + randomUUID();
      otherObject = await connect('healthy', independent);
      await ping(otherObject);
    }
    const burst = await request('burst', { role: 'slow', frames: 128, close: closePolicy === 'close' });
    assert.equal(burst.status, 200);
    await delay(6_000);
  }
  evidence.before = await (await request('status')).json();
  const beforeBoot = evidence.before.boot;
  if (route.startsWith('bridge')) evidence.outer_before = await (await request('outer-status')).json();
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
  if (route.startsWith('bridge')) {
    try { evidence.outer_after = await (await request('outer-status', undefined, 5_000)).json(); }
    catch (error) { evidence.outer_status_error = String(error); }
  }
  evidence.healthy_peer_closed_before_client_teardown = healthyClosed;
  if (mode !== 'empty' && evidence.reset_observed_before_client_teardown) {
    const replacement = await connect('healthy');
    await ping(replacement);
    evidence.replacement_socket_worked = true;
    if (otherObject) {
      await ping(otherObject);
      evidence.other_object_socket_survived = true;
    }
    // Resume only after the new instance answered: observe server termination without
    // crediting explicit client socket destruction for completing the reset.
    if (bounded) {
      const drained = await bounded.resume();
      slowClosed = drained.terminal;
      slowFrames = drained.frames;
    } else {
      slow.resume();
      await until(() => slowClosed !== undefined, 15_000);
    }
    evidence.old_slow_peer_terminal = slowClosed;
    evidence.old_slow_received_frames = slowFrames;
    evidence.old_burst_fully_delivered = slowFrames === 128;
  }
  for (const socket of sockets) socket.terminate();
  if (bounded) await bounded.terminate();
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
    const response = await fetch(base + '/' + route + '/control-' + randomUUID() + '/status',
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
  if (bounded) await bounded.terminate();
  evidence.finished_at = new Date().toISOString();
  await mkdir(new URL('./.evidence/', import.meta.url), { recursive: true });
  await writeFile(new URL('./.evidence/' + run + '.json', import.meta.url),
    JSON.stringify(evidence, null, 2) + '\n');
  console.log(JSON.stringify({ result: evidence.result, run, mode, route, close_policy: closePolicy,
    slow_receive_buffer_bytes: evidence.slow_receive_buffer_bytes,
    fresh_object_control: evidence.fresh_object_control, outer_after: evidence.outer_after,
    reset_http_status: evidence.reset_http_status, reset_probe_ms: evidence.reset_probe_ms,
    reset_observed_before_client_teardown: evidence.reset_observed_before_client_teardown,
    surviving_socket_count: evidence.surviving_socket_count,
    healthy_peer_closed_before_client_teardown: evidence.healthy_peer_closed_before_client_teardown,
    replacement_socket_worked: evidence.replacement_socket_worked,
    other_object_socket_survived: evidence.other_object_socket_survived,
    old_slow_peer_terminal: evidence.old_slow_peer_terminal,
    old_slow_received_frames: evidence.old_slow_received_frames,
    status_error_while_paused: evidence.status_error_while_paused, error: evidence.error }, null, 2));
}
