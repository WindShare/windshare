// Local-only fault injection. Production entrypoints never import this module.
import { DurableObject } from 'cloudflare:workers';
import { AccessObject as RustAccess, ShareRelayObject as RustShare } from '../build/index.js';
export { default } from '../entry.mjs';

export class AccessObject extends DurableObject {
  constructor(ctx, env) { super(ctx, env); this.rust = new RustAccess(ctx, env); }
  async fetch(request) {
    const path = new URL(request.url).pathname;
    if (path === '/__test/queue') {
      return Response.json([...this.ctx.storage.sql.exec(
        'SELECT credential,share,next_attempt FROM termination_queue ORDER BY next_attempt,credential,share')]);
    }
    if (path === '/__test/alarm') {
      await this.ctx.storage.deleteAlarm();
      return this.rust.alarm();
    }
    return this.rust.fetch(request);
  }
  // The test advances delivery explicitly, without wall-clock sleeps.
  async alarm() {}
}

export class ShareRelayObject extends DurableObject {
  constructor(ctx, env) { super(ctx, env); this.rust = new RustShare(ctx, env); }
  async fetch(request) {
    const path = new URL(request.url).pathname;
    if (path === '/__test/fail-termination') {
      await this.ctx.storage.put('fail_termination', true);
      return new Response('ok');
    }
    if (path === '/internal/terminate' && await this.ctx.storage.get('fail_termination')) {
      return new Response('injected target failure', { status: 503 });
    }
    return this.rust.fetch(request);
  }
  alarm(...args) { return this.rust.alarm(...args); }
  webSocketMessage(...args) { return this.rust.webSocketMessage(...args); }
  webSocketClose(...args) { return this.rust.webSocketClose(...args); }
  webSocketError(...args) { return this.rust.webSocketError(...args); }
}
