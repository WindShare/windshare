import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { setTimeout as delay } from 'node:timers/promises';
import WebSocket from 'ws';
import { boundedPeer } from './bounded-peer.mjs';
import { healthyPeer, request } from './pressure-client.mjs';

const base = process.argv[2]?.replace(/\/$/, '');
const policy = process.argv[3];
const disposition = process.argv[4] ?? 'hold';
const steps = Number(process.argv[5] ?? 64);
const FRAMES = 128;
const FRAME_BYTES = 65_536;
const MAX_RUN_MS = 15 * 60_000;
const STRICT_OBSERVATION_MS = 35_000;
const FINAL_HOLD_MS = 30_000;
assert.ok(base?.startsWith('https://'));
assert.ok(['strict', 'immediate', 'cooldown'].includes(policy));
assert.ok(['hold', 'abort', 'consume'].includes(disposition));
assert.ok(Number.isInteger(steps) && steps > 0 && steps <= 64);
const token = process.env.PROBE_TOKEN ??
  (await readFile(new URL('./.evidence/token', import.meta.url), 'utf8')).trim();
const run = 'risk-' + randomUUID();
const endpoint = base + '/pressure/' + run;
const evidence = { run, endpoint, policy, disposition, started_at: new Date().toISOString(),
  scope: 'bounded platform policy probe; not the production ShareRelay or a WindShare download',
  limits: { steps, frames_per_step: FRAMES, frame_bytes: FRAME_BYTES, max_run_ms: MAX_RUN_MS },
  physical_release_proven: false, steps: [], refusals: [], healthy_peers: [], observations: [] };
