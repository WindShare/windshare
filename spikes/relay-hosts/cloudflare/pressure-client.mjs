import assert from 'node:assert/strict';
import { randomBytes } from 'node:crypto';
import { setTimeout as delay } from 'node:timers/promises';
import WebSocket from 'ws';

const FRAME_BYTES = 65_536;
const ECHO_FRAMES = 16;
const PING_INTERVAL_MS = 1000;
const RESPONSE_TIMEOUT_MS = 5000;

export async function request(endpoint, token, action, body) {
  const start = performance.now();
  const response = await fetch(endpoint + '/' + action, {
    method: body === undefined ? 'GET' : 'POST',
    headers: { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(RESPONSE_TIMEOUT_MS),
  });
  const text = await response.text();
  let value;
  try { value = JSON.parse(text); } catch { value = text.slice(0, 300); }
  return { status: response.status, value, elapsed_ms: performance.now() - start,
    at: new Date().toISOString(), ray: response.headers.get('cf-ray') };
}

export async function healthyPeer(endpoint, token, role) {
  const pending = new Map();
  const record = { role, connected_at: new Date().toISOString(), pings: [], transfers: [] };
  const socket = new WebSocket(endpoint.replace(/^http/, 'ws') + '/ws?role=' + role, {
    headers: { Authorization: 'Bearer ' + token }, perMessageDeflate: false,
    handshakeTimeout: RESPONSE_TIMEOUT_MS, maxPayload: FRAME_BYTES,
  });
  socket.on('error', error => { record.error = String(error); });
  socket.on('close', (code, reason) => {
    record.closed = { code, reason: reason.toString(), at: new Date().toISOString() };
  });
  await new Promise((resolve, reject) => { socket.once('open', resolve); socket.once('error', reject); });
  record.extensions = socket.extensions;
  let serial = 0;
  let echo;
  socket.on('message', (data, binary) => {
    if (binary) {
      if (echo) {
        if (!data.equals(echo.payload)) echo.error = 'echo integrity mismatch';
        echo.frames++;
      }
      return;
    }
    const id = data.toString().replace(/^pong:/, '');
    const waiting = pending.get(id);
    if (waiting) {
      clearTimeout(waiting.timer);
      pending.delete(id);
      waiting.resolve(performance.now() - waiting.start);
    }
  });
  function ping() {
    const id = String(++serial);
    return new Promise((resolve, reject) => {
      if (socket.readyState !== WebSocket.OPEN) return reject(new Error('healthy socket closed'));
      const timer = setTimeout(() => { pending.delete(id); reject(new Error('pong timeout')); }, RESPONSE_TIMEOUT_MS);
      pending.set(id, { resolve, reject, timer, start: performance.now() });
      socket.send('ping:' + id);
    });
  }
  let pingInFlight = false;
  const timer = setInterval(async () => {
    if (pingInFlight || socket.readyState !== WebSocket.OPEN) return;
    pingInFlight = true;
    const sample = { at: new Date().toISOString() };
    try { sample.ms = await ping(); } catch (error) { sample.error = String(error); }
    record.pings.push(sample);
    pingInFlight = false;
  }, PING_INTERVAL_MS);
  return {
    record,
    async sample() {
      const rtt = await ping();
      echo = { payload: randomBytes(FRAME_BYTES), frames: 0 };
      const start = performance.now();
      for (let i = 0; i < ECHO_FRAMES; i++) socket.send(echo.payload);
      while (echo.frames < ECHO_FRAMES && !echo.error) {
        assert.equal(socket.readyState, WebSocket.OPEN, 'healthy connection lost during transfer');
        if (performance.now() - start > RESPONSE_TIMEOUT_MS) throw new Error('healthy transfer timeout');
        await delay(5);
      }
      assert.equal(echo.error, undefined);
      const elapsed = performance.now() - start;
      const value = { at: new Date().toISOString(), rtt_ms: rtt,
        echo_bytes: FRAME_BYTES * ECHO_FRAMES, elapsed_ms: elapsed,
        echoed_mib_per_second: (FRAME_BYTES * ECHO_FRAMES / 1048576) / (elapsed / 1000) };
      record.transfers.push(value);
      echo = undefined;
      return value;
    },
    terminate() {
      clearInterval(timer);
      for (const value of pending.values()) {
        clearTimeout(value.timer);
        value.reject(new Error('probe cleanup'));
      }
      pending.clear();
      socket.terminate();
    },
  };
}
