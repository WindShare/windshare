import { randomUUID } from 'node:crypto';
import { readWorkerIdentity, STATE_BINDING } from './deployment.mjs';
import { prepareRoute, readFrontendRoute, sameRoute, verifyRoute } from './routing.mjs';

const FORMAT = 'windshare-cloudflare-frontend-v1';
export function readFrontendInputs(env) { return { ...readWorkerIdentity(env), route: readFrontendRoute(env) }; }
function retainedState(settings, inputs, endpoint, allowSaaSAttachment = false) {
  if (settings.bindings?.some(binding => binding.type === 'durable_object_namespace')) {
    throw new Error('Frontend deployment cannot replace a relay or another stateful Worker. Choose a separate Worker name.');
  }
  const markers = settings.bindings?.filter(binding => binding.name === STATE_BINDING && binding.type === 'plain_text') ?? [];
  let state;
  if (markers.length === 1) { try { state = JSON.parse(markers[0].text); } catch { /* Invalid ownership records cannot authorize replacement. */ } }
  // The owned frontend can go live before SaaS billing and certificate validation.
  // Later attachment must preserve its primary domain and cannot remove or rebind a link host.
  const attachesSaaS = allowSaaSAttachment && !state?.route?.saasHostname && inputs.route.saasHostname &&
    sameRoute(state?.route, { ...inputs.route, saasHostname: undefined });
  if (state?.format !== FORMAT || !state.deploymentId || state.accountId !== inputs.accountId ||
      state.workerName !== inputs.workerName || state.endpoint !== endpoint || (!sameRoute(state.route, inputs.route) && !attachesSaaS)) {
    throw new Error('Existing Worker is not this frontend deployment; restore its route or choose a separate Worker name.');
  }
  return state;
}
export async function deployFrontend({ inputs, cloudflare, deploy, log = () => {}, newId = randomUUID }) {
  const endpoint = await prepareRoute(inputs, cloudflare);
  if (inputs.route.saasHostname) await verifySaaS(inputs, cloudflare, false);
  const settings = await cloudflare.settings();
  const state = settings === null
    ? { format: FORMAT, deploymentId: newId(), ...inputs, endpoint }
    : { ...retainedState(settings, inputs, endpoint, true), route: inputs.route };
  const kind = settings === null ? 'create' : 'update';
  log('frontend_prepared', { kind, deploymentId: state.deploymentId, endpoint });
  await deploy({ kind, state });
  const confirmed = retainedState(await cloudflare.settings(), inputs, endpoint);
  if (JSON.stringify(confirmed) !== JSON.stringify(state)) throw new Error('Frontend ownership changed during upload.');
  await verifyRoute(inputs, cloudflare);
  if (inputs.route.saasHostname) await verifySaaS(inputs, cloudflare, true);
  log('frontend_verified', { deploymentId: state.deploymentId, endpoint });
  return { kind, state };
}
async function verifySaaS(inputs, cloudflare, deployed) {
  const { zoneId, saasHostname } = inputs.route;
  const hostnames = await cloudflare.customHostnames(zoneId, saasHostname);
  if (!Array.isArray(hostnames) || hostnames.length !== 1 || hostnames[0].hostname !== saasHostname ||
      hostnames[0].status !== 'active' || hostnames[0].ssl?.status !== 'active') {
    throw new Error('Complete Cloudflare for SaaS hostname and certificate validation before deploying the link host.');
  }
  const routes = (await cloudflare.workerRoutes(zoneId)).filter(route => route.pattern === `${saasHostname}/*`);
  if (routes.some(route => route.script !== inputs.workerName) || deployed && routes.length !== 1) {
    throw new Error('The SaaS hostname route must belong to this frontend Worker.');
  }
}
