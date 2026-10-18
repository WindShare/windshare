import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import { setTimeout as delay } from 'node:timers/promises';
import { boundedPeer } from './bounded-peer.mjs';
import { request, healthyPeer } from './pressure-client.mjs';

const base = process.argv[2]?.replace(/\/$/, '');
assert.ok(base?.startsWith('https://'));
const token = process.env.PROBE_TOKEN ??
  (await readFile(new URL('./.evidence/token', import.meta.url), 'utf8')).trim();
const run = 'risk-recovery-' + randomUUID();
const endpoint = base + '/pressure/' + run;
const FRAMES = 128;
const QUIET_MS = 15_000;
const evidence = { run, endpoint, started_at: new Date().toISOString(),
  scope: 'experimental policy persistence; not production kernel integration',
  physical_release_proven: false, steps: [] };
const peers = [];
let healthy;
const call = (action, body) => request(endpoint, token, action, body);
async function status() {
  const response = await call('status');
  assert.equal(response.status, 200, JSON.stringify(response));
  return response;
}
try {
  assert.equal((await call('risk-configure', { policy: 'cooldown' })).status, 200);
  for (let i = 0; i < 4; i++) {
    const role = 'slow-' + i;
    const peer = await boundedPeer(endpoint.replace(/^http/, 'ws') + '/ws?role=' + role,
      token, { payload: 'xorshift32-v1', stop_after_frames: FRAMES });
    peers.push(peer);
    const burst = await call('burst', { role, frames: FRAMES, close: false });
    assert.equal(burst.status, 200);
    const retirement = await call('risk-retire', { role });
    assert.equal(retirement.status, 200);
    evidence.steps.push({ role, peer: peer.ready, burst, retirement });
  }
  evidence.before_idle = await status();
  const originalExpiry = evidence.before_idle.value.risk.usage.next_expiry_ms;
  await delay(QUIET_MS);
  evidence.after_idle = await status();
  evidence.natural_hibernation = evidence.before_idle.value.rust_boot !== evidence.after_idle.value.rust_boot;
  assert.equal(evidence.after_idle.value.risk.usage.charged_generations, 4);
  assert.equal(evidence.after_idle.value.risk.usage.next_expiry_ms, originalExpiry);
  evidence.repeated_retirement = await call('risk-retire', { role: 'slow-0' });
  assert.equal(evidence.repeated_retirement.value.usage.next_expiry_ms, originalExpiry);
  evidence.reset = await call('reset', {});
  evidence.after_reset = await status();
  assert.notEqual(evidence.after_reset.value.rust_boot, evidence.after_idle.value.rust_boot);
  assert.equal(evidence.after_reset.value.risk.usage.charged_generations, 4);
  assert.equal(evidence.after_reset.value.risk.usage.next_expiry_ms, originalExpiry);
  assert.equal(evidence.after_reset.value.reserved_frames, 4 * FRAMES);
  await delay(Math.max(0, originalExpiry - Date.now()) + 300);
  evidence.after_expiry = await status();
  assert.ok(evidence.after_expiry.value.risk.usage.charged_generations < 4);
  const replacement = await boundedPeer(endpoint.replace(/^http/, 'ws') + '/ws?role=slow-new',
    token, { payload: 'xorshift32-v1', stop_after_frames: FRAMES });
  peers.push(replacement);
  evidence.replacement_burst = await call('burst', { role: 'slow-new', frames: FRAMES, close: false });
  assert.equal(evidence.replacement_burst.status, 200);
  evidence.replacement_drain = await replacement.resume();
  assert.equal(evidence.replacement_drain.frames, FRAMES);
  assert.equal((await call('risk-retire', { role: 'slow-new' })).status, 200);
  evidence.stale_submit = await call('burst', { role: 'slow-0', frames: 1, close: false });
  assert.equal(evidence.stale_submit.status, 400);
  healthy = await healthyPeer(endpoint, token, 'healthy-recovered');
  evidence.healthy_after_recovery = await healthy.sample();
  evidence.final_status = await status();
  evidence.events = await call('events');
  evidence.result = 'completed';
} catch (error) {
  evidence.result = 'stopped_on_error';
  evidence.error = String(error.stack ?? error);
  process.exitCode = 1;
} finally {
  healthy?.terminate();
  for (const peer of peers) await peer.terminate();
  evidence.finished_at = new Date().toISOString();
  const output = new URL('./.evidence/' + run + '.json', import.meta.url);
  await writeFile(output, JSON.stringify(evidence, null, 2) + '\n');
  console.log(JSON.stringify({ run, result: evidence.result, hibernated: evidence.natural_hibernation,
    error: evidence.error, evidence: output.pathname }));
}
