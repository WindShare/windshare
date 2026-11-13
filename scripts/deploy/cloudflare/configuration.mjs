export const FULL_DEPLOYMENT = 'full';
export const RELAY_ONLY_DEPLOYMENT = 'relay-only';
export const FRONTEND_DEPLOYMENT = 'frontend';
const FRONTEND_DIRECTORY = '../../../web/dist';
function validateMode(mode) {
  if (![FULL_DEPLOYMENT, RELAY_ONLY_DEPLOYMENT, FRONTEND_DEPLOYMENT].includes(mode)) throw new Error('Set CLOUDFLARE_DEPLOYMENT_MODE to full, relay-only or frontend.');
  return mode;
}
export function readDeploymentMode(env) { return validateMode(env.CLOUDFLARE_DEPLOYMENT_MODE || FULL_DEPLOYMENT); }
export function deploymentConfig(host, inputs, mode = FULL_DEPLOYMENT, state) {
  validateMode(mode);
  if (mode === FRONTEND_DEPLOYMENT) return frontendConfig(inputs);
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
export function frontendConfig(inputs, directory = FRONTEND_DIRECTORY) {
  // An assets deployment must never inherit relay migrations, bindings or its Wasm build.
  return {
    name: inputs.workerName, account_id: inputs.accountId,
    main: '../../../scripts/deploy/cloudflare/frontend-entry.mjs', compatibility_date: '2026-11-01',
    workers_dev: inputs.route.kind === 'workers-dev', preview_urls: false,
    routes: inputs.route.kind === 'custom-domain'
      ? [{ pattern: inputs.route.hostname, zone_id: inputs.route.zoneId, custom_domain: true },
        ...(inputs.route.saasHostname ? [{ pattern: `${inputs.route.saasHostname}/*`, zone_id: inputs.route.zoneId }] : [])] : [],
    // Static page traffic must not consume Worker invocations or their free request quota.
    // Only directory validation and installer redirects require the handler.
    assets: { directory, binding: 'ASSETS', not_found_handling: 'single-page-application',
      run_worker_first: ['/.well-known/windshare-network', '/downloads/*'] },
    observability: { enabled: true },
  };
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
