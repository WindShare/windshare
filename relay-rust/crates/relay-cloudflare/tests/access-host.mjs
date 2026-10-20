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
    if (path === '/__test/turn-retry') {
      for (const row of this.ctx.storage.sql.exec("SELECT key,value FROM access_entries WHERE key LIKE 'turn_lease:%'")) {
        const lease = JSON.parse(row.value);
        if (lease.state.state === 'revocation_pending') {
          lease.state.next_attempt = 0;
          this.ctx.storage.sql.exec('UPDATE access_entries SET value=? WHERE key=?', JSON.stringify(lease), row.key);
        }
      }
      return new Response('ok');
    }
    const response = await this.rust.fetch(request);
    if (this.env.TURN_TEST_LIFETIME_MS && response.status === 201 &&
        (path === '/api/turn/issue' || path === '/api/turn/renew')) {
      const grant = await response.json();
      grant.expires_at = Date.now() + Number(this.env.TURN_TEST_LIFETIME_MS);
      return Response.json(grant, { status: response.status, headers: response.headers });
    }
    return response;
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
