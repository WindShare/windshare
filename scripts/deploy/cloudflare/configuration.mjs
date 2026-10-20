export const FULL_DEPLOYMENT = 'full';
export const RELAY_ONLY_DEPLOYMENT = 'relay-only';
const FRONTEND_DIRECTORY = '../../../web/dist';
function validateMode(mode) {
  if (mode !== FULL_DEPLOYMENT && mode !== RELAY_ONLY_DEPLOYMENT) throw new Error('Set CLOUDFLARE_DEPLOYMENT_MODE to full or relay-only.');
  return mode;
}
export function readDeploymentMode(env) { return validateMode(env.CLOUDFLARE_DEPLOYMENT_MODE || FULL_DEPLOYMENT); }
export function deploymentConfig(host, inputs, mode = FULL_DEPLOYMENT, state) {
  validateMode(mode);
  const config = {
    ...host, main: 'entry.mjs', name: inputs.workerName, account_id: inputs.accountId,
    workers_dev: inputs.route.kind === 'workers-dev', preview_urls: false,
    routes: inputs.route.kind === 'custom-domain'
      ? [inputs.route.hostname, inputs.route.consoleHostname].map(pattern => ({ pattern, zone_id: inputs.route.zoneId, custom_domain: true })) : [],
  };
  if (state) config.vars = { ...host.vars, RELAY_URL: state.endpoint, CONSOLE_ORIGIN: state.consoleEndpoint,
    FRONTEND_URL: mode === FULL_DEPLOYMENT ? state.endpoint : '' };
  if (mode === FULL_DEPLOYMENT) config.assets = {
    directory: FRONTEND_DIRECTORY, binding: 'ASSETS', not_found_handling: 'single-page-application', run_worker_first: true,
  };
  else delete config.assets;
  return config;
}
export function consoleConfig(host, inputs, state) {
  return {
    name: inputs.workerName + '-console', account_id: inputs.accountId, main: 'console-entry.mjs',
    compatibility_date: host.compatibility_date, workers_dev: true, preview_urls: false,
    vars: { CONSOLE_ORIGIN: state.consoleEndpoint },
    durable_objects: { bindings: [{ name: 'ACCESS', class_name: 'AccessObject', script_name: inputs.workerName }] },
    observability: { enabled: true },
  };
}
