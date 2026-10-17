import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { setTimeout as delay } from 'node:timers/promises';
import WebSocket from 'ws';

const base = process.argv[2]?.replace(/\/$/, '');
const token = process.env.PROBE_TOKEN
  ?? (await readFile(new URL('./.evidence/token', import.meta.url), 'utf8')).trim();
if (!base || !token) throw new Error('Usage: node probe.mjs <Preview URL>; PROBE_TOKEN required');
const run = 'close-' + randomUUID();
const endpoint = base + '/probe/' + run;
const headers = { Authorization: 'Bearer ' + token };
const sockets = [];
const evidence = { run, endpoint, started_at: new Date().toISOString(), stages: [] };

async function request(action, body) {
  const response = await fetch(endpoint + '/' + action, {
    headers: { ...headers, 'Content-Type': 'application/json' },
    method: body ? 'POST' : 'GET',
    body: body ? JSON.stringify(body) : undefined,
    signal: AbortSignal.timeout(20_000),
  });
  if (!response.ok) throw new Error(action + ': ' + response.status + ' ' + await response.text());
  return response.json();
}
function opened(role) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(endpoint.replace(/^http/, 'ws') + '/ws?role=' + role,
      { headers, handshakeTimeout: 20_000, maxPayload: 65_536 });
    sockets.push(ws);
    ws.once('open', () => resolve(ws));
    ws.once('error', reject);
  });
}
async function until(predicate, timeout = 15_000) {
  const deadline = performance.now() + timeout;
  while (!predicate()) {
    if (performance.now() >= deadline) throw new Error('bounded wait expired');
    await delay(25);
  }
}
async function snapshot(stage) {
  const result = await request('status');
  evidence.stages.push({ stage, observed_at: new Date().toISOString(), ...result });
  return result;
}

try {
  const slow = await opened('slow');
  slow.pause();
  const healthy = await opened('healthy');
  let healthyFrames = 0;
  let pong = false;
  healthy.on('message', (data, binary) => {
    if (!binary) { pong = data.toString() === 'pong'; return; }
    assert.equal(data.length, 65_536);
    assert.equal(Number(data.readBigUInt64BE()), healthyFrames++);
    assert.ok(data.subarray(8).every(value => value === 0x5a));
  });
  const slowBurst = await request('burst', { role: 'slow', frames: 128, close: true });
  assert.equal(slowBurst.submitted, 128);
  const start = performance.now();
  await request('burst', { role: 'healthy', frames: 8, close: false });
  await until(() => healthyFrames === 8);
  evidence.healthy_delivery_ms = performance.now() - start;
  healthy.send('ping');
  await until(() => pong);
  await delay(6_000);
  const blocked = await snapshot('paused_receiver_after_deadline');
  assert.ok(blocked.events.some(event => event.event === 'close_deadline'),
    'the actual Durable Object Alarm must execute');
  assert.equal(blocked.physical_release_proven, false);
  const slowState = blocked.sockets.find(socket => socket.attachment?.role === 'slow');
  evidence.slow_ready_state_at_deadline = slowState?.ready_state ?? null;

  let slowFrames = 0;
  slow.on('message', (data, binary) => {
    assert.ok(binary);
    assert.equal(data.length, 65_536);
    assert.equal(Number(data.readBigUInt64BE()), slowFrames++);
  });
  slow.resume();
  await until(() => slow.readyState === WebSocket.CLOSED);
  await delay(250);
  const drained = await snapshot('receiver_resumed_and_close_completed');
  assert.equal(slowFrames, 128, 'all accepted bytes must precede the close on the wire');
  assert.ok(drained.events.some(event => event.event === 'close_callback'
    && event.details.role === 'slow'));
  evidence.slow_received_frames = slowFrames;

  const peer = await opened('peer-close');
  peer.close(1000, 'client initiated');
  await until(() => peer.readyState === WebSocket.CLOSED);
  healthy.close(1000, 'probe complete');
  await until(() => healthy.readyState === WebSocket.CLOSED);
  await delay(250);
  await snapshot('all_clients_closed');
  evidence.result = 'observed';
  evidence.limitation = 'Terminal state and callbacks are observable; host-buffer release is not exposed.';
} catch (error) {
  evidence.result = 'failed';
  evidence.error = String(error.stack ?? error);
  process.exitCode = 1;
} finally {
  for (const socket of sockets) socket.terminate();
  evidence.finished_at = new Date().toISOString();
  const directory = new URL('./.evidence/', import.meta.url);
  await mkdir(directory, { recursive: true });
  await writeFile(new URL(run + '.json', directory), JSON.stringify(evidence, null, 2) + '\n');
  console.log(JSON.stringify({ result: evidence.result, run,
    healthy_delivery_ms: evidence.healthy_delivery_ms,
    slow_ready_state_at_deadline: evidence.slow_ready_state_at_deadline,
    slow_received_frames: evidence.slow_received_frames,
    evidence_file: '.evidence/' + run + '.json', error: evidence.error }, null, 2));
}
