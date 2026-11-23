const SECRET = 'TURN_KEY';
const WARNING = 'Managed TURN provisioning was skipped. Enable Realtime TURN for this account and grant the API token Account / Cloudflare Calls / Edit, or set CLOUDFLARE_TURN_ENABLED=false.';

export async function provisionTURN({ client, workerName, deploymentId, enabled, log }) {
  const name = `windshare-${workerName}-${deploymentId.slice(0, 8)}`;
  try {
    const settings = await client.settings();
    const bound = settings.bindings.some(binding => binding.name === SECRET && binding.type === 'secret_text');
    if (enabled && bound) { log('turn_provisioning_reused'); return; }
    if (!enabled && bound) await client.deleteSecret(SECRET);
    const owned = (await client.turnKeys()).filter(key => key.name === name);
    // The secret is write-only. A key without its binding cannot be recovered.
    for (const key of owned) await client.deleteTURNKey(key.uid);
    if (!enabled) { log('turn_provisioning_skipped', { reason: 'disabled' }); return; }
    const key = await client.createTURNKey(name);
    if (!key?.uid || !key.key) throw new Error('Missing generated key material');
    await client.putSecret(SECRET, JSON.stringify({ key_id: key.uid, api_token: key.key }));
    log('turn_provisioning_ready');
  } catch (error) {
    // Only status and API error codes from the client are safe to publish.
    const diagnostic = /^Cloudflare request failed \(HTTP \d+, codes [0-9,none]+\)\.$/.test(error.message) ? ' ' + error.message : '';
    const warning = WARNING + diagnostic;
    log('turn_provisioning_skipped', { reason: warning });
    return warning;
  }
}
