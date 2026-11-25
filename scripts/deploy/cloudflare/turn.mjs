import { CloudflareRequestError } from './cloudflare.mjs';

const SECRET = 'TURN_KEY';
const TURN_KEY_PREFIX = 'windshare-';
const TURN_KEY_ID_LENGTH = 8;
const PROVISION_WARNING = 'Managed TURN provisioning was skipped. Enable Realtime TURN for this account and grant the API token Account / Cloudflare Calls / Edit, or set CLOUDFLARE_TURN_ENABLED=false.';
const CLEANUP_WARNING = 'Managed TURN cleanup is incomplete. Check the Worker secret binding and the deployment-owned TURN key.';

export function turnKeyPrefix(workerName) {
  return `${TURN_KEY_PREFIX}${workerName}-`;
}

export function turnKeyName(workerName, deploymentId) {
  return `${turnKeyPrefix(workerName)}${deploymentId.slice(0, TURN_KEY_ID_LENGTH)}`;
}

export async function provisionTURN({ client, workerName, deploymentId, enabled, log }) {
  const name = turnKeyName(workerName, deploymentId);
  try {
    const settings = await client.settings();
    if (!Array.isArray(settings?.bindings)) {
      const warning = 'Managed TURN provisioning was skipped because the Worker bindings could not be verified.';
      log('turn_provisioning_skipped', { reason: warning });
      return warning;
    }
    const bound = settings.bindings.some(binding => binding.name === SECRET && binding.type === 'secret_text');
    if (enabled && bound) {
      log('turn_provisioning_reused');
      return;
    }
    if (!enabled && bound) await client.deleteSecret(SECRET);
    const keys = await client.turnKeys().catch(error => {
      // An opted-out Worker without a secret needs no Calls access to operate.
      if (!enabled && !bound && error instanceof CloudflareRequestError && error.status === 403) return [];
      throw error;
    });
    // The secret is write-only. Once an orphan is known, any deletion failure needs attention.
    for (const key of keys.filter(key => key.name === name)) await client.deleteTURNKey(key.uid);
    if (enabled) {
      const key = await client.createTURNKey(name);
      if (!key?.uid || !key.key) throw new Error('Missing generated key material');
      await client.putSecret(SECRET, JSON.stringify({ key_id: key.uid, api_token: key.key }));
      log('turn_provisioning_ready');
      return;
    }
  } catch (error) {
    const diagnostic = error instanceof CloudflareRequestError ? ' ' + error.message : '';
    const warning = (enabled ? PROVISION_WARNING : CLEANUP_WARNING) + diagnostic;
    log('turn_provisioning_skipped', { reason: warning });
    return warning;
  }
  log('turn_provisioning_skipped', { reason: 'disabled' });
}
