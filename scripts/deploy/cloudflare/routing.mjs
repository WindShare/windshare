const DNS_LABEL = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
const ZONE_ID = /^[a-f0-9]{32}$/;

function requireValue(condition, message) {
  if (!condition) throw new Error(message);
}

export function readRoute(env) {
  const hostname = env.CLOUDFLARE_RELAY_DOMAIN;
  const zoneId = env.CLOUDFLARE_ZONE_ID;
  if (!hostname && !zoneId) return { kind: 'workers-dev' };
  requireValue(typeof hostname === 'string' && hostname.length <= 253 &&
    hostname.includes('.') && hostname.split('.').every(label => DNS_LABEL.test(label)),
  'Set CLOUDFLARE_RELAY_DOMAIN to a lowercase hostname without a scheme, port or path.');
  requireValue(ZONE_ID.test(zoneId ?? ''), 'Custom domains require CLOUDFLARE_ZONE_ID.');
  return { kind: 'custom-domain', hostname, zoneId };
}

export function endpointFor(inputs, subdomain) {
  if (inputs.route.kind === 'custom-domain') return `https://${inputs.route.hostname}`;
  requireValue(DNS_LABEL.test(subdomain ?? ''), 'Create your workers.dev subdomain in Cloudflare before deploying.');
  return `https://${inputs.workerName}.${subdomain}.workers.dev`;
}

export function sameRoute(left, right) {
  return left?.kind === right.kind && (right.kind === 'workers-dev' ||
    (left.hostname === right.hostname && left.zoneId === right.zoneId));
}

function ownsDomain(domain, inputs) {
  return domain.hostname === inputs.route.hostname && domain.zone_id === inputs.route.zoneId &&
    domain.service === inputs.workerName && domain.environment === 'production';
}

export async function prepareRoute(inputs, cloudflare) {
  if (inputs.route.kind === 'workers-dev') return endpointFor(inputs, await cloudflare.subdomain());
  const { hostname, zoneId } = inputs.route;
  const zone = await cloudflare.zone(zoneId);
  requireValue(zone.id === zoneId && zone.account?.id === inputs.accountId && zone.status === 'active' &&
    (hostname === zone.name || hostname.endsWith(`.${zone.name}`)),
  'Custom domain must belong to an active zone in the deployment account.');
  const domains = await cloudflare.domains(hostname);
  requireValue(Array.isArray(domains) && domains.length <= 1 && domains.every(domain => ownsDomain(domain, inputs)),
    'Custom domain is already attached to another Worker; refusing to replace it.');
  return endpointFor(inputs);
}

export async function verifyRoute(inputs, cloudflare) {
  const route = await cloudflare.route();
  requireValue(route.enabled === (inputs.route.kind === 'workers-dev') && route.previews_enabled === false,
    'The stable workers.dev route or preview URL settings did not match; rerun the deployment.');
  if (inputs.route.kind === 'custom-domain') {
    const domains = await cloudflare.domains(inputs.route.hostname);
    requireValue(Array.isArray(domains) && domains.length === 1 && ownsDomain(domains[0], inputs) &&
      domains[0].enabled === true && domains[0].previews_enabled === false,
      'Custom domain was not attached to this Worker; rerun the deployment.');
  }
}
