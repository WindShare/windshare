export const FULL_DEPLOYMENT = 'full';
export const RELAY_ONLY_DEPLOYMENT = 'relay-only';
const FRONTEND_DIRECTORY = '../../../web/dist';
const RELAY_ROUTES = ['/v2/*'];

function validateMode(mode) {
  if (mode !== FULL_DEPLOYMENT && mode !== RELAY_ONLY_DEPLOYMENT) {
    throw new Error('Set CLOUDFLARE_DEPLOYMENT_MODE to full or relay-only.');
  }
  return mode;
}

export function readDeploymentMode(env) {
  return validateMode(env.CLOUDFLARE_DEPLOYMENT_MODE || FULL_DEPLOYMENT);
}

export function deploymentConfig(host, inputs, mode = FULL_DEPLOYMENT) {
  validateMode(mode);
  const config = {
    ...host, name: inputs.workerName, account_id: inputs.accountId,
    workers_dev: inputs.route.kind === 'workers-dev', preview_urls: false,
    routes: inputs.route.kind === 'custom-domain'
      ? [{ pattern: inputs.route.hostname, zone_id: inputs.route.zoneId, custom_domain: true }]
      : [],
  };
  if (mode === FULL_DEPLOYMENT) {
    config.assets = {
      directory: FRONTEND_DIRECTORY,
      not_found_handling: 'single-page-application',
      run_worker_first: [...RELAY_ROUTES],
    };
  } else {
    delete config.assets;
  }
  return config;
}
