import { serveDirectory } from './directory-mirror.mjs';

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
    if (url.pathname === '/.well-known/windshare-network') return serveDirectory(request, env);
    return env.ASSETS.fetch(request);
  },
};
