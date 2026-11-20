export default {
  fetch(request, env) {
    const url = new URL(request.url);
    if (url.origin !== env.CONSOLE_ORIGIN || url.pathname.startsWith('/internal/')) return new Response('not found', { status: 404 });
    // Route through the stateless relay Worker edge so static console assets never wake AccessObject.
    return env.RELAY.fetch(request);
  },
};
