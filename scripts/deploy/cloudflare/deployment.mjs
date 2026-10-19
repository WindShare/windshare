import { randomBytes, randomUUID } from 'node:crypto';
import { prepareRoute, readRoute, sameRoute, verifyRoute } from './routing.mjs';

export const STATE_BINDING = 'WINDSHARE_DEPLOYMENT';
export const SECRET_BINDING = 'REGISTRATION_SECRET';
export const SHARE_BINDING = 'SHARES';
export const SHARE_CLASS = 'ShareRelayObject';
const STATE_FORMAT = 'windshare-cloudflare-deployment-v1';
const TOKEN_BYTES = 32;
const MAX_STATE_BYTES = 5 * 1024;
const DNS_LABEL = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
const ACCOUNT_ID = /^[a-f0-9]{32}$/;
const RECIPIENT = /^age1[a-z0-9]{58}$/;

export function readInputs(env) {
  const inputs = {
    accountId: env.CLOUDFLARE_ACCOUNT_ID,
    workerName: env.CLOUDFLARE_WORKER_NAME,
    recipient: env.WINDSHARE_CONFIG_RECIPIENT,
    route: readRoute(env),
  };
  requireValue(ACCOUNT_ID.test(inputs.accountId ?? ''), 'Set CLOUDFLARE_ACCOUNT_ID to your 32-character account ID.');
  requireValue(DNS_LABEL.test(inputs.workerName ?? ''), 'Set CLOUDFLARE_WORKER_NAME to a DNS label (1–63 lowercase characters).');
  requireValue(RECIPIENT.test(inputs.recipient ?? ''), 'Set WINDSHARE_CONFIG_RECIPIENT to the public age1 key from age-keygen.');
  return inputs;
}

function requireValue(condition, message) {
  if (!condition) throw new Error(message);
}

function binding(settings, name, type) {
  const matches = Array.isArray(settings?.bindings) ? settings.bindings.filter(item => item.name === name) : [];
  requireValue(matches.length === 1 && matches[0].type === type, `Existing Worker is missing its ${name} ${type} binding; refusing to replace deployment state.`);
  return matches[0];
}

export function readDeployment(settings, inputs, endpoint) {
  const text = binding(settings, STATE_BINDING, 'plain_text').text;
  requireValue(typeof text === 'string' && Buffer.byteLength(text) <= MAX_STATE_BYTES, 'Invalid deployment record.');
  let state;
  try { state = JSON.parse(text); } catch { throw new Error('Invalid deployment record.'); }
  requireValue(state?.format === STATE_FORMAT && typeof state.deploymentId === 'string' && state.deploymentId.length > 0,
    'Unsupported deployment record; refusing to replace the existing Worker.');
  requireValue(state.accountId === inputs.accountId && state.workerName === inputs.workerName &&
    state.endpoint === endpoint && sameRoute(state.route, inputs.route),
  'Deployment address changed; restore the original account, Worker name and route configuration.');
  requireValue(state.recipient === inputs.recipient,
    'Configuration recipient changed; restore the original WINDSHARE_CONFIG_RECIPIENT. Updates never rotate credentials.');
  requireValue(typeof state.encryptedConfig === 'string' &&
    state.encryptedConfig.startsWith('-----BEGIN AGE ENCRYPTED FILE-----\n') &&
    state.encryptedConfig.trimEnd().endsWith('-----END AGE ENCRYPTED FILE-----'),
  'Missing encrypted configuration; refusing to regenerate credentials.');
  binding(settings, SECRET_BINDING, 'secret_text');
  const shares = binding(settings, SHARE_BINDING, 'durable_object_namespace');
  requireValue(shares.class_name === SHARE_CLASS && /^[a-f0-9]{32}$/.test(shares.namespace_id ?? '') &&
    (!shares.script_name || shares.script_name === inputs.workerName) && !shares.environment,
  'Existing share storage does not belong to this Worker; refusing to replace it.');
  return { state, namespaceId: shares.namespace_id };
}

// Refuse destructive source configuration before Wrangler can apply migrations.
// Post-upload namespace comparison diagnoses drift but cannot undo deleted SQLite.
export function validateHostConfig(config) {
  requireValue(config.workers_dev === true && config.preview_urls === false &&
    !config.route && !(config.routes?.length),
  'Source routing must remain generic; repository Variables select the deployment route.');
  const shares = config.durable_objects?.bindings?.filter(item => item.name === SHARE_BINDING) ?? [];
  requireValue(shares.length === 1 && shares[0].class_name === SHARE_CLASS &&
    !shares[0].script_name && !shares[0].environment,
  'Source configuration must retain the local SHARES / ShareRelayObject binding.');
  for (const name of [STATE_BINDING, SECRET_BINDING]) {
    requireValue(!Object.hasOwn(config.vars ?? {}, name), `Source configuration cannot supply ${name}.`);
  }
  let creations = 0;
  for (const migration of config.migrations ?? []) {
    if (migration.new_sqlite_classes?.includes(SHARE_CLASS)) creations++;
    requireValue(!migration.deleted_classes?.includes(SHARE_CLASS) &&
      !migration.new_classes?.includes(SHARE_CLASS) &&
      !migration.renamed_classes?.some(item => item.from === SHARE_CLASS || item.to === SHARE_CLASS) &&
      !migration.transferred_classes?.some(item => item.from === SHARE_CLASS || item.to === SHARE_CLASS),
    'Automatic updates cannot delete, rename or transfer share storage.');
  }
  requireValue(creations === 1, 'Keep the original SQLite share-class creation migration.');
}

// Secrets and the recoverable encrypted copy enter the same Worker version.
// A lost deployment response can then be retried without losing or rotating the key.
export async function deployRelay({
  inputs, cloudflare, encrypt, deploy, log = () => {},
  newToken = () => randomBytes(TOKEN_BYTES).toString('base64url'), newId = randomUUID,
}) {
  const endpoint = await prepareRoute(inputs, cloudflare);
  const existing = await cloudflare.settings();
  let plan;
  if (existing === null) {
    const credential = newToken();
    const encryptedConfig = await encrypt(JSON.stringify({
      relays: [{ url: endpoint, registration_token: credential }],
    }) + '\n', inputs.recipient);
    const state = { format: STATE_FORMAT, deploymentId: newId(), ...inputs, endpoint, encryptedConfig };
    requireValue(Buffer.byteLength(JSON.stringify(state)) <= MAX_STATE_BYTES, 'Encrypted deployment record exceeds the Worker variable limit.');
    plan = { kind: 'create', state, credential };
  } else {
    plan = { kind: 'update', ...readDeployment(existing, inputs, endpoint) };
  }
  log('deployment_prepared', { kind: plan.kind, deploymentId: plan.state.deploymentId, endpoint, namespaceId: plan.namespaceId });
  await deploy(plan);
  const confirmed = readDeployment(await cloudflare.settings(), inputs, endpoint);
  requireValue(JSON.stringify(confirmed.state) === JSON.stringify(plan.state),
    'Deployment record did not match after upload; rerun the workflow to inspect the retained state.');
  if (plan.kind === 'update') {
    requireValue(confirmed.namespaceId === plan.namespaceId, 'Share namespace changed during deployment.');
  }
  await verifyRoute(inputs, cloudflare);
  log('deployment_verified', { kind: plan.kind, deploymentId: confirmed.state.deploymentId, endpoint, namespaceId: confirmed.namespaceId });
  return { ...confirmed, kind: plan.kind };
}
