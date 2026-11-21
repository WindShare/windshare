import { ICEEndpointPool, MAXIMUM_CATALOG_ENDPOINTS, type ICEEndpoint } from './endpoints'
import type { NetworkNode, NetworkSnapshot } from '../../network/node-directory'

export const EXISTING_DEFAULT_STUN_SERVER = 'stun:stun.l.google.com:19302'

export const CLOUDFLARE_STUN_SERVER = 'stun:stun.cloudflare.com:3478'
export const TWILIO_STUN_SERVER = 'stun:global.stun.twilio.com:3478'

// Static priority tiers keep healthy active directory nodes ahead of canary
// and draining nodes, signed public STUN, and embedded fallbacks, while
// allowing runtime profile success to override static order.
export const DIRECTORY_NODE_STUN_PRIORITY = 4
export const DIRECTORY_CANARY_STUN_PRIORITY = 3
export const DIRECTORY_DRAINING_STUN_PRIORITY = 2
export const DIRECTORY_PUBLIC_STUN_PRIORITY = 1
export const FALLBACK_STUN_PRIORITY = 0

function directoryNodeSTUNPriority(status: NetworkNode['status']): number {
 switch (status) {
  case 'active': return DIRECTORY_NODE_STUN_PRIORITY
  case 'canary': return DIRECTORY_CANARY_STUN_PRIORITY
  case 'draining': return DIRECTORY_DRAINING_STUN_PRIORITY
 }
}

// Provider-published fallbacks make no claim about mainland reachability.
export function defaultICEEndpointPool(): ICEEndpointPool {
 return new ICEEndpointPool([
  { id: 'shipped-google-stun', url: EXISTING_DEFAULT_STUN_SERVER, provider: 'google', failureDomain: 'google-stun' },
  { id: 'public-cloudflare-stun', url: CLOUDFLARE_STUN_SERVER, provider: 'cloudflare', failureDomain: 'cloudflare-stun' },
  { id: 'public-twilio-stun', url: TWILIO_STUN_SERVER, provider: 'twilio', failureDomain: 'twilio-stun' },
 ].map(endpoint => ({ ...endpoint, region: '', family: 'any', trust: 'reviewed', priority: FALLBACK_STUN_PRIORITY, enabled: true })))
}

// Directory entries are deduplicated ahead of fallbacks so signed metadata and
// priority win when a URL also appears in the embedded fallback set.
export function directoryICEEndpointPool(snapshot: NetworkSnapshot | undefined): ICEEndpointPool {
 const entries: ICEEndpoint[] = [], seen = new Set<string>()
 const add = (entry: ICEEndpoint) => {
  if (seen.has(entry.url) || entries.length >= MAXIMUM_CATALOG_ENDPOINTS) return
  seen.add(entry.url)
  entries.push(entry)
 }
 for (const node of snapshot?.nodes ?? []) if (node.roles.includes('stun')) {
  const priority = directoryNodeSTUNPriority(node.status)
  for (const url of node.stun) add({
   id: `directory:${url}`, url, region: node.region, provider: node.provider,
   failureDomain: `${node.operator}:${node.provider}:${node.region}`,
   family: 'any', trust: 'reviewed', priority, enabled: true,
  })
 }
 for (const endpoint of snapshot?.publicSTUN ?? []) add({
  id: `directory:${endpoint.url}`, url: endpoint.url, region: endpoint.region, provider: endpoint.provider,
  failureDomain: endpoint.failureDomain, family: 'any', trust: 'reviewed',
  priority: DIRECTORY_PUBLIC_STUN_PRIORITY, enabled: true,
 })
 for (const fallback of defaultICEEndpointPool().endpoints) add(fallback)
 return new ICEEndpointPool(entries)
}
