const RELEASES = 'https://github.com/WindShare/windshare/releases';
// Published archives carry exact release tags; immutable versioned routes match CN static hosting 1:1.
const RELEASE_VERSION = /^v[0-9]+\.[0-9]+\.[0-9]+$/;
const ASSET_NAME = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,179}$/;

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname.startsWith('/downloads/')) {
      if (request.method !== 'GET' && request.method !== 'HEAD') return new Response(null, { status: 405, headers: { Allow: 'GET, HEAD' } });
      const parts = url.pathname.slice('/downloads/'.length).split('/');
      const [version, asset] = parts;
      if (parts.length !== 2 || !RELEASE_VERSION.test(version) || !ASSET_NAME.test(asset) || ['.', '..'].includes(asset)) {
        return new Response(null, { status: 404 });
      }
      return new Response(null, {
        status: 302,
        headers: { Location: `${RELEASES}/download/${version}/${asset}`, 'Cache-Control': 'public, max-age=300' },
      });
    }
    if (url.pathname === '/.well-known/windshare-network') {
      // Missing directory exports must not become a successful SPA response.
      const headers = { 'Access-Control-Allow-Origin': '*', 'Cache-Control': 'public, max-age=300', 'Content-Type': 'application/json' };
      const response = await env.ASSETS.fetch(request);
      if (!response.ok || response.headers.get('content-type')?.includes('text/html')) return new Response(null, { status: 503, headers: { ...headers, 'Retry-After': '30' } });
      const publicHeaders = new Headers(response.headers);
      for (const [name, value] of Object.entries(headers)) publicHeaders.set(name, value);
      return new Response(response.body, { status: response.status, headers: publicHeaders });
    }
    return env.ASSETS.fetch(request);
  },
};
