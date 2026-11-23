// Local-only fault injection. Production entrypoints never import this module.
import { DurableObject } from 'cloudflare:workers';
import { AccessObject as RustAccess, ShareRelayObject as RustShare } from '../build/index.js';
export { default } from '../entry.mjs';

export class AccessObject extends DurableObject {
  constructor(ctx, env) { super(ctx, env); this.rust = new RustAccess(ctx, env); this.fetches = 0; }
  async fetch(request) {
    const path = new URL(request.url).pathname;
    if (path === '/__test/fetches') {
      return Response.json({ fetches: this.fetches });
    }
    if (path === '/__test/queue') {
      return Response.json([...this.ctx.storage.sql.exec(
        'SELECT credential,share,next_attempt FROM termination_queue ORDER BY next_attempt,credential,share')]);
    }
    if (path === '/__test/alarm') {
      await this.ctx.storage.deleteAlarm();
      return this.rust.alarm();
    }
    if (path === '/__test/access-entries' && request.method === 'POST') {
      const entries = await request.json();
      this.ctx.storage.transactionSync(() => {
        for (const { key, value } of entries) {
          this.ctx.storage.sql.exec('INSERT INTO access_entries (key,value) VALUES (?,?)', key, JSON.stringify(value));
        }
      });
      return new Response('ok');
    }
    this.fetches++;
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
    const response = await this.rust.fetch(request);
    if (response.status === 400) console.error(JSON.stringify({ event: 'test_route_refused', endpoint: request.url, reason: await response.clone().text() }));
    return response;
  }
  alarm(...args) { return this.rust.alarm(...args); }
  webSocketMessage(...args) { return this.rust.webSocketMessage(...args); }
  webSocketClose(...args) { return this.rust.webSocketClose(...args); }
  webSocketError(...args) { return this.rust.webSocketError(...args); }
}
