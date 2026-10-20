import { randomUUID } from 'node:crypto';
import { prepareRoute, readRoute, sameRoute, verifyRoute } from './routing.mjs';

export const STATE_BINDING = 'WINDSHARE_DEPLOYMENT';
export const SECRET_BINDING = 'OWNER_PASSWORD';
export const SHARE_BINDING = 'SHARES';
export const SHARE_CLASS = 'ShareRelayObject';
export const ACCESS_BINDING = 'ACCESS';
export const ACCESS_CLASS = 'AccessObject';
const STATE_FORMAT = 'windshare-cloudflare-deployment-v2';
const MAX_STATE_BYTES = 5 * 1024;
const WORKER_NAME = /^[a-z0-9](?:[a-z0-9-]{0,53}[a-z0-9])?$/;
const ACCOUNT_ID = /^[a-f0-9]{32}$/;

export function readInputs(env) {
  const inputs = {
    accountId: env.CLOUDFLARE_ACCOUNT_ID, workerName: env.CLOUDFLARE_WORKER_NAME,
    ownerPassword: env.WINDSHARE_OWNER_PASSWORD, route: readRoute(env),
  };
  requireValue(ACCOUNT_ID.test(inputs.accountId ?? ''), 'Set CLOUDFLARE_ACCOUNT_ID to your 32-character account ID.');
  requireValue(WORKER_NAME.test(inputs.workerName ?? ''), 'Set CLOUDFLARE_WORKER_NAME to a DNS label of at most 55 characters.');
  requireValue(typeof inputs.ownerPassword === 'string' && inputs.ownerPassword.length >= 16,
    'Set WINDSHARE_OWNER_PASSWORD in GitHub Secrets to a password of at least 16 characters.');
  return inputs;
}
function requireValue(condition, message) { if (!condition) throw new Error(message); }
function binding(settings, name, type) {
  const matches = settings?.bindings?.filter(item => item.name === name) ?? [];
  requireValue(matches.length === 1 && matches[0].type === type, `Existing Worker is missing its ${name} ${type} binding.`);
  return matches[0];
}
function stateFrom(settings) {
  const text = binding(settings, STATE_BINDING, 'plain_text').text;
  requireValue(typeof text === 'string' && Buffer.byteLength(text) <= MAX_STATE_BYTES, 'Invalid deployment record.');
  let state;
  try { state = JSON.parse(text); } catch { throw new Error('Invalid deployment record.'); }
  requireValue(state?.format === STATE_FORMAT && typeof state.deploymentId === 'string' && state.deploymentId.length > 0,
    'Unsupported deployment record; refusing to replace the existing Worker.');
  return state;
}
export function readDeployment(settings, inputs, endpoint) {
  const state = stateFrom(settings);
  requireValue(state.accountId === inputs.accountId && state.workerName === inputs.workerName &&
    state.endpoint === endpoint && sameRoute(state.route, inputs.route),
  'Deployment address changed; restore the original account, Worker name and route configuration.');
  binding(settings, SECRET_BINDING, 'secret_text');
  const namespace = (name, className) => {
    const item = binding(settings, name, 'durable_object_namespace');
    requireValue(item.class_name === className && /^[a-f0-9]{32}$/.test(item.namespace_id ?? '') &&
      (!item.script_name || item.script_name === inputs.workerName) && !item.environment,
    `Existing ${name} storage does not belong to this Worker.`);
    return item.namespace_id;
  };
  return { state, namespaceId: namespace(SHARE_BINDING, SHARE_CLASS), accessNamespaceId: namespace(ACCESS_BINDING, ACCESS_CLASS) };
}
export function validateHostConfig(config) {
  requireValue(config.workers_dev === true && config.preview_urls === false && !config.route && !(config.routes?.length),
    'Source routing must remain generic; repository Secrets select the deployment route.');
  for (const [name, className] of [[SHARE_BINDING, SHARE_CLASS], [ACCESS_BINDING, ACCESS_CLASS]]) {
    const items = config.durable_objects?.bindings?.filter(item => item.name === name) ?? [];
    requireValue(items.length === 1 && items[0].class_name === className && !items[0].script_name && !items[0].environment,
      `Source configuration must retain the local ${name} / ${className} binding.`);
    let creations = 0;
    for (const migration of config.migrations ?? []) {
      if (migration.new_sqlite_classes?.includes(className)) creations++;
      requireValue(!migration.deleted_classes?.includes(className) && !migration.new_classes?.includes(className) &&
        !migration.renamed_classes?.some(item => item.from === className || item.to === className) &&
        !migration.transferred_classes?.some(item => item.from === className || item.to === className),
      'Automatic updates cannot delete, rename or transfer access or share storage.');
    }
    requireValue(creations === 1, `Keep the original SQLite ${className} creation migration.`);
  }
  for (const name of [STATE_BINDING, SECRET_BINDING]) {
    requireValue(!Object.hasOwn(config.vars ?? {}, name), `Source configuration cannot supply ${name}.`);
  }
}
function verifyConsole(settings, state, accessNamespaceId) {
  requireValue(JSON.stringify(stateFrom(settings)) === JSON.stringify(state), 'Companion console belongs to another deployment.');
  const access = binding(settings, ACCESS_BINDING, 'durable_object_namespace');
  requireValue(access.class_name === ACCESS_CLASS && access.script_name === state.workerName &&
    (!accessNamespaceId || access.namespace_id === accessNamespaceId), 'Companion console must use the authoritative access namespace.');
}
export async function deployRelay({ inputs, cloudflare, consoleCloudflare, deploy, deployConsole, log = () => {}, newId = randomUUID }) {
  const endpoint = await prepareRoute(inputs, cloudflare);
  const consoleEndpoint = inputs.route.kind === 'custom-domain'
    ? `https://${inputs.route.consoleHostname}`
    : endpoint.replace(`https://${inputs.workerName}.`, `https://${inputs.workerName}-console.`);
  const existing = await cloudflare.settings();
  const retained = existing === null ? null : readDeployment(existing, inputs, endpoint);
  const state = retained?.state ?? { format: STATE_FORMAT, deploymentId: newId(), accountId: inputs.accountId,
    workerName: inputs.workerName, route: inputs.route, endpoint, consoleEndpoint };
  requireValue(state.consoleEndpoint === consoleEndpoint, 'Console address changed; restore its original route.');
  const plan = retained ? { kind: 'update', ...retained } : { kind: 'create', state };
  if (inputs.route.kind === 'workers-dev') {
    requireValue(consoleCloudflare && deployConsole, 'A workers.dev deployment requires its companion console.');
    const existingConsole = await consoleCloudflare.settings();
    if (existingConsole !== null) {
      requireValue(retained !== null, 'Companion console name is already in use.');
      verifyConsole(existingConsole, state, retained.accessNamespaceId);
    }
  }
  log('deployment_prepared', { kind: plan.kind, deploymentId: state.deploymentId, endpoint, consoleEndpoint });
  await deploy(plan);
  const confirmed = readDeployment(await cloudflare.settings(), inputs, endpoint);
  requireValue(JSON.stringify(confirmed.state) === JSON.stringify(state), 'Deployment record changed during upload.');
  if (retained) requireValue(confirmed.namespaceId === retained.namespaceId && confirmed.accessNamespaceId === retained.accessNamespaceId,
    'Access or share namespace changed during deployment.');
  await verifyRoute(inputs, cloudflare);
  if (inputs.route.kind === 'workers-dev') {
    await deployConsole(plan);
    verifyConsole(await consoleCloudflare.settings(), state, confirmed.accessNamespaceId);
    const route = await consoleCloudflare.route();
    requireValue(route.enabled === true && route.previews_enabled === false, 'Companion console route did not match.');
  }
  log('deployment_verified', { deploymentId: state.deploymentId, endpoint, consoleEndpoint,
    namespaceId: confirmed.namespaceId, accessNamespaceId: confirmed.accessNamespaceId });
  return { ...confirmed, kind: plan.kind };
}
