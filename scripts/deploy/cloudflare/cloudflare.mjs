const API_BASE = 'https://api.cloudflare.com/client/v4';
const REQUEST_TIMEOUT_MS = 30_000;
const WORKER_NOT_FOUND = 10007;

export function cloudflareClient({ accountId, workerName, apiToken, fetchImpl = fetch }) {
  if (!apiToken?.trim()) throw new Error('Set the CLOUDFLARE_API_TOKEN repository secret.');
  const accountPath = `/accounts/${accountId}/workers`;
  const workerPath = `${accountPath}/scripts/${workerName}`;
  async function get(path, allowMissing = false, method = 'GET', value) {
    const response = await fetchImpl(API_BASE + path, {
      method, body: value === undefined ? undefined : JSON.stringify(value),
      headers: { Authorization: `Bearer ${apiToken}`, ...(value === undefined ? {} : { 'Content-Type': 'application/json' }) },
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
    if (!response.ok || body.success !== true || (body.result == null && method !== 'DELETE')) {
      const codes = (body.errors ?? []).map(error => Number(error.code)).join(',');
      throw new Error(`Cloudflare request failed (HTTP ${response.status}, codes ${codes || 'none'}).`);
    }
    return body.result;
  }
  return {
    turnKeys: () => get(`/accounts/${accountId}/calls/turn_keys`),
    createTURNKey: name => get(`/accounts/${accountId}/calls/turn_keys`, false, 'POST', { name }),
    deleteTURNKey: id => get(`/accounts/${accountId}/calls/turn_keys/${encodeURIComponent(id)}`, false, 'DELETE'),
    putSecret: (name, text) => get(`${workerPath}/secrets`, false, 'PUT', { name, text, type: 'secret_text' }),
    deleteSecret: name => get(`${workerPath}/secrets/${encodeURIComponent(name)}`, false, 'DELETE'),
    subdomain: async () => (await get(`${accountPath}/subdomain`)).subdomain,
    settings: () => get(`${workerPath}/settings`, true),
    route: () => get(`${workerPath}/subdomain`),
    zone: zoneId => get(`/zones/${zoneId}`),
    domains: hostname => get(`${accountPath}/domains?hostname=${encodeURIComponent(hostname)}`),
  };
}
