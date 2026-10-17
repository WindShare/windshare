import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { setTimeout as delay } from 'node:timers/promises';
import { boundedPeer } from './bounded-peer.mjs';
import { healthyPeer, request } from './pressure-client.mjs';

const base = process.argv[2]?.replace(/\/$/, '');
const mode = process.argv[3];
const steps = Number(process.argv[4] ?? 32);
const frames = Number(process.argv[5] ?? 256);
const holdMs = Number(process.argv[6] ?? 30_000);
const calibrationKind = process.argv[7] ?? 'heap';
const FRAME_BYTES = 65_536;
const MAX_RUN_MS = 12 * 60_000;
const DRAIN_CONCURRENCY = 4;
assert.ok(base?.startsWith('https://'));
assert.ok(['live', 'close', 'reset', 'calibration'].includes(mode));
assert.ok(Number.isInteger(steps) && steps >= 1 && steps <= 32);
assert.ok(Number.isInteger(frames) && frames >= 1 && frames <= 256);
assert.ok(Number.isInteger(holdMs) && holdMs >= 0 && holdMs <= 60_000);
const token = process.env.PROBE_TOKEN ??
  (await readFile(new URL('./.evidence/token', import.meta.url), 'utf8')).trim();
const run = mode + '-' + randomUUID();
const endpoint = base + '/pressure/' + run;
const evidence = { run, endpoint, mode, started_at: new Date().toISOString(),
  limits: { steps, frames_per_step: frames, frame_bytes: FRAME_BYTES, hold_ms: holdMs, max_run_ms: MAX_RUN_MS },
  payload: 'xorshift32-v1', compression_requested: false,
  calibration_kind: mode === 'calibration' ? calibrationKind : undefined, steps: [], healthy_peers: [],
  physical_release_proven: false };
const clients = [];
const healthy = [];
const start = performance.now();
const call = (action, body) => request(endpoint, token, action, body);
const output = new URL('./.evidence/pressure-' + run + '.json', import.meta.url);

async function save() {
  await mkdir(new URL('./.evidence/', import.meta.url), { recursive: true });
  await writeFile(output, JSON.stringify(evidence, null, 2) + '\n');
}
async function connectHealthy(role, target = endpoint) {
  const peer = await healthyPeer(target, token, role);
  healthy.push(peer);
  evidence.healthy_peers.push(peer.record);
  return peer;
}
async function status() {
  const response = await call('status');
  assert.equal(response.status, 200, JSON.stringify(response));
  return response;
}
async function drainClients() {
  // Resume only after all load and holding observations. Each drain actively changes
  // the system; its age is a lower bound on observed retention, never a release TTL.
  for (let index = 0; index < clients.length; index += DRAIN_CONCURRENCY) {
    await Promise.all(clients.slice(index, index + DRAIN_CONCURRENCY).map(async ({ peer, step }) => {
      step.resumed_at = new Date().toISOString();
      step.hold_after_send_ms = Date.now() - Date.parse(step.burst?.at ?? step.started_at);
      try { step.drain = await peer.resume(); }
      catch (error) { step.drain_error = String(error); }
      await peer.terminate();
    }));
    await save();
  }
}

try {
  evidence.initial_status = await status();
  let sentinel = await connectHealthy('healthy-0');
  const sibling = await connectHealthy('healthy-sibling', base + '/pressure/sibling-' + randomUUID());
  evidence.baseline = await sentinel.sample();
  evidence.sibling_baseline = await sibling.sample();
  if (mode === 'calibration') {
    for (let i = 0; i < 12; i++) {
      const step = { index: i, started_at: new Date().toISOString() };
      evidence.steps.push(step);
      try {
        step.allocation = await call('calibrate', { kind: calibrationKind });
        console.log(JSON.stringify({ run, allocation: step.allocation }));
        if (step.allocation.status !== 200) break;
        if (i > 0 && step.allocation.value.boot !== evidence.steps[0].allocation.value.boot) break;
        await delay(250);
      } catch (error) { step.error = String(error); break; }
    }
    evidence.calibration_recovery = await call('outer-status');
    evidence.release_calibration = await call('release-calibration', {});
  } else {
    for (let i = 0; i < steps; i++) {
      assert.ok(performance.now() - start < MAX_RUN_MS, 'run duration bound reached');
      const step = { index: i, role: 'slow-' + i, started_at: new Date().toISOString() };
      evidence.steps.push(step);
      const peer = await boundedPeer(endpoint.replace(/^http/, 'ws') + '/ws?role=' + step.role, token,
        { payload: 'xorshift32-v1', ...(mode === 'live' ? { stop_after_frames: frames } : {}) });
      step.peer = peer.ready;
      clients.push({ peer, step });
      step.burst = await call('burst', { role: step.role, frames, close: mode === 'close' });
      assert.equal(step.burst.status, 200, JSON.stringify(step.burst));
      if (mode === 'reset') {
        step.before_reset = await status();
        step.reset = await call('reset', {});
        step.after_reset = await status();
        assert.notEqual(step.after_reset.value.rust_boot, step.before_reset.value.rust_boot);
        step.healthy_closed_after_reset = sentinel.record.closed ?? null;
        sentinel.terminate();
        sentinel = await connectHealthy('healthy-' + (i + 1));
        step.replacement = await sentinel.sample();
      } else if ((i + 1) % 4 === 0 || i === 0) {
        step.healthy = await sentinel.sample();
      }
      step.status = await status();
      console.log(JSON.stringify({ run, step: i + 1, cumulative_mib: (i + 1) * frames / 16,
        sockets: step.status.value.sockets.length, rust_boot: step.status.value.rust_boot }));
      await save();
    }
    evidence.before_hold = await status();
    await delay(holdMs);
    evidence.after_hold = await status();
    evidence.final_healthy = await sentinel.sample();
    evidence.sibling_after_load = await sibling.sample();
    const newcomer = await connectHealthy('healthy-newcomer');
    evidence.new_connection = await newcomer.sample();
  }
  evidence.result = 'completed';
} catch (error) {
  evidence.result = 'stopped_on_error';
  evidence.error = String(error.stack ?? error);
  try { evidence.after_error = await call('status'); } catch (failure) { evidence.after_error = String(failure); }
  console.log(JSON.stringify({ run, result: evidence.result, error: evidence.error }));
} finally {
  if (clients.length) await drainClients();
  for (const peer of healthy) peer.terminate();
  for (const { peer } of clients) await peer.terminate();
  if (mode === 'calibration') {
    try { await call('release-calibration', {}); } catch {}
  }
  evidence.finished_at = new Date().toISOString();
  evidence.cumulative_send_returned_bytes = evidence.steps.filter(s => s.burst?.status === 200).length * frames * FRAME_BYTES;
  await save();
  console.log(JSON.stringify({ run, result: evidence.result, evidence: output.pathname,
    cumulative_send_returned_bytes: evidence.cumulative_send_returned_bytes,
    drained_frames: evidence.steps.reduce((sum, s) => sum + (s.drain?.frames ?? 0), 0) }));
}
