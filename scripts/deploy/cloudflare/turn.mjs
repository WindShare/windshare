export const TURN_SECRET = 'TURN_KEY';
const MAX_RECOVERY_STEPS = 5;

// The access object records intent and owns publication. Actions alone holds the
// account token; neither clients nor the Worker need account-wide write authority.
export async function provisionTURN({ cloudflare, consoleClient, log = () => {} }) {
  let record;
  try {
    record = await consoleClient.call('turn/provision', { action: 'begin' });
    for (let step = 0; step < MAX_RECOVERY_STEPS; step++) {
      const state = record.provisioning;
      const fields = { serviceId: record.service_id, attempt: state.attempt, keyId: state.key_id };
      if (state.phase === 'published') {
        if (record.secret_key_id !== state.key_id) throw new Error('Published TURN key secret is missing; retained the service identity.');
        log('turn_provisioning_reused', fields);
        return { status: 'ready', serviceId: record.service_id, keyId: state.key_id };
      }
      if (record.secret_key_id && state.phase !== 'abandoning') {
        if (record.secret_key_id !== state.key_id) throw new Error('TURN secret does not match the deployment creation record.');
        record = await consoleClient.call('turn/provision', { action: 'publish', attempt: state.attempt });
        continue;
      }
      const keys = await cloudflare.turnKeys();
      const owned = keys.filter(key => key.name === record.key_name);
      if (owned.length > 1 || (state.key_id && owned.some(key => key.uid !== state.key_id))) {
        throw new Error('TURN creation ownership is ambiguous; no keys were changed.');
      }
      if (state.phase === 'created' || state.phase === 'abandoning' || owned.length) {
        if (state.phase === 'intent') {
          record = await consoleClient.call('turn/provision', { action: 'created', attempt: state.attempt, key_id: owned[0].uid });
          continue;
        }
        const key = keys.find(key => key.uid === state.key_id);
        if (key && key.name !== record.key_name) throw new Error('TURN key is not owned by this deployment.');
        // Freeze publication before deleting a confirmed unpublished key. A stale
        // process can no longer publish credentials backed by this key.
        if (state.phase !== 'abandoning') {
          record = await consoleClient.call('turn/provision', { action: 'abandon', attempt: state.attempt, key_id: state.key_id });
        }
        if (key) await cloudflare.deleteTURNKey(state.key_id);
        if (record.secret_key_id === state.key_id) await cloudflare.deleteSecret(TURN_SECRET);
        else if (record.secret_key_id) throw new Error('An unrelated TURN secret prevents recovery.');
        record = await consoleClient.call('turn/provision', { action: 'replace', attempt: state.attempt, key_id: state.key_id });
        log('turn_unpublished_key_replaced', fields);
        continue;
      }
      log('turn_creation_intent_saved', { ...fields, keyName: record.key_name });
      const created = await cloudflare.createTURNKey(record.key_name);
      if (!created.uid || !created.secret || created.name !== record.key_name) throw new Error('Cloudflare returned an invalid TURN key.');
      record = await consoleClient.call('turn/provision', { action: 'created', attempt: state.attempt, key_id: created.uid });
      await cloudflare.putSecret(TURN_SECRET, JSON.stringify({ key_id: created.uid, api_token: created.secret }));
      record = await consoleClient.call('turn/provision', { action: 'publish', attempt: state.attempt });
    }
    throw new Error('TURN provisioning requires another deployment run to finish recovery.');
  } catch (error) {
    log('turn_provisioning_incomplete', { serviceId: record?.service_id, reason: error.message });
    return { status: 'incomplete', serviceId: record?.service_id, reason: error.message };
  }
}

export function analyticsSecret(environment) {
  const token = environment.CLOUDFLARE_ANALYTICS_API_TOKEN;
  if (!token || token === environment.CLOUDFLARE_API_TOKEN) return {};
  return { TURN_ANALYTICS: JSON.stringify({ account_id: environment.CLOUDFLARE_ACCOUNT_ID, api_token: token }) };
}

export function turnConsoleClient({ origin, password, fetchImpl = fetch }) {
  let cookie;
  async function request(path, body) {
    const response = await fetchImpl(origin + '/api/' + path, {
      method: body === undefined ? 'GET' : 'POST',
      headers: { Origin: origin, ...(cookie ? { Cookie: cookie } : {}),
        ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(30_000), redirect: 'error',
    });
    if (!response.ok) throw new Error('TURN console request failed (HTTP ' + response.status + ').');
    if (path === 'login') cookie = response.headers.get('set-cookie')?.split(';')[0];
    return response.json();
  }
  return {
    async call(path, body) {
      if (!cookie) {
        await request('login', { password });
        if (!cookie) throw new Error('TURN console login did not return a session.');
      }
      return request(path, body);
    },
  };
}
