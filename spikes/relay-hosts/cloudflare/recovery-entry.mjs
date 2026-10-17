// Isolate runtime abort from Rust/Wasm futures without duplicating relay policy.
import { DurableObject } from 'cloudflare:workers';
import RustWorker, { CloseProbe as RustCloseProbe } from './build/index.js';

export { RustCloseProbe as CloseProbe };

const foreignErrors = [];
addEventListener('error', event => {
  if (foreignErrors.length < 16) foreignErrors.push({
    name: event.error?.name, message: event.error?.message ?? event.message,
  });
});

export class ResetControl extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env);
    this.boot = crypto.randomUUID();
  }

  async fetch(request) {
    const action = new URL(request.url).pathname.split('/').at(-1);
    if (request.method === 'GET' && action === 'status') {
      return Response.json({ boot: this.boot, sockets: [], events: [],
        reset_issued: (await this.ctx.storage.get('reset-issued')) ?? false,
        physical_release_proven: false });
    }
    if (request.method === 'POST' && action === 'reset') {
      if (await this.ctx.storage.get('reset-issued')) return new Response('already reset', { status: 409 });
      await this.ctx.storage.put('reset-issued', true);
      await this.ctx.storage.sync();
      this.ctx.abort('JavaScript control reset', { retryAlarm: false });
    }
    return new Response('not found', { status: 404 });
  }
}

export class BridgeProbe extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env);
    this.boot = crypto.randomUUID();
    this.rust = new RustCloseProbe(ctx, env);
  }

  async fetch(request) {
    const url = new URL(request.url);
    const action = url.pathname.split('/').at(-1);
    if (request.method === 'GET' && action === 'outer-status') {
      return Response.json({ boot: this.boot, sockets: this.ctx.getWebSockets().length, errors: foreignErrors });
    }
    if (request.method === 'POST' && action === 'reset' && !url.pathname.startsWith('/bridge-raw/')) {
      // Finish the Rust future and its storage barrier before interrupting execution.
      url.pathname = url.pathname.replace(/\/reset$/, '/prepare-reset');
      const prepared = await this.rust.fetch(new Request(url, request));
      if (!prepared.ok) return prepared;
      this.ctx.abort('reset after Rust future completion', { retryAlarm: false });
    }
    return this.rust.fetch(request);
  }

  alarm(...args) { return this.rust.alarm(...args); }
  webSocketMessage(...args) { return this.rust.webSocketMessage(...args); }
  webSocketClose(...args) { return this.rust.webSocketClose(...args); }
  webSocketError(...args) { return this.rust.webSocketError(...args); }
}

export default class RecoveryEntrypoint extends RustWorker {
  async fetch(request) {
    const segments = new URL(request.url).pathname.split('/').filter(Boolean);
    if (!this.env.PROBE_TOKEN || request.headers.get('Authorization') !== 'Bearer ' + this.env.PROBE_TOKEN) {
      return new Response('unauthorized', { status: 401 });
    }
    if (segments.length !== 3 || !/^[A-Za-z0-9-]{1,64}$/.test(segments[1])) {
      return new Response('invalid probe route', { status: 400 });
    }
    if (segments[0] === 'probe') return super.fetch(request);
    const binding = { direct: 'CLOSE_PROBE', bridge: 'BRIDGE_PROBE', 'bridge-raw': 'BRIDGE_PROBE', control: 'RESET_CONTROL' }[segments[0]];
    if (!binding) return new Response('unknown probe', { status: 404 });
    return this.env[binding].getByName(segments[1]).fetch(request);
  }
}
