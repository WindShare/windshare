export default {
  fetch(request, env) {
    const url = new URL(request.url);
    if (url.origin !== env.CONSOLE_ORIGIN || url.pathname.startsWith('/internal/')) return new Response('not found', { status: 404 });
    return env.ACCESS.getByName('authority').fetch(request);
  },
};