const output = new URL('./.evidence/' + run + '.json', import.meta.url);
const clients = [];
const healthy = [];
const start = performance.now();
const call = (action, body) => request(endpoint, token, action, body);
async function save() {
  await mkdir(new URL('./.evidence/', import.meta.url), { recursive: true });
  await writeFile(output, JSON.stringify(evidence, null, 2) + '\n');
}
async function status() {
  const result = await call('status');
  assert.equal(result.status, 200, JSON.stringify(result));
  return result;
}
async function connectHealthy(role, target = endpoint) {
  const peer = await healthyPeer(target, token, role);
  healthy.push(peer);
  evidence.healthy_peers.push(peer.record);
  return peer;
}
async function refusedAdmission(role) {
  return new Promise((resolve, reject) => {
    const socket = new WebSocket(endpoint.replace(/^http/, 'ws') + '/ws?role=' + role,
      { headers: { Authorization: 'Bearer ' + token }, perMessageDeflate: false, handshakeTimeout: 5000 });
    socket.once('unexpected-response', (_request, response) => {
      const status = response.statusCode;
      response.resume();
      socket.terminate();
      resolve({ status, at: new Date().toISOString() });
    });
    socket.once('open', () => { socket.terminate(); resolve({ status: 101, at: new Date().toISOString() }); });
    socket.on('error', reject);
  });
}
try {
  evidence.configuration = await call('risk-configure', { policy });
  assert.equal(evidence.configuration.status, 200, JSON.stringify(evidence.configuration));
  evidence.initial_status = await status();
  const sentinel = await connectHealthy('healthy-main');
  const sibling = await connectHealthy('healthy-sibling', base + '/pressure/sibling-' + randomUUID());
  evidence.baseline = await sentinel.sample();
  for (let index = 0; index < steps; index++) {
    assert.ok(performance.now() - start < MAX_RUN_MS, 'run duration bound reached');
    let current = await status();
    if (current.value.risk.usage.charged_generations >= current.value.risk.reservation_limit) {
      const refused = { index, before: current, attempt: await refusedAdmission('slow-refused-' + index) };
      evidence.refusals.push(refused);
      assert.equal(refused.attempt.status, 429, JSON.stringify(refused));
      refused.healthy = await sentinel.sample();
      if (policy === 'strict') {
        await delay(STRICT_OBSERVATION_MS);
        refused.after_wait = await status();
        refused.second_attempt = await refusedAdmission('slow-still-refused-' + index);
        assert.equal(refused.second_attempt.status, 429);
        break;
      }
      assert.equal(policy, 'cooldown');
      const waitMs = Math.max(0, current.value.risk.usage.next_expiry_ms - Date.now()) + 300;
      assert.ok(waitMs <= 31_000);
      await delay(waitMs);
      refused.after_wait = await status();
      assert.ok(refused.after_wait.value.risk.usage.charged_generations < current.value.risk.reservation_limit);
      refused.recovered_automatically = true;
    }
    const step = { index, role: 'slow-' + index, started_at: new Date().toISOString() };
    evidence.steps.push(step);
    const peer = await boundedPeer(endpoint.replace(/^http/, 'ws') + '/ws?role=' + step.role, token,
      { payload: 'xorshift32-v1', stop_after_frames: FRAMES });
    clients.push({ peer, step });
    step.peer = peer.ready;
    step.burst = await call('burst', { role: step.role, frames: FRAMES, close: false });
    assert.equal(step.burst.status, 200, JSON.stringify(step.burst));
    if (disposition === 'abort') step.terminal = await peer.abort();
    if (disposition === 'consume') {
      step.drain = await peer.resume();
      assert.equal(step.drain.frames, FRAMES);
    }
    step.retirement = await call('risk-retire', { role: step.role });
    assert.equal(step.retirement.status, 200, JSON.stringify(step.retirement));
    if (index === 0) {
      step.old_generation_resubmit = await call('burst', { role: step.role, frames: 1, close: false });
      assert.equal(step.old_generation_resubmit.status, 400);
    }
    if (index % 4 === 0) step.healthy = await sentinel.sample();
    step.status = await status();
    console.log(JSON.stringify({ run, policy, disposition, step: index + 1,
      submitted_mib: (index + 1) * FRAMES / 16,
      charged: step.status.value.risk.usage.charged_generations,
      expired: step.status.value.risk.usage.expired_generations,
      sockets: step.status.value.sockets.length }));
    await save();
  }
  evidence.before_final_hold = await status();
  await delay(FINAL_HOLD_MS);
  evidence.after_final_hold = await status();
  evidence.final_healthy = await sentinel.sample();
  evidence.final_sibling = await sibling.sample();
  evidence.events_before_drain = await call('events');
  evidence.result = 'completed';
} catch (error) {
  evidence.result = 'stopped_on_error';
  evidence.error = String(error.stack ?? error);
  process.exitCode = 1;
  console.error(JSON.stringify({ run, error: evidence.error }));
} finally {
  // Observe outstanding old data only after the policy and healthy-peer measurements.
  // This intervention cannot establish a passive transport-release deadline.
  for (let i = 0; i < clients.length; i += 4) {
    await Promise.all(clients.slice(i, i + 4).map(async ({ peer, step }) => {
      if (disposition === 'hold') {
        step.resumed_at = new Date().toISOString();
        step.hold_after_send_ms = Date.now() - Date.parse(step.burst?.at ?? step.started_at);
        try { step.drain = await peer.resume(); } catch (error) { step.drain_error = String(error); }
      }
      await peer.terminate();
    }));
    await save();
  }
  try { evidence.final_status = await status(); evidence.events_after_cleanup = await call('events'); } catch {}
  healthy.forEach(peer => peer.terminate());
  evidence.finished_at = new Date().toISOString();
  evidence.cumulative_submitted_bytes = evidence.steps.filter(step => step.burst?.status === 200).length * FRAMES * FRAME_BYTES;
  evidence.drained_frames = evidence.steps.reduce((sum, step) => sum + (step.drain?.frames ?? 0), 0);
  await save();
  console.log(JSON.stringify({ run, result: evidence.result, submitted_mib: evidence.cumulative_submitted_bytes / 1048576,
    drained_mib: evidence.drained_frames / 16, refusals: evidence.refusals.length, evidence: output.pathname }));
}
