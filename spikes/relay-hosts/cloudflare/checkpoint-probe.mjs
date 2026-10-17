import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { setTimeout as delay } from 'node:timers/promises';
import WebSocket from 'ws';
import { sourceState } from './checkpoint-evidence.mjs';

const base = process.argv[2]?.replace(/\/$/, '');
const selected = process.argv[3] ?? 'all';
if (!base) throw new Error('Usage: node checkpoint-probe.mjs <URL> [all|hibernate|attachment-loss|disconnect|reset]');
const token = process.env.PROBE_TOKEN
  ?? (await readFile(new URL('./.evidence/token', import.meta.url), 'utf8')).trim();
const headers = { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json' };
const FRAME_BYTES = 1024;
const WAIT_MS = 10_000;
const evidence = { started_at: new Date().toISOString(), base, frame_bytes: FRAME_BYTES,
  fixture_clock: 'frozen', physical_release_proven: false, source: await sourceState(), cases: [] };

async function until(test, label, timeout = WAIT_MS) {
  const deadline = performance.now() + timeout;
  while (!test()) {
    if (performance.now() >= deadline) throw new Error('timeout: ' + label);
    await delay(20);
  }
}
function control(magic, frames, bytes, session = 1) {
  const value = Buffer.alloc(32);
  value.write(magic);
  value[4] = 2;
  value.writeBigUInt64BE(BigInt(session), 8);
  value.writeBigUInt64BE(BigInt(frames), 16);
  value.writeBigUInt64BE(BigInt(bytes), 24);
  return value;
}
function opaque(sequence, session = 1) {
  const value = Buffer.alloc(FRAME_BYTES, 0x5a);
  value.write('WS2O'); value[4] = 2; value.fill(0, 5, 8);
  value.writeBigUInt64BE(BigInt(session), 8);
  value.writeUInt32BE(FRAME_BYTES - 20, 16);
  value.writeBigUInt64BE(BigInt(sequence), 20);
  return value;
}
function admitted(session = 1) {
  const value = Buffer.alloc(16);
  value.write('WS2M'); value[4] = 2; value.writeBigUInt64BE(BigInt(session), 8);
  return value;
}
function usage(status, id = 2) {
  return status.generations?.find(row => row.connection === 'c-' + id + '-1');
}
async function scenario(mode, kind) {
  const run = 'checkpoint-' + randomUUID();
  const endpoint = base + '/checkpoint/' + run;
  const result = { run, mode, kind, endpoint };
  const peers = [];
  async function request(action, body) {
    const response = await fetch(endpoint + '/' + action, {
      method: body === undefined ? 'GET' : 'POST', headers,
      body: body === undefined || Object.keys(body).length === 0 ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(WAIT_MS),
    });
    if (!response.ok) throw new Error(action + ': ' + response.status + ' ' + await response.text());
    const text = await response.text();
    try { return JSON.parse(text); } catch { return text; }
  }
  async function connect(role) {
    const socket = new WebSocket(endpoint.replace(/^http/, 'ws') + '/ws?role=' + role,
      { headers, handshakeTimeout: WAIT_MS });
    const peer = { socket, role, messages: [], binary: [], closed: false, errors: [] };
    peers.push(peer);
    socket.on('message', (value, binary) => {
      if (binary) peer.binary.push(Buffer.from(value));
      else {
        try { peer.messages.push(JSON.parse(value.toString())); }
        catch { peer.messages.push(value.toString()); }
      }
    });
    socket.on('close', (code, reason) => { peer.closed = true; peer.close = { code, reason: reason.toString() }; });
    socket.on('error', error => peer.errors.push(error.message));
    await new Promise((resolve, reject) => { socket.once('open', resolve); socket.once('error', reject); });
    return peer;
  }
  async function ping(peer, expected = true) {
    const count = peer.messages.length;
    peer.socket.send('ping');
    await until(() => peer.messages.length > count, 'ping response');
    assert.equal(peer.messages.slice(count).includes('pong'), expected);
  }
  function settle(peer) {
    for (const frame of peer.binary.filter(frame => frame.toString('ascii', 0, 4) === 'WS2N')) {
      const ack = Buffer.from(frame); ack.write('WS2S'); peer.socket.send(ack);
    }
  }
  try {
    assert.equal((await request('configure', { mode })).ready, true);
    const sender = await connect('sender');
    await until(() => sender.messages.some(value => value.event === 'registered'), 'registration');
    const receiver = await connect('receiver');
    await until(() => receiver.messages.some(value => value.event === 'joined'), 'join');
    receiver.socket.send(opaque(0));
    await until(() => sender.binary.some(value => value.toString('ascii', 0, 4) === 'WS2O'), 'receiver bootstrap');
    sender.socket.send(admitted());
    sender.socket.send(control('WS2B', 1, FRAME_BYTES));
    await ping(sender);
    const before = await request('status');
    sender.socket.send(opaque(0));
    sender.socket.send(opaque(1));
    await until(() => receiver.binary.filter(value => value.toString('ascii', 0, 4) === 'WS2O').length === 2, 'forwarded frames');
    // Client receipt can precede event-end flushing; the following event establishes the observation boundary.
    const transferred = await request('status');
    assert.equal(transferred.last_refusal, null, JSON.stringify(transferred));
    assert.equal(usage(transferred).submitted_frames, 2);
    assert.equal(usage(transferred).submitted_bytes, 2 * FRAME_BYTES);
    assert.deepEqual(receiver.binary.filter(value => value.toString('ascii', 0, 4) === 'WS2O'), [opaque(0), opaque(1)]);
    result.before_fault = transferred;
    result.data_event_sql_writes = transferred.writes - before.writes;
    if (mode === 'attachments') assert.equal(result.data_event_sql_writes, 0);
    else assert.equal(result.data_event_sql_writes, 4);

    if (kind === 'hibernate') {
      let woke = transferred;
      // No HTTP polling, heartbeat, Alarm or timer runs inside the object during idle.
      for (let attempt = 0; attempt < 3 && woke.boot === transferred.boot; attempt++) {
        await delay(15_000);
        woke = await request('status');
      }
      result.natural_hibernation = woke.boot !== transferred.boot;
      result.after_idle = woke;
      assert.equal(woke.ready, true);
      assert.equal(usage(woke).submitted_frames, 2);
      if (!base.includes('127.0.0.1') && !base.includes('localhost')) {
        assert.equal(result.natural_hibernation, true, 'Cloudflare did not demonstrate hibernation');
      }
      result.after_reconstruction = await request('reconstruct', {});
      assert.notEqual(result.after_reconstruction.boot, woke.boot);
      assert.equal(usage(result.after_reconstruction).submitted_frames, 2);
      sender.socket.send(opaque(2));
      await until(() => receiver.binary.filter(value => value.toString('ascii', 0, 4) === 'WS2O').length === 3, 'post-wake forwarding');
      receiver.socket.send(control('WS2B', 3, 3 * FRAME_BYTES));
      await ping(receiver);
      assert.equal(usage(await request('status')).submitted_frames, 0);
      // A fourth, unconsumed frame exercises ordered WS2N/WS2S settlement after recovery.
      sender.socket.send(opaque(3));
      await until(() => receiver.binary.filter(value => value.toString('ascii', 0, 4) === 'WS2O').length === 4, 'retirement payload');
      await request('cancel', {});
      await until(() => peers.every(peer => peer.binary.some(frame => frame.toString('ascii', 0, 4) === 'WS2N')), 'settlement notices');
      result.awaiting_settlement = await request('reconstruct', {});
      assert.equal(usage(result.awaiting_settlement).submitted_frames, 1);
      peers.forEach(settle);
      await ping(sender);
      await until(() => receiver.closed, 'settled receiver closes');
      result.after_settlement = await request('status');
      assert.equal(usage(result.after_settlement)?.submitted_frames ?? 0, 0);
      result.healthy_sender_progressed = true;
    } else if (kind === 'attachment-loss') {
      await request('lose-attachment', {});
      result.after_reconstruction = await request('reconstruct', {});
      const recoverable = mode === 'sqlite_mirror';
      assert.equal(result.after_reconstruction.ready, recoverable);
      if (recoverable) assert.equal(usage(result.after_reconstruction).submitted_frames, 2);
      else assert.match(result.after_reconstruction.reason, /MissingResponsibility/);
      await ping(sender, recoverable);
      result.healthy_sender_progressed = recoverable;
    } else if (kind === 'disconnect') {
      receiver.socket.close();
      await until(() => receiver.closed, 'receiver closed');
      await delay(250);
      result.after_reconstruction = await request('reconstruct', {});
      assert.equal(result.after_reconstruction.ready, true);
      assert.equal(usage(result.after_reconstruction).submitted_frames, 2);
      assert.equal(usage(result.after_reconstruction).communicating, false);
      await ping(sender);
      settle(sender);
      await ping(sender);
      result.after_sender_settlement = await request('status');
      assert.equal(usage(result.after_sender_settlement).submitted_frames, 2);
      result.receiver_settlement_available = false;
      const replacement = await connect('receiver');
      await until(() => replacement.messages.some(value => value.event === 'joined'), 'replacement joined');
      const session = replacement.messages.find(value => value.event === 'joined').session;
      assert.equal(session, 2, 'wire session high-water mark survived reconstruction');
      replacement.socket.send(opaque(0, session));
      await until(() => sender.binary.some(frame => frame.toString('ascii', 0, 4) === 'WS2O'
        && frame.readBigUInt64BE(8) === 2n), 'replacement bootstrap');
      sender.socket.send(admitted(session));
      sender.socket.send(control('WS2B', 1, FRAME_BYTES, session));
      sender.socket.send(opaque(0, session));
      await until(() => replacement.binary.some(frame => frame.toString('ascii', 0, 4) === 'WS2O'), 'replacement forward');
      replacement.socket.send(control('WS2B', 1, FRAME_BYTES, session));
      await ping(replacement);
      // A new transport cannot acknowledge the missing generation by replaying its wire ID.
      replacement.socket.send(control('WS2S', 2, 2 * FRAME_BYTES));
      await ping(replacement);
      result.after_replacement = await request('status');
      assert.equal(usage(result.after_replacement).submitted_frames, 2);
      assert.equal(result.after_replacement.generations.find(row => row.connection === 'c-2-2').submitted_frames, 0);
      result.old_generation_retained_after_replayed_settlement = true;
      await ping(sender);
      result.healthy_sender_progressed = true;
    } else if (kind === 'reset') {
      try { await request('reset', {}); } catch (error) { result.reset_response = error.message; }
      await until(() => peers.every(peer => peer.closed), 'reset disconnects sockets');
      result.after_reset = await request('status');
      const recoverable = mode === 'sqlite_mirror';
      assert.equal(result.after_reset.ready, recoverable);
      if (recoverable) {
        assert.equal(usage(result.after_reset).submitted_frames, 2);
        assert.equal(result.after_reset.sockets, 0);
        result.after_second_reconstruction = await request('reconstruct', {});
        assert.equal(usage(result.after_second_reconstruction).submitted_frames, 2);
        assert.equal(result.after_second_reconstruction.kernel_deadline_ms, result.after_reset.kernel_deadline_ms);
      } else assert.match(result.after_reset.reason, /MissingResponsibility/);
      result.healthy_sender_disconnected = sender.closed;
      result.receiver_settlement_available = false;
    } else throw new Error('unknown scenario ' + kind);
    result.passed = true;
    return result;
  } catch (error) {
    result.failure = error.stack;
    try { result.failure_status = await request('status'); } catch {}
    throw Object.assign(error, { result });
  } finally {
    result.peers = peers.map(({ role, closed, close, errors }) => ({ role, closed, close, errors }));
    peers.forEach(peer => peer.socket.terminate());
  }
}
try {
  const kinds = selected === 'all' ? ['hibernate', 'attachment-loss', 'disconnect', 'reset'] : [selected];
  for (const mode of ['attachments', 'sqlite_mirror']) {
    for (const kind of kinds) {
      console.log('Running', mode, kind);
      evidence.cases.push(await scenario(mode, kind));
      console.log('Passed', mode, kind);
    }
  }
} catch (error) {
  if (error.result) evidence.cases.push(error.result);
  evidence.failure = error.stack;
  process.exitCode = 1;
} finally {
  evidence.finished_at = new Date().toISOString();
  if ((await sourceState()).sha256 !== evidence.source.sha256) {
    evidence.failure = 'Source changed during this run; evidence does not identify one source state';
    process.exitCode = 1;
  }
  const path = new URL('./.evidence/checkpoint-' + Date.now() + '.json', import.meta.url);
  await mkdir(new URL('./.evidence/', import.meta.url), { recursive: true });
  await writeFile(path, JSON.stringify(evidence, null, 2) + '\n');
  console.log('Evidence:', path.pathname);
  if (evidence.failure) console.error(evidence.failure);
}
