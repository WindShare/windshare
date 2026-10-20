import RustWorker from './build/index.js';
export { ShareRelayObject, AccessObject } from './build/index.js';
export default class RelayEntrypoint extends RustWorker {
  async fetch(request) {
    const url = new URL(request.url);
    if (url.origin === this.env.CONSOLE_ORIGIN || url.pathname === '/.well-known/windshare' || url.pathname.startsWith('/v2/') || ['/api/turn/capability', '/api/turn/issue', '/api/turn/renew', '/api/turn/revoke'].includes(url.pathname)) {
      return super.fetch(request);
    }
    if (url.pathname.startsWith('/api/') || url.pathname.startsWith('/internal/')) return new Response('not found', { status: 404 });
    if (this.env.ASSETS && request.method === 'GET') return this.env.ASSETS.fetch(request);
    return new Response('WindShare relay', { status: 404 });
  }
}
