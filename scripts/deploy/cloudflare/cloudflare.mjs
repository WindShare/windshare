const API_BASE = 'https://api.cloudflare.com/client/v4';
const REQUEST_TIMEOUT_MS = 30_000;
const WORKER_NOT_FOUND = 10007;

export function cloudflareClient({ accountId, workerName, apiToken, fetchImpl = fetch }) {
  if (!apiToken?.trim()) throw new Error('Set the CLOUDFLARE_API_TOKEN repository secret.');
  const accountPath = `/accounts/${accountId}/workers`;
  const workerPath = `${accountPath}/scripts/${workerName}`;
  async function request(path, { method = 'GET', data, allowMissing = false } = {}) {
    const response = await fetchImpl(API_BASE + path, {
      method,
      ...(data === undefined ? {} : { body: JSON.stringify(data) }),
      headers: { ...(data === undefined ? {} : { 'Content-Type': 'application/json' }), Authorization: `Bearer ${apiToken}` },
      signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      redirect: 'error',
    });
    let body;
    try { body = await response.json(); } catch {
      throw new Error(`Cloudflare returned invalid JSON (HTTP ${response.status}).`);
    }
    // A permission error, unknown account or transient failure is never proof
    // that the deployment is absent. Only the script-specific absence permits creation.
    if (allowMissing && response.status === 404 && body.success === false &&
        body.errors?.length === 1 && body.errors[0].code === WORKER_NOT_FOUND) return null;
    if (!response.ok || body.success !== true || (body.result == null && method === 'GET')) {
      const codes = (body.errors ?? []).map(error => Number(error.code)).join(',');
      throw new Error(`Cloudflare request failed (HTTP ${response.status}, codes ${codes || 'none'}).`);
    }
    return body.result;
  }
  const get = (path, allowMissing = false) => request(path, { allowMissing });
  const keys = `/accounts/${accountId}/calls/turn_keys`;
  return {
    turnKeys: () => get(keys),
    createTURNKey: name => request(keys, { method: 'POST', data: { name } }),
    deleteTURNKey: id => request(`${keys}/${encodeURIComponent(id)}`, { method: 'DELETE' }),
    putSecret: (name, text) => request(`${workerPath}/secrets`, { method: 'PUT', data: { name, type: 'secret_text', text } }),
    deleteSecret: name => request(`${workerPath}/secrets/${encodeURIComponent(name)}`, { method: 'DELETE' }),
    subdomain: async () => (await get(`${accountPath}/subdomain`)).subdomain,
    settings: () => get(`${workerPath}/settings`, true),
    route: () => get(`${workerPath}/subdomain`),
    zone: zoneId => get(`/zones/${zoneId}`),
    domains: hostname => get(`${accountPath}/domains?hostname=${encodeURIComponent(hostname)}`),
    customHostnames: (zoneId, hostname) => get(`/zones/${zoneId}/custom_hostnames?hostname=${encodeURIComponent(hostname)}`),
    workerRoutes: zoneId => get(`/zones/${zoneId}/workers/routes`),
  };
}
