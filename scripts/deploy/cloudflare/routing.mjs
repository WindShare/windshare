const DNS_LABEL = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
const ZONE_ID = /^[a-f0-9]{32}$/;
function requireValue(condition, message) { if (!condition) throw new Error(message); }
function hostname(value) { return typeof value === 'string' && value.length <= 253 && value.includes('.') && value.split('.').every(label => DNS_LABEL.test(label)); }
export function readRoute(env) {
  const name = env.CLOUDFLARE_RELAY_DOMAIN;
  const zoneId = env.CLOUDFLARE_ZONE_ID;
  const consoleHostname = env.CLOUDFLARE_CONSOLE_DOMAIN || (name ? `console.${name}` : undefined);
  if (!name && !zoneId && !consoleHostname) return { kind: 'workers-dev' };
  requireValue(hostname(name), 'Set CLOUDFLARE_RELAY_DOMAIN to a lowercase hostname without scheme, port or path.');
  requireValue(hostname(consoleHostname) && consoleHostname !== name, 'Set CLOUDFLARE_CONSOLE_DOMAIN to a separate console hostname.');
  requireValue(ZONE_ID.test(zoneId ?? ''), 'Custom domains require CLOUDFLARE_ZONE_ID.');
  return { kind: 'custom-domain', hostname: name, consoleHostname, zoneId };
}
export function endpointFor(inputs, subdomain) {
  if (inputs.route.kind === 'custom-domain') return `https://${inputs.route.hostname}`;
  requireValue(DNS_LABEL.test(subdomain ?? ''), 'Create your workers.dev subdomain in Cloudflare before deploying.');
  return `https://${inputs.workerName}.${subdomain}.workers.dev`;
}
export function sameRoute(left, right) {
  return left?.kind === right.kind && (right.kind === 'workers-dev' ||
    (left.hostname === right.hostname && left.consoleHostname === right.consoleHostname && left.zoneId === right.zoneId));
}
function ownsDomain(domain, inputs, name) {
  return domain.hostname === name && domain.zone_id === inputs.route.zoneId &&
    domain.service === inputs.workerName && domain.environment === 'production';
}
export async function prepareRoute(inputs, cloudflare) {
  if (inputs.route.kind === 'workers-dev') return endpointFor(inputs, await cloudflare.subdomain());
  const zone = await cloudflare.zone(inputs.route.zoneId);
  requireValue(zone.id === inputs.route.zoneId && zone.account?.id === inputs.accountId && zone.status === 'active',
    'Custom domains must belong to an active zone in the deployment account.');
  for (const name of [inputs.route.hostname, inputs.route.consoleHostname]) {
    requireValue(hostname(name) && (name === zone.name || name.endsWith(`.${zone.name}`)), 'Custom domain is outside the selected zone.');
    const domains = await cloudflare.domains(name);
    requireValue(Array.isArray(domains) && domains.length <= 1 && domains.every(domain => ownsDomain(domain, inputs, name)),
      'Custom domain is already attached to another Worker; refusing to replace it.');
  }
  return endpointFor(inputs);
}
export async function verifyRoute(inputs, cloudflare) {
  const route = await cloudflare.route();
  requireValue(route.enabled === (inputs.route.kind === 'workers-dev') && route.previews_enabled === false,
    'The stable workers.dev route or preview URL settings did not match; rerun the deployment.');
  if (inputs.route.kind === 'custom-domain') {
    for (const name of [inputs.route.hostname, inputs.route.consoleHostname]) {
      const domains = await cloudflare.domains(name);
      requireValue(Array.isArray(domains) && domains.length === 1 && ownsDomain(domains[0], inputs, name) &&
        domains[0].enabled === true && domains[0].previews_enabled === false,
      'Custom domain was not attached to this Worker; rerun the deployment.');
    }
  }
}
